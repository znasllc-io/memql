package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/httptls"
	"github.com/znasllc-io/memql/core/logger"
)

type (
	ServerArg       common.CtorArg[config]
	ListenerFactory func(network, address string) (net.Listener, error)

	NetHTTP struct {
		mu            sync.Mutex
		componentName common.ComponentName
		logger        *slog.Logger
		server        *http.Server
		listener      net.Listener
		config        *config
		running       bool
		lifecycle     common.Lifecycle
		activeBaseURL string
		readyCh       chan struct{}
	}

	config struct {
		address             string
		network             string
		useEnvBaseURL       bool
		baseURL             string
		writer              io.Writer
		level               slog.Level
		readTimeout         time.Duration
		readHeaderTimeout   time.Duration
		writeTimeout        time.Duration
		idleTimeout         time.Duration
		shutdownTimeout     time.Duration
		strictServer        StrictServerInterface
		strictMiddlewares   []StrictMiddlewareFunc
		stdMiddlewares      []MiddlewareFunc
		allowedOrigins      []string
		strictReqErrHandler func(http.ResponseWriter, *http.Request, error)
		strictResErrHandler func(http.ResponseWriter, *http.Request, error)
		stdErrorHandler     func(http.ResponseWriter, *http.Request, error)
		router              ServeMux
		listenerFactory     ListenerFactory
	}
)

const (
	defaultNetwork         = "tcp"
	defaultAddress         = ":8085"
	defaultReadTimeout     = 15 * time.Second
	defaultReadHeader      = 5 * time.Second
	defaultWriteTimeout    = 15 * time.Second
	defaultIdleTimeout     = 60 * time.Second
	defaultShutdownTimeout = 5 * time.Second
)

func defaultConfig() *config {
	return &config{
		address:           defaultAddress,
		network:           defaultNetwork,
		useEnvBaseURL:     true,
		allowedOrigins:    []string{"*"},
		writer:            os.Stdout,
		level:             slog.LevelInfo,
		readTimeout:       defaultReadTimeout,
		readHeaderTimeout: defaultReadHeader,
		writeTimeout:      defaultWriteTimeout,
		idleTimeout:       defaultIdleTimeout,
		shutdownTimeout:   defaultShutdownTimeout,
		strictServer:      nil,
		listenerFactory:   defaultListenerFactory,
	}
}

func defaultListenerFactory(network, address string) (net.Listener, error) {
	return net.Listen(network, address)
}

func optionalArg(name string, fn func(*config)) ServerArg {
	return common.NewCtorArg(name, true, fn)
}

func WithAddress(address string) ServerArg {
	return optionalArg("address", func(cfg *config) {
		trimmed := strings.TrimSpace(address)
		if trimmed != "" {
			cfg.address = trimmed
		}
	})
}

func WithNetwork(network string) ServerArg {
	return optionalArg("network", func(cfg *config) {
		if trimmed := strings.TrimSpace(network); trimmed != "" {
			cfg.network = trimmed
		}
	})
}

func WithBaseURL(baseURL string) ServerArg {
	return optionalArg("base_url", func(cfg *config) {
		cfg.baseURL = strings.TrimSpace(baseURL)
		cfg.useEnvBaseURL = false
	})
}

func WithBaseURLFromEnv() ServerArg {
	return optionalArg("base_url_env", func(cfg *config) {
		cfg.baseURL = ""
		cfg.useEnvBaseURL = true
	})
}

func WithLoggerWriter(writer io.Writer) ServerArg {
	return optionalArg("logger_writer", func(cfg *config) {
		if writer != nil {
			cfg.writer = writer
		}
	})
}

func WithLoggerLevel(level slog.Level) ServerArg {
	return optionalArg("logger_level", func(cfg *config) {
		cfg.level = level
	})
}

func WithReadTimeout(timeout time.Duration) ServerArg {
	return optionalArg("read_timeout", func(cfg *config) {
		if timeout >= 0 {
			cfg.readTimeout = timeout
		}
	})
}

func WithReadHeaderTimeout(timeout time.Duration) ServerArg {
	return optionalArg("read_header_timeout", func(cfg *config) {
		if timeout >= 0 {
			cfg.readHeaderTimeout = timeout
		}
	})
}

func WithWriteTimeout(timeout time.Duration) ServerArg {
	return optionalArg("write_timeout", func(cfg *config) {
		if timeout >= 0 {
			cfg.writeTimeout = timeout
		}
	})
}

func WithIdleTimeout(timeout time.Duration) ServerArg {
	return optionalArg("idle_timeout", func(cfg *config) {
		if timeout >= 0 {
			cfg.idleTimeout = timeout
		}
	})
}

func WithShutdownTimeout(timeout time.Duration) ServerArg {
	return optionalArg("shutdown_timeout", func(cfg *config) {
		if timeout >= 0 {
			cfg.shutdownTimeout = timeout
		}
	})
}

func WithStrictServer(strictServer StrictServerInterface) ServerArg {
	return optionalArg("strict_server", func(cfg *config) {
		if strictServer != nil {
			cfg.strictServer = strictServer
		}
	})
}

func WithStrictMiddlewares(middlewares ...StrictMiddlewareFunc) ServerArg {
	cloned := append([]StrictMiddlewareFunc(nil), middlewares...)
	return optionalArg("strict_middlewares", func(cfg *config) {
		cfg.strictMiddlewares = cloned
	})
}

func WithStandardMiddlewares(middlewares ...MiddlewareFunc) ServerArg {
	cloned := append([]MiddlewareFunc(nil), middlewares...)
	return optionalArg("standard_middlewares", func(cfg *config) {
		cfg.stdMiddlewares = cloned
	})
}

func WithAllowedOrigins(origins ...string) ServerArg {
	cloned := append([]string(nil), origins...)
	return optionalArg("allowedOrigins", func(cfg *config) {
		cfg.allowedOrigins = normalizeOrigins(cloned)
	})
}

func WithStrictRequestErrorHandler(handler func(http.ResponseWriter, *http.Request, error)) ServerArg {
	return optionalArg("strict_request_error_handler", func(cfg *config) {
		cfg.strictReqErrHandler = handler
	})
}

func WithStrictResponseErrorHandler(handler func(http.ResponseWriter, *http.Request, error)) ServerArg {
	return optionalArg("strict_response_error_handler", func(cfg *config) {
		cfg.strictResErrHandler = handler
	})
}

func WithStandardErrorHandler(handler func(http.ResponseWriter, *http.Request, error)) ServerArg {
	return optionalArg("standard_error_handler", func(cfg *config) {
		cfg.stdErrorHandler = handler
	})
}

func WithBaseRouter(router ServeMux) ServerArg {
	return optionalArg("base_router", func(cfg *config) {
		cfg.router = router
	})
}

func WithListenerFactory(factory ListenerFactory) ServerArg {
	return optionalArg("listener_factory", func(cfg *config) {
		if factory != nil {
			cfg.listenerFactory = factory
		}
	})
}

func NewNetHTTP(componentName common.ComponentName, args ...ServerArg) (*NetHTTP, error) {
	cfg := defaultConfig()

	for _, arg := range args {
		if arg == nil {
			continue
		}
		arg.Apply(cfg)
	}

	if cfg.strictServer == nil {
		return nil, fmt.Errorf("strict server implementation is required")
	}

	cfg.strictMiddlewares = prependStrictMiddleware(cfg.strictMiddlewares, injectRequestContext)

	if cfg.listenerFactory == nil {
		cfg.listenerFactory = defaultListenerFactory
	}

	if cfg.writer == nil {
		cfg.writer = os.Stdout
	}

	if strings.TrimSpace(cfg.network) == "" {
		cfg.network = defaultNetwork
	}

	logger := logger.New(componentName, cfg.writer, cfg.level)

	s := &NetHTTP{
		componentName: componentName,
		logger:        logger,
		config:        cfg,
		readyCh:       make(chan struct{}),
	}

	logger.Info("instance created")

	return s, nil
}

func prependStrictMiddleware(existing []StrictMiddlewareFunc, middleware StrictMiddlewareFunc) []StrictMiddlewareFunc {
	if middleware == nil {
		return existing
	}
	middlewares := make([]StrictMiddlewareFunc, 0, len(existing)+1)
	middlewares = append(middlewares, middleware)
	middlewares = append(middlewares, existing...)
	return middlewares
}

func (s *NetHTTP) Start(ctx context.Context) {
	if s == nil {
		return
	}

	err := s.lifecycle.Start(ctx, s.logger, common.LifecycleHooks{
		Prepare: s.prepareForRun,
		Run:     s.run,
		OnStop:  s.reset,
		OnStarted: func() {
			s.mu.Lock()
			s.running = true
			s.mu.Unlock()
			select {
			case <-s.readyCh:
			default:
				close(s.readyCh)
			}
		},
	})

	if err != nil {
		switch {
		case errors.Is(err, common.ErrLifecycleAlreadyRunning):
			s.logWarn("start skipped; server already running")
		case errors.Is(err, context.Canceled):
			// context cancellation already logged by lifecycle
		default:
			s.logError("failed to start", err)
		}
	}
}

func (s *NetHTTP) Stop(ctx context.Context) {
	if s == nil {
		return
	}

	if err := s.lifecycle.Stop(ctx, s.logger); err != nil && !errors.Is(err, common.ErrLifecycleNotRunning) && !errors.Is(err, context.Canceled) {
		s.logError("failed to stop", err)
	}
}

func (s *NetHTTP) IsRunning() bool {
	if s == nil {
		return false
	}

	return s.lifecycle.IsRunning()
}

func (s *NetHTTP) ComponentName() common.ComponentName {
	if s == nil {
		return ""
	}
	return s.componentName
}

func (s *NetHTTP) Order() int {
	return 100
}

// Ready returns a channel that is closed when the server is ready.
func (s *NetHTTP) Ready() <-chan struct{} {
	return s.readyCh
}

func (s *NetHTTP) Address() string {
	if s == nil || s.config == nil {
		return ""
	}
	return strings.TrimSpace(s.config.address)
}

func (s *NetHTTP) prepareForRun(ctx context.Context) (context.Context, context.CancelFunc, error) {
	ctx = ensureContext(ctx)

	s.mu.Lock()
	cfg := s.config
	baseURL := cfg.baseURL

	if cfg.useEnvBaseURL {
		baseURL = sanitizeBaseURLFromEnv()
	}

	strictHandler := s.buildStrictHandler(cfg)

	routerHandler := HandlerWithOptions(strictHandler, StdHTTPServerOptions{
		BaseURL:          baseURL,
		BaseRouter:       cfg.router,
		ErrorHandlerFunc: cfg.stdErrorHandler,
	})

	handler := http.Handler(routerHandler)
	for i := len(cfg.stdMiddlewares) - 1; i >= 0; i-- {
		if middleware := cfg.stdMiddlewares[i]; middleware != nil {
			handler = middleware(handler)
		}
	}

	// Apply CORS middleware as the outermost layer (runs first)
	handler = corsMiddleware(cfg.allowedOrigins, handler)

	server := &http.Server{
		Addr:              cfg.address,
		Handler:           handler,
		ReadTimeout:       cfg.readTimeout,
		ReadHeaderTimeout: cfg.readHeaderTimeout,
		WriteTimeout:      cfg.writeTimeout,
		IdleTimeout:       cfg.idleTimeout,
	}

	s.server = server
	s.activeBaseURL = baseURL
	s.listener = nil
	s.running = false
	s.mu.Unlock()

	runCtx, cancel := context.WithCancel(ctx)
	return runCtx, cancel, nil
}

func (s *NetHTTP) run(ctx context.Context, markStarted func()) error {
	cfg := s.config
	listener, err := cfg.listenerFactory(cfg.network, cfg.address)

	if err != nil {
		s.logError("failed to listen", err, "network", cfg.network, "address", cfg.address)
		return err
	}

	s.mu.Lock()
	s.listener = listener
	server := s.server
	s.mu.Unlock()

	if server == nil {
		_ = listener.Close()
		s.mu.Lock()
		s.listener = nil
		s.mu.Unlock()
		s.logError("server not configured", errors.New("http server is nil"))
		return errors.New("server not configured")
	}

	// Optional TLS: when MEMQL_HTTP_TLS_CERT_FILE/_KEY_FILE are set
	// (e.g. on the identity node so /node/bootstrap + JWKS are served
	// over https), serve TLS on the same listener. Unset == plaintext
	// (the legacy default, fine behind a TLS-terminating proxy / LB).
	certFile, keyFile, tlsEnabled, tlsErr := httptls.ServerCertFiles()
	if tlsErr != nil {
		_ = listener.Close()
		s.mu.Lock()
		s.listener = nil
		s.mu.Unlock()
		s.logError("http server tls misconfigured", tlsErr)
		return tlsErr
	}

	s.logInfo("http server listening", "network", cfg.network, "address", listener.Addr().String(), "base_url", s.activeBaseURL, "tls", tlsEnabled)
	markStarted()

	errCh := make(chan error, 1)
	go func() {
		if tlsEnabled {
			errCh <- server.ServeTLS(listener, certFile, keyFile)
		} else {
			errCh <- server.Serve(listener)
		}
	}()

	var serveErr error

	select {
	case <-ctx.Done():
		s.gracefulShutdown(ctx)
		serveErr = <-errCh
	case serveErr = <-errCh:
	}

	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		s.logError("http server stopped with error", serveErr)
		return serveErr
	}

	if serveErr == nil {
		s.logInfo("http server stopped")
	}

	return nil
}

func (s *NetHTTP) gracefulShutdown(ctx context.Context) {
	ctx = ensureContext(ctx)

	s.mu.Lock()
	server := s.server
	timeout := s.config.shutdownTimeout
	s.mu.Unlock()

	if server == nil {
		return
	}

	shutdownCtx := ctx

	if _, hasDeadline := ctx.Deadline(); !hasDeadline && timeout > 0 {
		var cancel context.CancelFunc
		shutdownCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	if err := server.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, http.ErrServerClosed) {
		s.logError("graceful shutdown failed", err)
	} else {
		s.logInfo("graceful shutdown complete")
	}
}

func (s *NetHTTP) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.listener != nil {
		_ = s.listener.Close()
	}

	s.listener = nil
	s.server = nil
	s.running = false
}

func (s *NetHTTP) buildStrictHandler(cfg *config) ServerInterface {
	middlewares := append([]StrictMiddlewareFunc(nil), cfg.strictMiddlewares...)

	if cfg.strictReqErrHandler != nil || cfg.strictResErrHandler != nil {
		options := StrictHTTPServerOptions{
			RequestErrorHandlerFunc:  cfg.strictReqErrHandler,
			ResponseErrorHandlerFunc: cfg.strictResErrHandler,
		}

		if options.RequestErrorHandlerFunc == nil {
			options.RequestErrorHandlerFunc = defaultStrictRequestErrorHandler
		}

		if options.ResponseErrorHandlerFunc == nil {
			options.ResponseErrorHandlerFunc = defaultStrictResponseErrorHandler
		}

		return NewStrictHandlerWithOptions(cfg.strictServer, middlewares, options)
	}

	return NewStrictHandler(cfg.strictServer, middlewares)
}

func (s *NetHTTP) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.listener != nil {
		return s.listener.Addr().String()
	}

	if s.server != nil && strings.TrimSpace(s.server.Addr) != "" {
		return s.server.Addr
	}

	if s.config != nil {
		return s.config.address
	}

	return ""
}

func defaultStrictRequestErrorHandler(w http.ResponseWriter, r *http.Request, err error) {
	WriteSafeError(w, r, nil, http.StatusBadRequest, "bad_request", "invalid request", err)
}

func defaultStrictResponseErrorHandler(w http.ResponseWriter, r *http.Request, err error) {
	WriteSafeError(w, r, nil, http.StatusInternalServerError, "internal", "internal server error", err)
}

func ensureContext(ctx context.Context) context.Context {
	if ctx != nil {
		return ctx
	}
	return context.Background()
}

func (s *NetHTTP) logInfo(msg string, args ...any) {
	if s == nil || s.logger == nil {
		return
	}
	s.logger.Info(msg, args...)
}

func (s *NetHTTP) logWarn(msg string, args ...any) {
	if s == nil || s.logger == nil {
		return
	}
	s.logger.Warn(msg, args...)
}

func (s *NetHTTP) logError(msg string, err error, args ...any) {
	if s == nil || s.logger == nil || err == nil {
		return
	}
	args = append(args, "error", err)
	s.logger.Error(msg, args...)
}

const (
	defaultPublicPath   = "/"
	serverPublicPathEnv = "SERVER_PUBLIC_PATH"
)

func sanitizeBaseURLFromEnv() string {
	raw := strings.TrimSpace(os.Getenv(serverPublicPathEnv))
	if raw == "" {
		raw = defaultPublicPath
	}
	return normalizeBaseURL(raw)
}

func normalizeBaseURL(raw string) string {
	if !strings.HasPrefix(raw, "/") {
		raw = "/" + raw
	}

	cleanPath := path.Clean(raw)

	if cleanPath == "." {
		cleanPath = defaultPublicPath
	}

	if cleanPath == defaultPublicPath {
		return ""
	}

	return cleanPath
}

func HealthzPaths() []string {
	base := sanitizeBaseURLFromEnv()
	paths := []string{"/healthz"}

	if base != "" {
		healthPath := base + "/healthz"
		if healthPath != "/healthz" {
			paths = append(paths, healthPath)
		}
	}

	return paths
}

// ReadyzPaths returns the public path(s) for the schema-assertion readiness
// probe (#657), honoring SERVER_PUBLIC_PATH the same way HealthzPaths does.
func ReadyzPaths() []string {
	base := sanitizeBaseURLFromEnv()
	paths := []string{"/readyz"}

	if base != "" {
		readyPath := base + "/readyz"
		if readyPath != "/readyz" {
			paths = append(paths, readyPath)
		}
	}

	return paths
}

// LivezPaths returns the public path(s) for the pure process-liveness probe
// (#1117), honoring SERVER_PUBLIC_PATH the same way HealthzPaths does. The
// k8s livenessProbe targets /livez so a transient dependency/mesh blip never
// liveness-kills an otherwise-alive pod (readiness stays on /healthz).
func LivezPaths() []string {
	base := sanitizeBaseURLFromEnv()
	paths := []string{"/livez"}

	if base != "" {
		livePath := base + "/livez"
		if livePath != "/livez" {
			paths = append(paths, livePath)
		}
	}

	return paths
}

// AuthPaths returns paths for the identity-service auth endpoints
// proxied through this binary. They're listed as public so the
// verifier's HTTP middleware doesn't gate the unauthenticated
// landing/login redirect surface that some browsers still hit.
//
// Note: the identity service itself owns the canonical auth pages
// at the MEMQL_IDENTITY_BASE_URL. These entries are kept for backward
// compatibility and the dev-mode landing/logout shim in
// app/config.go.
func AuthPaths() []string {
	base := sanitizeBaseURLFromEnv()
	authEndpoints := []string{"/auth/login", "/auth/callback", "/auth/logout", "/auth/landing", "/auth/exchange"}
	paths := make([]string, 0, len(authEndpoints)*2)

	for _, endpoint := range authEndpoints {
		paths = append(paths, endpoint)
		if base != "" {
			basePath := base + endpoint
			if basePath != endpoint {
				paths = append(paths, basePath)
			}
		}
	}

	return paths
}

// JWKSPaths returns the public JWKS endpoint(s). The keyset MUST be
// fetchable WITHOUT auth: every node's verifier fetches it to validate
// tokens, and in cluster mode it does so over HTTP (e.g.
// http://identity:8085/.well-known/jwks.json) before it holds any token
// of its own. Gating this path deadlocks cross-node auth.
func JWKSPaths() []string {
	return withBasePath("/.well-known/jwks.json")
}

// IdentityDiscoveryPaths returns the identity service's public discovery
// documents. Both are mounted by identity's Service.RegisterRoutes and both
// MUST be reachable without auth to do their job: the memql-config document is
// what `memql-cockpit authorize <url>` reads to learn which gRPC endpoint and
// client_id to use, and the RFC 8414 metadata is how an OAuth client discovers
// the authorize/token/registration endpoints before it holds any credential.
// Neither carries user data.
//
// Declared here because #2939 is about the unauthenticated surface being
// DECLARED rather than incidental. These two were unauthenticated by
// deliberate design and named in no list at all -- and the handoff exemption
// in app/mux_registration_test.go justified itself by asserting identity's
// well-knowns were already in PublicPaths(), which was true of jwks.json and
// false of these. component/identity/wellknown_declared_test.go keeps the
// declaration in step with what identity actually mounts.
//
// Know what declaring them costs, because PublicPaths() is not only read by
// this package's boot check: verifier.shouldBypassAuth treats EVERY entry as a
// prefix regardless of trailing slash, so on every verifier-consuming node
// these entries also bypass auth for anything mounted BENEATH them --
// /.well-known/oauth-authorization-server/<issuer>, the RFC 8414
// issuer-suffixed form, being the realistic one. Nothing is mounted there
// today on any binary. Anything added there later inherits the bypass without
// a further decision, so it is a decision to make then, not an accident to
// discover.
func IdentityDiscoveryPaths() []string {
	return withBasePath(
		"/.well-known/memql-config.json",
		"/.well-known/oauth-authorization-server",
	)
}

// withBasePath returns each path as written, plus its base-prefixed spelling
// when SERVER_PUBLIC_PATH is configured and that spelling differs.
func withBasePath(paths ...string) []string {
	base := sanitizeBaseURLFromEnv()
	out := make([]string, 0, len(paths)*2)
	for _, p := range paths {
		out = append(out, p)
		if base == "" {
			continue
		}
		if prefixed := base + p; prefixed != p {
			out = append(out, prefixed)
		}
	}
	return out
}

// MetricsPaths returns the public Prometheus scrape endpoint(s). The
// /metrics endpoint carries no user data and is only reachable on the
// in-cluster pod network (the public ingress routes specific paths
// only), so it is unauthenticated like the health probes -- a
// ServiceMonitor/Prometheus scrape can't present a bearer token.
// memql#1523.
func MetricsPaths() []string {
	base := sanitizeBaseURLFromEnv()
	metricsPath := "/metrics"
	paths := []string{metricsPath}
	if base != "" {
		p := base + metricsPath
		if p != metricsPath {
			paths = append(paths, p)
		}
	}
	return paths
}

func PublicPaths() []string {
	paths := HealthzPaths()
	paths = append(paths, ReadyzPaths()...)  // schema-assertion readiness probe (#657)
	paths = append(paths, LivezPaths()...)   // pure process-liveness probe (#1117)
	paths = append(paths, MetricsPaths()...) // prometheus scrape (#1523)
	paths = append(paths, JWKSPaths()...)    // public keyset (cross-node verifier fetch)
	// identity's public discovery documents (cockpit authorize + RFC 8414)
	paths = append(paths, IdentityDiscoveryPaths()...)
	paths = append(paths, AuthPaths()...) // identity-service auth endpoints
	// Polyphon Bridge Agent internal endpoints (service-to-service, no user auth)
	paths = append(paths, PolyphonUtterancePaths()...)
	paths = append(paths, PolyphonPreloadPaths()...)
	// AI HTTP endpoints (service-to-service, e.g., frontend -> memQL)
	paths = append(paths, AIHTTPPaths()...)
	// Concept metadata endpoint (public, no auth required)
	paths = append(paths, ConceptAPIPaths()...)
	return paths
}

// MemqlWebsocketPaths returns all valid websocket endpoints for the WebSocket bridge.
func MemqlWebsocketPaths() []string {
	base := sanitizeBaseURLFromEnv()
	wsPath := "/memql/ws"
	paths := []string{wsPath}
	if base != "" {
		p := base + wsPath
		if p != wsPath {
			paths = append(paths, p)
		}
	}
	return paths
}

// AudioWebsocketPaths returns all valid websocket endpoints for audio streaming.
func AudioWebsocketPaths() []string {
	base := sanitizeBaseURLFromEnv()
	audioPath := "/memql/audio"
	paths := []string{audioPath}
	if base != "" {
		p := base + audioPath
		if p != audioPath {
			paths = append(paths, p)
		}
	}
	return paths
}

// pathsWithBase is a helper that returns paths with optional SERVER_PUBLIC_PATH prefix.
func pathsWithBase(path string) []string {
	base := sanitizeBaseURLFromEnv()
	paths := []string{path}
	if base != "" {
		p := base + path
		if p != path {
			paths = append(paths, p)
		}
	}
	return paths
}

// PolyphonRoomTokenPaths returns paths for the Polyphon room token endpoint.
func PolyphonRoomTokenPaths() []string {
	return pathsWithBase("/polyphon/room-token")
}

// PolyphonStatusPaths returns paths for the Polyphon status endpoint.
func PolyphonStatusPaths() []string {
	return pathsWithBase("/polyphon/status")
}

// PolyphonUtterancePaths returns paths for the Bridge Agent utterance endpoint.
// This is an internal endpoint that bypasses auth for service-to-service communication.
func PolyphonUtterancePaths() []string {
	return pathsWithBase("/polyphon/utterance")
}

// PolyphonPreloadPaths returns paths for the Bridge Agent prewarm
// endpoint. Bridge fires this when the user starts speaking so the
// cognition node warms its per-space caches in parallel with the
// ASR session, knocking ~50-150ms off handlerOffsetMs on the
// downstream utterance handler. Public path (no auth) for the same
// service-to-service reasons as /polyphon/utterance.
func PolyphonPreloadPaths() []string {
	return pathsWithBase("/polyphon/preload")
}

// AIHTTPPaths used to return the legacy /si/* HTTP endpoints. All of
// them have been retired in favour of MemqlService.Stream with
// AiChatMsg / AiSpeechMsg / AiTranscribeMsg / AiSuggestMsg. The
// function stays as an empty stub so callers that walked the list
// compile without fanning out to every call site; the auth middleware
// just gets an empty list now.
func AIHTTPPaths() []string {
	return nil
}

// SpaceAttachmentPaths returns the path prefix used to register the space
// attachment upload handler (POST /spaces/{partitionId}/attachments).
func SpaceAttachmentPaths() []string {
	return pathsWithBase("/spaces/")
}

// InboundWebhookPaths returns the path prefix the inbound-delivery receiver
// mounts under (POST /inbound/{source}, memql#2957).
//
// HTTP rather than gRPC because the caller is a third party that dials US --
// Shopify, Amazon SP-API, a POS -- so the endpoint-protocol policy's
// external-requirement exception applies exactly as it does to the OAuth
// callbacks.
//
// Declared in HandlerAuthorizedPaths(), NOT PublicPaths(): the receiver
// authorizes every request itself against a deny-by-default source allowlist
// plus a per-source HMAC check, and refuses with 404/401 when either is
// missing. Listing it in PublicPaths() would bypass the verifier for
// /inbound/* on every verifier-consuming node -- unnecessary (the handler
// wants no memQL identity) and strictly worse, since the prefix would then
// bless anything mounted beneath it later.
func InboundWebhookPaths() []string {
	return pathsWithBase("/inbound/")
}

func normalizeOrigins(origins []string) []string {
	if len(origins) == 0 {
		return []string{"*"}
	}

	var (
		normalized []string
		seen       = make(map[string]struct{}, len(origins))
		allowAll   bool
	)

	for _, origin := range origins {
		trimmed := strings.TrimSpace(origin)
		if trimmed == "" {
			continue
		}
		if trimmed == "*" {
			allowAll = true
			break
		}
		key := strings.ToLower(trimmed)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		normalized = append(normalized, trimmed)
	}

	if allowAll {
		return []string{"*"}
	}

	if len(normalized) == 0 {
		return []string{"*"}
	}

	return normalized
}

// corsMiddleware returns an HTTP middleware that handles CORS preflight requests
// and adds appropriate CORS headers to all responses.
func corsMiddleware(allowedOrigins []string, next http.Handler) http.Handler {
	// Build a set of allowed origins for O(1) lookup
	allowAll := false
	originSet := make(map[string]struct{}, len(allowedOrigins))
	for _, origin := range allowedOrigins {
		if origin == "*" {
			allowAll = true
			break
		}
		originSet[strings.ToLower(origin)] = struct{}{}
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")

		// Determine if origin is allowed
		var allowedOrigin string
		if origin != "" {
			if allowAll {
				allowedOrigin = origin
			} else if _, ok := originSet[strings.ToLower(origin)]; ok {
				allowedOrigin = origin
			}
		}

		// Set CORS headers if origin is allowed.
		//
		// Credentials posture: `Access-Control-Allow-Credentials: true`
		// is ONLY emitted when the allow-list is explicit (not `*`).
		// The CORS spec forbids credentials + wildcard; browsers
		// silently reject the response. Worse, when we echoed the
		// caller's Origin alongside `Credentials: true`, the response
		// was effectively "any origin may read credentialed
		// responses" -- a credentialed-XHR exfiltration surface.
		// Dev environments that need credentialed requests must
		// configure an explicit allow-list; the legacy wildcard
		// fallback degrades to a credential-less posture instead.
		if allowedOrigin != "" {
			w.Header().Set("Access-Control-Allow-Origin", allowedOrigin)
			if !allowAll {
				w.Header().Set("Access-Control-Allow-Credentials", "true")
			}
			w.Header().Set("Vary", "Origin")
		}

		// Handle preflight OPTIONS request
		if r.Method == http.MethodOptions {
			if allowedOrigin != "" {
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Accept, Authorization, Content-Type, X-Requested-With")
				w.Header().Set("Access-Control-Max-Age", "86400") // 24 hours
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}
