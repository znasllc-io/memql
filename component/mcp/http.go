package mcp

// http.go is the MCP Streamable HTTP transport head -- the remote path that
// lets Claude Code (`claude mcp add --transport http <url>`) and other MCP
// hosts reach a hosted memQL deployment over the network, as opposed to the
// local-subprocess stdio path in stdio.go (memql#1550).
//
// It implements the spec's single-endpoint Streamable HTTP binding over the
// transport-agnostic Server:
//   - POST /mcp        carries one JSON-RPC message. A request gets an
//                      application/json reply; a notification / client->server
//                      response gets 202 Accepted with no body.
//   - GET  /mcp        opens an SSE channel for server->client notifications
//                      (resource-subscription pushes); 405 when the writer
//                      can't stream.
//   - DELETE /mcp      ends the session.
//   - Mcp-Session-Id   assigned on `initialize`, echoed on the response, and
//                      required on every subsequent request. Each id maps to
//                      its own *Server (one Server == one MCP session), reaped
//                      after an idle TTL.
//
// AUTH IS MANDATORY. The endpoint can author DSL and run mutations /
// automations, so the handler is wrapped with the identity bearer-JWT
// verifier middleware: an unauthenticated or invalid request is rejected with
// 401 before it ever reaches a Server. The acting user/role for a session are
// derived from the verified token claims (unless pinned by env). A nil
// verifier is refused at construction -- the remote endpoint must never be
// reachable without identity.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/identity/verifier"
	"github.com/znasllc-io/memql/core/common"
)

const (
	// DefaultHTTPAddr is the bind address the Streamable HTTP head uses when
	// MEMQL_MCP_HTTP_ADDR is unset.
	DefaultHTTPAddr = ":8090"

	// MCPPath is the single Streamable HTTP endpoint path.
	MCPPath = "/mcp"

	// ProtectedResourcePath is the RFC 9728 OAuth Protected Resource Metadata
	// well-known path. Served UNAUTHENTICATED so an MCP custom connector can
	// discover this resource's identifier + which authorization server(s) issue
	// the bearer tokens it accepts, before it has any token at all.
	ProtectedResourcePath = "/.well-known/oauth-protected-resource"

	// sessionHeader is the MCP session-id header (spec: Mcp-Session-Id).
	sessionHeader = "Mcp-Session-Id"

	// DefaultSessionIdleTTL is how long a session may sit idle before the
	// janitor reaps it (tearing down its subscriptions).
	DefaultSessionIdleTTL = 30 * time.Minute

	// maxRequestBodyBytes caps an inbound POST body. JSON-RPC control messages
	// are tiny; this bounds memory against a hostile/oversized request.
	maxRequestBodyBytes = 8 << 20 // 8 MiB

	sseKeepaliveInterval = 25 * time.Second
)

// HTTPConfig configures the Streamable HTTP transport.
type HTTPConfig struct {
	// Addr is the bind address (e.g. ":8090"). Empty -> DefaultHTTPAddr.
	Addr string

	Logger *slog.Logger

	// Verifier gates the endpoint with identity bearer-JWT auth. REQUIRED:
	// NewHTTPDependency refuses to build an unauthenticated remote endpoint
	// (default-deny). The same per-node verifier the rest of the HTTP surface
	// uses (app.identityVerifier).
	Verifier *verifier.Verifier

	// BaseConfig carries the deployment-wide knobs: Tier (MEMQL_MCP_MODE) and
	// optional ActingRole / ActingUser pins (MEMQL_MCP_ROLE / MEMQL_MCP_USER).
	// When a pin is empty the per-session value is derived from the caller's
	// verified token claims.
	BaseConfig Config

	// NewServer builds a fresh per-session Server for a resolved Config.
	// REQUIRED.
	NewServer func(Config) *Server

	// IdleTTL overrides DefaultSessionIdleTTL (tests / tuning).
	IdleTTL time.Duration

	// PublicURL is this resource's public base URL (e.g.
	// https://mcp.staging.copresent.ai) -- the `resource` identifier advertised
	// in the RFC 9728 Protected Resource Metadata document and in the
	// WWW-Authenticate `resource_metadata` hint on a 401. Empty -> the resource
	// is derived per-request from the inbound scheme+Host and no
	// resource_metadata hint is emitted.
	PublicURL string

	// AuthServerURL is the public identity issuer (e.g.
	// https://identity.staging.copresent.ai) advertised as the
	// authorization_servers entry in the Protected Resource Metadata. Empty ->
	// the authorization_servers field is omitted.
	AuthServerURL string

	// authMiddleware overrides the verifier-built auth wrapper. Unexported on
	// purpose: production callers (app.transportMCP) only ever set Verifier;
	// the in-package tests inject a stub so the unauth-rejection / health-bypass
	// wiring can be exercised without a live JWKS.
	authMiddleware func(http.Handler) http.Handler
}

// NewHTTPDependency builds the Streamable HTTP transport as a
// common.Dependency so the MCP head joins the standard service lifecycle.
//
// It is default-deny: a nil Verifier (and no test authMiddleware override) is
// an error -- the remote endpoint must not exist without identity auth.
func NewHTTPDependency(cfg HTTPConfig) (common.Dependency, error) {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.NewServer == nil {
		return nil, errors.New("mcp http: NewServer factory is required")
	}
	publicURL := strings.TrimRight(strings.TrimSpace(cfg.PublicURL), "/")
	authServerURL := strings.TrimSpace(cfg.AuthServerURL)

	authMW := cfg.authMiddleware
	if authMW == nil {
		if cfg.Verifier == nil {
			return nil, errors.New("mcp http: identity verifier is required (the remote endpoint must not be reachable without auth)")
		}
		authMW = verifier.HTTPMiddleware(cfg.Verifier, verifier.MiddlewareOptions{
			Logger: logger,
			// The RFC 9728 Protected Resource Metadata document MUST be reachable
			// without a bearer token (a client fetches it precisely because it has
			// no token yet). Mark it public the same way the verifier treats
			// /healthz + /readyz.
			PublicPaths:         []string{ProtectedResourcePath},
			UnauthorizedHandler: unauthorizedHandler(publicURL),
		})
	}

	addr := strings.TrimSpace(cfg.Addr)
	if addr == "" {
		addr = DefaultHTTPAddr
	}
	idleTTL := cfg.IdleTTL
	if idleTTL <= 0 {
		idleTTL = DefaultSessionIdleTTL
	}

	head := &httpHead{
		logger:        logger,
		base:          cfg.BaseConfig,
		newSrv:        cfg.NewServer,
		idleTTL:       idleTTL,
		sessions:      make(map[string]*httpSession),
		publicURL:     publicURL,
		authServerURL: authServerURL,
	}

	mux := http.NewServeMux()
	mux.Handle(MCPPath, head)
	// RFC 9728 Protected Resource Metadata, served unauthenticated (the verifier
	// PublicPaths entry above bypasses auth for this path).
	mux.HandleFunc(ProtectedResourcePath, head.handleProtectedResource)
	// Unauthenticated health endpoints for k8s probes targeting this port.
	// verifier.HTTPMiddleware treats /healthz + /readyz as public, and we add
	// /livez here too.
	mux.HandleFunc("/healthz", healthOK)
	mux.HandleFunc("/readyz", healthOK)
	mux.HandleFunc("/livez", healthOK)

	handler := authMW(mux)

	srv := &http.Server{
		Addr:    addr,
		Handler: handler,
		// No WriteTimeout: the GET SSE channel is long-lived. ReadHeaderTimeout
		// bounds slow-header (slowloris) attacks; IdleTimeout reaps idle keep-alive
		// conns.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	return &httpDependency{
		srv:     srv,
		head:    head,
		logger:  logger,
		addr:    addr,
		readyCh: make(chan struct{}),
		doneCh:  make(chan struct{}),
	}, nil
}

func healthOK(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// ---------------------------------------------------------------------------
// httpHead: the per-endpoint request dispatcher + session registry.
// ---------------------------------------------------------------------------

type httpHead struct {
	logger  *slog.Logger
	base    Config
	newSrv  func(Config) *Server
	idleTTL time.Duration

	// publicURL is this resource's public base (no trailing slash), used as the
	// RFC 9728 `resource` identifier. Empty -> derived per-request.
	publicURL string
	// authServerURL is the public identity issuer advertised in
	// authorization_servers. Empty -> omitted.
	authServerURL string

	mu       sync.Mutex
	sessions map[string]*httpSession
}

// protectedResourceMetadata is the RFC 9728 OAuth Protected Resource Metadata
// document. A struct (not a map) keeps JSON field order stable.
type protectedResourceMetadata struct {
	Resource               string   `json:"resource"`
	AuthorizationServers   []string `json:"authorization_servers,omitempty"`
	BearerMethodsSupported []string `json:"bearer_methods_supported"`
	ResourceName           string   `json:"resource_name"`
}

// handleProtectedResource serves the RFC 9728 Protected Resource Metadata
// document UNAUTHENTICATED. The client uses it to discover which authorization
// server issues the bearer tokens this resource accepts.
func (h *httpHead) handleProtectedResource(w http.ResponseWriter, r *http.Request) {
	resource := h.publicURL
	if resource == "" {
		// No configured public URL -> derive the resource from the request.
		resource = requestBaseURL(r)
	}
	doc := protectedResourceMetadata{
		Resource:               resource,
		BearerMethodsSupported: []string{"header"},
		ResourceName:           "memQL MCP",
	}
	if h.authServerURL != "" {
		doc.AuthorizationServers = []string{h.authServerURL}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(doc)
}

type httpSession struct {
	srv      *Server
	mu       sync.Mutex // serialises Dispatch for this session
	lastSeen time.Time
}

func (h *httpHead) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		h.handlePost(w, r)
	case http.MethodGet:
		h.handleGet(w, r)
	case http.MethodDelete:
		h.handleDelete(w, r)
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
	}
}

func (h *httpHead) handlePost(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodyBytes))
	if err != nil {
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}
	method, _ := peekRPC(body)

	sessionID := strings.TrimSpace(r.Header.Get(sessionHeader))
	var sess *httpSession

	if method == "initialize" {
		// A fresh session: derive its Config from the caller's verified claims
		// (or the env pins) and assign a new session id.
		cfg := h.configForRequest(r)
		sessionID = newSessionID()
		sess = h.put(sessionID, h.newSrv(cfg))
		w.Header().Set(sessionHeader, sessionID)
		if h.logger != nil {
			h.logger.Info("mcp http: session initialized",
				"session", sessionID, "acting_user", cfg.ActingUser,
				"acting_role", cfg.ActingRole, "tier", cfg.Tier.String())
		}
	} else {
		if sessionID == "" {
			writeRPCError(w, http.StatusBadRequest, body, "missing Mcp-Session-Id header (call initialize first)")
			return
		}
		sess = h.get(sessionID)
		if sess == nil {
			// Unknown / expired session -> 404 so the client re-initializes
			// (per the Streamable HTTP spec).
			http.Error(w, "unknown or expired session", http.StatusNotFound)
			return
		}
	}

	sess.mu.Lock()
	respBytes, hasReply := sess.srv.Dispatch(r.Context(), body)
	sess.mu.Unlock()
	h.touch(sessionID)

	if !hasReply {
		// Notification or client->server response: ack with no body.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(respBytes)
}

// handleGet opens the server->client SSE channel for a session. Server
// notifications (resource-subscription pushes) flow here until the client
// disconnects. Returns 405 when the ResponseWriter can't stream.
func (h *httpHead) handleGet(w http.ResponseWriter, r *http.Request) {
	sessionID := strings.TrimSpace(r.Header.Get(sessionHeader))
	if sessionID == "" {
		http.Error(w, "missing Mcp-Session-Id header", http.StatusBadRequest)
		return
	}
	sess := h.get(sessionID)
	if sess == nil {
		http.Error(w, "unknown or expired session", http.StatusNotFound)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		w.Header().Set("Allow", "POST, DELETE")
		http.Error(w, "streaming unsupported", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	sw := &sseWriter{w: w, flusher: flusher}
	sess.srv.SetOutput(sw)
	defer sess.srv.SetOutput(nil)

	ticker := time.NewTicker(sseKeepaliveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			sw.keepalive()
			h.touch(sessionID)
		}
	}
}

func (h *httpHead) handleDelete(w http.ResponseWriter, r *http.Request) {
	sessionID := strings.TrimSpace(r.Header.Get(sessionHeader))
	if sessionID == "" {
		http.Error(w, "missing Mcp-Session-Id header", http.StatusBadRequest)
		return
	}
	if h.drop(sessionID) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Error(w, "unknown or expired session", http.StatusNotFound)
}

// configForRequest resolves the per-session Config: the deployment Tier plus
// the acting user/role. An env pin (BaseConfig.ActingUser / ActingRole) wins;
// otherwise the value comes from the caller's verified token claims, so each
// session acts as its own authenticated identity.
func (h *httpHead) configForRequest(r *http.Request) Config {
	cfg := h.base
	if cfg.ActingUser == "" || cfg.ActingRole == "" {
		if id, err := auth.UserIdentityFromContext(r.Context()); err == nil {
			if cfg.ActingUser == "" {
				cfg.ActingUser = id.Subject
			}
			if cfg.ActingRole == "" {
				cfg.ActingRole = id.Role
			}
		}
	}
	return cfg
}

// ---- session registry helpers ----

func (h *httpHead) put(id string, srv *Server) *httpSession {
	sess := &httpSession{srv: srv, lastSeen: nowUTC()}
	h.mu.Lock()
	h.sessions[id] = sess
	h.mu.Unlock()
	return sess
}

func (h *httpHead) get(id string) *httpSession {
	if id == "" {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sessions[id]
}

func (h *httpHead) touch(id string) {
	h.mu.Lock()
	if s := h.sessions[id]; s != nil {
		s.lastSeen = nowUTC()
	}
	h.mu.Unlock()
}

func (h *httpHead) drop(id string) bool {
	h.mu.Lock()
	sess, ok := h.sessions[id]
	delete(h.sessions, id)
	h.mu.Unlock()
	if ok && sess != nil {
		sess.srv.Close()
	}
	return ok
}

// reapIdle tears down sessions idle longer than the TTL. Called by the
// janitor on a timer.
func (h *httpHead) reapIdle(now time.Time) int {
	var stale []*httpSession
	h.mu.Lock()
	for id, sess := range h.sessions {
		if now.Sub(sess.lastSeen) > h.idleTTL {
			stale = append(stale, sess)
			delete(h.sessions, id)
		}
	}
	h.mu.Unlock()
	for _, sess := range stale {
		sess.srv.Close()
	}
	return len(stale)
}

// closeAll tears down every session (graceful shutdown).
func (h *httpHead) closeAll() {
	h.mu.Lock()
	sessions := h.sessions
	h.sessions = make(map[string]*httpSession)
	h.mu.Unlock()
	for _, sess := range sessions {
		sess.srv.Close()
	}
}

// ---------------------------------------------------------------------------
// sseWriter: re-frames each JSON-RPC message the Server writes as an SSE event.
// ---------------------------------------------------------------------------

type sseWriter struct {
	mu      sync.Mutex
	w       io.Writer
	flusher http.Flusher
}

// Write takes a single JSON-RPC message (the Server writes one message per
// Write, optionally newline-terminated) and emits it as `data: <json>\n\n`.
func (s *sseWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	msg := bytes.TrimRight(p, "\n")
	if _, err := io.WriteString(s.w, "data: "); err != nil {
		return 0, err
	}
	if _, err := s.w.Write(msg); err != nil {
		return 0, err
	}
	if _, err := io.WriteString(s.w, "\n\n"); err != nil {
		return 0, err
	}
	s.flusher.Flush()
	return len(p), nil
}

func (s *sseWriter) keepalive() {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = io.WriteString(s.w, ": keepalive\n\n")
	s.flusher.Flush()
}

// ---------------------------------------------------------------------------
// httpDependency: lifecycle wrapper.
// ---------------------------------------------------------------------------

type httpDependency struct {
	srv    *http.Server
	head   *httpHead
	logger *slog.Logger
	addr   string

	readyCh chan struct{}
	doneCh  chan struct{}
	running atomic.Bool

	janitorCancel context.CancelFunc
}

func (d *httpDependency) Start(_ context.Context) {
	d.running.Store(true)
	close(d.readyCh)

	jctx, cancel := context.WithCancel(context.Background())
	d.janitorCancel = cancel
	go d.runJanitor(jctx)

	go func() {
		defer close(d.doneCh)
		defer d.running.Store(false)
		d.logger.Info("mcp http server listening", "addr", d.addr, "path", MCPPath)
		if err := d.srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			d.logger.Error("mcp http server exited with error", "error", err)
			return
		}
		d.logger.Info("mcp http server stopped")
	}()
}

func (d *httpDependency) runJanitor(ctx context.Context) {
	interval := d.head.idleTTL / 2
	if interval < time.Minute {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if n := d.head.reapIdle(nowUTC()); n > 0 {
				d.logger.Info("mcp http: reaped idle sessions", "count", n)
			}
		}
	}
}

func (d *httpDependency) Stop(ctx context.Context) {
	if d.janitorCancel != nil {
		d.janitorCancel()
	}
	if err := d.srv.Shutdown(ctx); err != nil {
		d.logger.Warn("mcp http server shutdown error", "error", err)
	}
	d.head.closeAll()
	select {
	case <-d.doneCh:
	case <-ctx.Done():
	}
}

func (d *httpDependency) IsRunning() bool { return d.running.Load() }

// Order places the MCP head near the top of the stack so it stops before the
// engine/database underneath it during shutdown (mirrors the stdio head).
func (d *httpDependency) Order() int { return 80 }

func (d *httpDependency) ComponentName() common.ComponentName { return ComponentName }

func (d *httpDependency) Ready() <-chan struct{} { return d.readyCh }

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// peekRPC extracts the JSON-RPC method and whether an id is present, without
// fully decoding the message. Used to route initialize (new session) vs other
// methods and to distinguish requests from notifications.
func peekRPC(raw []byte) (method string, hasID bool) {
	var probe struct {
		Method string          `json:"method"`
		ID     json.RawMessage `json:"id"`
	}
	_ = json.Unmarshal(raw, &probe)
	trimmed := bytes.TrimSpace(probe.ID)
	return probe.Method, len(trimmed) > 0 && string(trimmed) != "null"
}

// writeRPCError emits a JSON-RPC error envelope with the given HTTP status,
// echoing the inbound id when present.
func writeRPCError(w http.ResponseWriter, status int, inbound []byte, msg string) {
	var probe struct {
		ID json.RawMessage `json:"id"`
	}
	_ = json.Unmarshal(inbound, &probe)
	resp := errorResponse(probe.ID, codeInvalidReq, msg)
	payload, _ := json.Marshal(resp)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(payload)
}

func newSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// rand.Read failing is catastrophic; fall back to a time-seeded value
		// so the server stays up (the session id is an opaque handle, not a
		// secret bearer -- the bearer JWT is the security boundary).
		return "sess-" + hex.EncodeToString([]byte(time.Now().UTC().Format(time.RFC3339Nano)))
	}
	return hex.EncodeToString(b[:])
}

func nowUTC() time.Time { return time.Now().UTC() }

// unauthorizedHandler builds the 401 responder used by the auth middleware. It
// advertises bearer auth so an OAuth-aware client can discover the scheme; when
// the public URL is known it also emits the RFC 9728 §5.1 resource_metadata
// hint pointing at the Protected Resource Metadata document. Claude Code passes
// the bearer header directly when it already holds a token.
func unauthorizedHandler(publicURL string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", wwwAuthenticate(publicURL))
		http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
	}
}

// wwwAuthenticate builds the WWW-Authenticate challenge for a 401. When
// publicURL is set it adds the RFC 9728 §5.1 resource_metadata hint pointing at
// the Protected Resource Metadata document; otherwise it falls back to the bare
// realm form.
func wwwAuthenticate(publicURL string) string {
	if publicURL == "" {
		return `Bearer realm="memql-mcp"`
	}
	return `Bearer realm="memql-mcp", resource_metadata="` + publicURL + ProtectedResourcePath + `"`
}

// requestBaseURL reconstructs the scheme+host base URL from an inbound request,
// used as the RFC 9728 `resource` identifier when no PublicURL is configured.
func requestBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if xf := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); xf != "" {
		scheme = xf
	}
	return scheme + "://" + r.Host
}
