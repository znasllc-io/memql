package memqlws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/core/id"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"nhooyr.io/websocket"
)

const (
	defaultClientId                  = "memql-ws"
	defaultSDKName                   = "memql"
	defaultSDKVersion                = "websocket"
	defaultDialTimeout               = 5 * time.Second
	defaultWriteTimeout              = 10 * time.Second
	defaultMaxConcurrent             = 4
	defaultMaxPending                = 64
	defaultMaxLocalErrors            = 256
	defaultMaxMessageBytes     int64 = 5 * 1024 * 1024
	defaultPingInterval              = 30 * time.Second
	defaultOverloadLogInterval       = 1 * time.Second
	defaultLocalErrorInterval        = 50 * time.Millisecond
)

// Options configure the WebSocket bridge.
type Options struct {
	Logger                *slog.Logger
	Endpoint              string
	DialTimeout           time.Duration
	MaxConcurrentRequests int
	MaxMessageBytes       int64
	WriteTimeout          time.Duration
	PingInterval          time.Duration
	// OriginPatterns is the allow-list of Origin header values that may
	// upgrade to a WebSocket. Patterns are passed verbatim to
	// nhooyr.io/websocket's AcceptOptions; that library matches with
	// shell-style globs (so "https://*.example.com" works). Empty means
	// "fall back to the legacy wildcard". Production deployments should
	// always supply an explicit list (e.g. via MEMQL_WEBSOCKET_ORIGIN_PATTERNS
	// in the bootstrap config) so the upgrade can't be triggered from an
	// arbitrary attacker page. See docs/auth/threat-model.md.
	OriginPatterns []string
}

// Handler proxies WebSocket frames to the MemQL gRPC stream surface.
type Handler struct {
	logger                *slog.Logger
	endpoint              string
	dialTimeout           time.Duration
	maxConcurrentRequests int
	maxMessageBytes       int64
	writeTimeout          time.Duration
	pingInterval          time.Duration
	originPatterns        []string
}

// New constructs a Handler.
func New(opts Options) (*Handler, error) {
	endpoint := strings.TrimSpace(opts.Endpoint)
	if endpoint == "" {
		return nil, fmt.Errorf("memql websocket handler requires a gRPC endpoint")
	}
	return &Handler{
		logger:                opts.Logger,
		endpoint:              endpoint,
		dialTimeout:           firstDuration(opts.DialTimeout, defaultDialTimeout),
		maxConcurrentRequests: firstPositiveInt(opts.MaxConcurrentRequests, defaultMaxConcurrent),
		maxMessageBytes:       firstPositiveInt64(opts.MaxMessageBytes, defaultMaxMessageBytes),
		writeTimeout:          firstDuration(opts.WriteTimeout, defaultWriteTimeout),
		pingInterval:          firstDuration(opts.PingInterval, defaultPingInterval),
		originPatterns:        sanitizeOriginPatterns(opts.OriginPatterns),
	}, nil
}

// sanitizeOriginPatterns drops empty strings and trims whitespace. When
// the result is empty, the handler falls back to the legacy wildcard
// pattern so existing dev environments stay unblocked. A WARN log at
// upgrade time flags the situation.
func sanitizeOriginPatterns(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		t := strings.TrimSpace(s)
		if t == "" {
			continue
		}
		out = append(out, t)
	}
	return out
}

// ServeHTTP upgrades eligible requests and proxies messages.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r == nil || r.Method != http.MethodGet {
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}

	identity, err := auth.UserIdentityFromContext(r.Context())
	if err != nil {
		h.logWarn("memql websocket identity missing", "remote", remoteAddr(r))
		http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
		return
	}
	// Allow any authenticated user (role check removed to support frontend client apps)

	originPatterns := h.originPatterns
	if len(originPatterns) == 0 {
		// No allow-list configured. Fall back to wildcard for backwards
		// compatibility, but log so the operator knows the upgrade is
		// effectively unauthenticated by origin. Production deployments
		// must populate MEMQL_WEBSOCKET_ORIGIN_PATTERNS.
		h.logWarn("memql websocket origin allow-list empty; accepting any origin -- configure MEMQL_WEBSOCKET_ORIGIN_PATTERNS in production",
			"remote", remoteAddr(r))
		originPatterns = []string{"*"}
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionDisabled,
		OriginPatterns:  originPatterns,
	})
	if err != nil {
		h.logError("websocket accept failed", err)
		return
	}
	defer conn.Close(websocket.StatusInternalError, "unexpected error")

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	h.logInfo("memql websocket request accepted", "subject", identity.Subject, "role", identity.Role, "remote", remoteAddr(r))

	stream, grpcConn, err := h.openStream(ctx, r)
	if err != nil {
		h.logError("failed to open memql stream", err)
		conn.Close(websocket.StatusInternalError, "memql stream unavailable")
		return
	}
	defer grpcConn.Close()

	if err := h.sendClientHello(ctx, stream); err != nil {
		h.logError("failed to send client hello", err)
		conn.Close(websocket.StatusInternalError, "memql stream unavailable")
		return
	}

	conn.SetReadLimit(h.maxMessageBytes)
	session := newSession(sessionConfig{
		ctx:          ctx,
		logger:       h.logger,
		conn:         conn,
		stream:       stream,
		principal:    identity.Subject,
		maxActive:    h.maxConcurrentRequests,
		maxBytes:     h.maxMessageBytes,
		writeLimit:   h.writeTimeout,
		pingInterval: h.pingInterval,
	})

	if err := session.run(); err != nil && !errors.Is(err, context.Canceled) {
		h.logWarn("memql websocket session ended", "subject", identity.Subject, "error", err)
	} else {
		h.logInfo("memql websocket session closed", "subject", identity.Subject)
	}
}

func (h *Handler) openStream(ctx context.Context, r *http.Request) (memqlv1.MemqlService_StreamClient, *grpc.ClientConn, error) {
	dialCtx, cancel := context.WithTimeout(ctx, h.dialTimeout)
	defer cancel()

	conn, err := grpc.DialContext(dialCtx, h.endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		return nil, nil, err
	}

	client := memqlv1.NewMemqlServiceClient(conn)
	md := metadataFromRequest(r)
	streamCtx := metadata.NewOutgoingContext(ctx, md)

	stream, err := client.Stream(streamCtx)
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	return stream, conn, nil
}

func (h *Handler) sendClientHello(_ context.Context, stream memqlv1.MemqlService_StreamClient) error {
	msg := &memqlv1.MemqlClientMessage{
		MessageId: id.NewShortId(),
		Payload: &memqlv1.MemqlClientMessage_ClientHello{
			ClientHello: &memqlv1.ClientHello{
				ClientId:   defaultClientId,
				SdkName:    defaultSDKName,
				SdkVersion: defaultSDKVersion,
			},
		},
	}
	return stream.Send(msg)
}

func (h *Handler) logError(msg string, err error, args ...any) {
	if h.logger == nil || err == nil {
		return
	}
	args = append(args, "error", err)
	h.logger.Error(msg, args...)
}

func (h *Handler) logWarn(msg string, args ...any) {
	if h.logger == nil {
		return
	}
	h.logger.Warn(msg, args...)
}

func (h *Handler) logInfo(msg string, args ...any) {
	if h.logger == nil {
		return
	}
	h.logger.Info(msg, args...)
}

func remoteAddr(r *http.Request) string {
	if r == nil {
		return ""
	}
	return strings.TrimSpace(r.RemoteAddr)
}

type sessionConfig struct {
	ctx          context.Context
	logger       *slog.Logger
	conn         *websocket.Conn
	stream       memqlv1.MemqlService_StreamClient
	principal    string
	maxActive    int
	maxBytes     int64
	writeLimit   time.Duration
	pingInterval time.Duration
}

type session struct {
	ctx          context.Context
	logger       *slog.Logger
	conn         *websocket.Conn
	stream       memqlv1.MemqlService_StreamClient
	principal    string
	maxActive    int
	maxBytes     int64
	writeLimit   time.Duration
	pingInterval time.Duration

	activeMu sync.Mutex
	active   map[string]struct{}

	pending      chan *queuedExecute
	pendingMu    sync.Mutex
	pendingSet   map[string]struct{}
	cancelledMu  sync.Mutex
	cancelledSet map[string]struct{}
	tokens       chan struct{}
	localErrors  chan *queuedLocalError
	overloadMu   sync.Mutex
	lastOverload time.Time
}

type queuedExecute struct {
	requestId string
	msg       *memqlv1.MemqlClientMessage
}

type queuedLocalError struct {
	requestId string
	code      codes.Code
	message   string
}

func newSession(cfg sessionConfig) *session {
	maxActive := cfg.maxActive
	if maxActive <= 0 {
		maxActive = defaultMaxConcurrent
	}
	maxBytes := cfg.maxBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxMessageBytes
	}
	writeLimit := cfg.writeLimit
	if writeLimit <= 0 {
		writeLimit = defaultWriteTimeout
	}
	pingInterval := cfg.pingInterval
	if pingInterval <= 0 {
		pingInterval = defaultPingInterval
	}

	return &session{
		ctx:          cfg.ctx,
		logger:       cfg.logger,
		conn:         cfg.conn,
		stream:       cfg.stream,
		principal:    cfg.principal,
		maxActive:    maxActive,
		maxBytes:     maxBytes,
		writeLimit:   writeLimit,
		pingInterval: pingInterval,
		active:       make(map[string]struct{}),
		pending:      make(chan *queuedExecute, defaultMaxPending),
		pendingSet:   make(map[string]struct{}),
		cancelledSet: make(map[string]struct{}),
		tokens:       make(chan struct{}, maxActive),
		localErrors:  make(chan *queuedLocalError, defaultMaxLocalErrors),
	}
}

func (s *session) run() error {
	if s.conn == nil || s.stream == nil {
		return fmt.Errorf("session not initialized")
	}

	errCh := make(chan error, 4)
	go func() {
		errCh <- s.readLoop()
	}()
	go func() {
		errCh <- s.writeLoop()
	}()
	go func() {
		errCh <- s.pingLoop()
	}()
	go func() {
		errCh <- s.dispatchLoop()
	}()
	go s.localErrorLoop()

	err := <-errCh
	s.conn.Close(websocket.StatusNormalClosure, "")
	s.stream.CloseSend()
	return err
}

func (s *session) readLoop() error {
	for {
		select {
		case <-s.ctx.Done():
			return s.ctx.Err()
		default:
		}

		msgType, data, err := s.conn.Read(s.ctx)
		if err != nil {
			return err
		}
		if msgType != websocket.MessageBinary && msgType != websocket.MessageText {
			continue
		}
		if int64(len(data)) > s.maxBytes {
			return fmt.Errorf("client message exceeded %d bytes", s.maxBytes)
		}

		clientMsg := &memqlv1.MemqlClientMessage{}
		if msgType == websocket.MessageBinary {
			if err := proto.Unmarshal(data, clientMsg); err != nil {
				return err
			}
		} else {
			if err := protojson.Unmarshal(data, clientMsg); err != nil {
				return err
			}
		}

		if err := s.forwardClientMessage(clientMsg); err != nil {
			return err
		}
	}
}

func (s *session) writeLoop() error {
	for {
		serverMsg, err := s.stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}

		s.handleServerMessage(serverMsg)

		payload, err := marshalServerMessage(serverMsg)
		if err != nil {
			return err
		}
		if err := s.writeMessage(payload); err != nil {
			return err
		}
	}
}

func (s *session) writeMessage(payload []byte) error {
	ctx, cancel := context.WithTimeout(s.ctx, s.writeLimit)
	defer cancel()
	return s.conn.Write(ctx, websocket.MessageText, payload)
}

func (s *session) forwardClientMessage(msg *memqlv1.MemqlClientMessage) error {
	if msg == nil {
		return nil
	}
	switch payload := msg.GetPayload().(type) {
	case *memqlv1.MemqlClientMessage_ExecuteQuery:
		requestId := normalizeRequestId(msg.GetMessageId(), payload.ExecuteQuery.GetRequestId())
		if requestId == "" {
			requestId = id.NewShortId()
		}
		// Ensure the forwarded message always has a requestId set so server responses
		// correlate to the ID we track locally.
		if strings.TrimSpace(payload.ExecuteQuery.GetRequestId()) == "" {
			payload.ExecuteQuery.RequestId = requestId
		}

		// If this request is already pending or in-flight, ignore duplicates.
		if !s.markPending(requestId) {
			s.logDebug("memql ws execute ignored (already pending or in-flight)", "request_id", requestId)
			return nil
		}

		qe := &queuedExecute{requestId: requestId, msg: msg}
		select {
		case s.pending <- qe:
			s.logDebug("memql ws execute queued", "request_id", requestId)
			return nil
		default:
			// Queue is full: reject with a local error (and release pending marker).
			s.unmarkPending(requestId)
			s.maybeLogOverload("memql ws overload: pending queue full", "request_id", requestId, "pending_cap", cap(s.pending))
			// Queue local errors so we don't spam the socket under a request storm.
			s.queueLocalError(requestId, codes.ResourceExhausted, "too many concurrent MemQL queries")
			return nil
		}
	case *memqlv1.MemqlClientMessage_CancelRequest:
		requestId := strings.TrimSpace(payload.CancelRequest.GetRequestId())
		if requestId != "" {
			s.markCancelled(requestId)
			if s.releaseRequest(requestId) {
				s.releaseToken()
			}
			s.logDebug("memql ws cancel forwarded", "request_id", requestId)
		}
		// Forward cancellation to the MemQL stream as well.
		return s.stream.Send(msg)
	}
	return s.stream.Send(msg)
}

func (s *session) handleServerMessage(msg *memqlv1.MemqlServerMessage) {
	if msg == nil {
		return
	}
	switch payload := msg.GetPayload().(type) {
	case *memqlv1.MemqlServerMessage_QueryResult:
		if payload.QueryResult.GetDone() {
			if s.releaseRequest(strings.TrimSpace(payload.QueryResult.GetRequestId())) {
				s.releaseToken()
			}
			s.logDebug("memql ws query complete", "request_id", payload.QueryResult.GetRequestId())
		}
	case *memqlv1.MemqlServerMessage_QueryError:
		if s.releaseRequest(strings.TrimSpace(payload.QueryError.GetRequestId())) {
			s.releaseToken()
		}
		s.logDebug("memql ws query errored", "request_id", payload.QueryError.GetRequestId())
	}
}

func (s *session) dispatchLoop() error {
	for {
		select {
		case <-s.ctx.Done():
			return s.ctx.Err()
		case qe := <-s.pending:
			if qe == nil {
				continue
			}

			// No longer pending.
			s.unmarkPending(qe.requestId)

			// If cancelled while queued, skip dispatching.
			if s.isCancelled(qe.requestId) {
				s.unmarkCancelled(qe.requestId)
				continue
			}

			// Acquire an in-flight token before sending to gRPC.
			select {
			case s.tokens <- struct{}{}:
			case <-s.ctx.Done():
				return s.ctx.Err()
			}

			// If cancelled after acquiring a token, release and skip.
			if s.isCancelled(qe.requestId) {
				s.unmarkCancelled(qe.requestId)
				s.releaseToken()
				continue
			}

			// Mark request as in-flight.
			if !s.beginInFlight(qe.requestId) {
				// Duplicate in-flight; release token and skip.
				s.releaseToken()
				continue
			}

			if err := s.stream.Send(qe.msg); err != nil {
				// Best-effort cleanup so tokens don't leak. Clear the
				// in-flight record (side effect) then release the token
				// unconditionally -- both former branches did the same.
				s.releaseRequest(qe.requestId)
				s.releaseToken()
				return err
			}
			s.logDebug("memql ws execute dispatched", "request_id", qe.requestId)
		}
	}
}

func (s *session) releaseRequest(requestId string) bool {
	if requestId == "" {
		return false
	}
	s.activeMu.Lock()
	_, existed := s.active[requestId]
	delete(s.active, requestId)
	s.activeMu.Unlock()
	return existed
}

func (s *session) sendLocalError(requestId string, code codes.Code, message string) error {
	errMsg := &memqlv1.MemqlServerMessage{
		MessageId: id.NewShortId(),
		Payload: &memqlv1.MemqlServerMessage_QueryError{
			QueryError: &memqlv1.QueryErrorMsg{
				RequestId: requestId,
				Error: &memqlv1.QueryError{
					Code:    code.String(),
					Message: message,
				},
			},
		},
	}
	payload, err := protojson.MarshalOptions{EmitUnpopulated: true}.Marshal(errMsg)
	if err != nil {
		return err
	}
	s.logDebug("memql ws local error", "request_id", requestId, "code", code.String())
	return s.writeMessage(payload)
}

func (s *session) logDebug(msg string, args ...any) {
	if s.logger == nil {
		return
	}
	if s.principal != "" {
		args = append(args, "principal", s.principal)
	}
	s.logger.Debug(msg, args...)
}

func (s *session) markPending(requestId string) bool {
	requestId = strings.TrimSpace(requestId)
	if requestId == "" {
		return true
	}
	s.activeMu.Lock()
	_, inFlight := s.active[requestId]
	s.activeMu.Unlock()
	if inFlight {
		return false
	}
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	if _, exists := s.pendingSet[requestId]; exists {
		return false
	}
	s.pendingSet[requestId] = struct{}{}
	return true
}

func (s *session) unmarkPending(requestId string) {
	requestId = strings.TrimSpace(requestId)
	if requestId == "" {
		return
	}
	s.pendingMu.Lock()
	delete(s.pendingSet, requestId)
	s.pendingMu.Unlock()
}

func (s *session) beginInFlight(requestId string) bool {
	requestId = strings.TrimSpace(requestId)
	if requestId == "" {
		return true
	}
	s.activeMu.Lock()
	defer s.activeMu.Unlock()
	if _, exists := s.active[requestId]; exists {
		return false
	}
	// Enforce maxActive defensively (tokens should already handle this).
	if len(s.active) >= s.maxActive {
		return false
	}
	s.active[requestId] = struct{}{}
	return true
}

func (s *session) releaseToken() {
	select {
	case <-s.tokens:
	default:
	}
}

func (s *session) markCancelled(requestId string) {
	requestId = strings.TrimSpace(requestId)
	if requestId == "" {
		return
	}
	s.cancelledMu.Lock()
	s.cancelledSet[requestId] = struct{}{}
	s.cancelledMu.Unlock()
}

func (s *session) isCancelled(requestId string) bool {
	requestId = strings.TrimSpace(requestId)
	if requestId == "" {
		return false
	}
	s.cancelledMu.Lock()
	_, ok := s.cancelledSet[requestId]
	s.cancelledMu.Unlock()
	return ok
}

func (s *session) unmarkCancelled(requestId string) {
	requestId = strings.TrimSpace(requestId)
	if requestId == "" {
		return
	}
	s.cancelledMu.Lock()
	delete(s.cancelledSet, requestId)
	s.cancelledMu.Unlock()
}

func (s *session) maybeLogOverload(msg string, args ...any) {
	s.overloadMu.Lock()
	defer s.overloadMu.Unlock()
	now := time.Now()
	if !s.lastOverload.IsZero() && now.Sub(s.lastOverload) < defaultOverloadLogInterval {
		return
	}
	s.lastOverload = now
	s.logDebug(msg, args...)
}

func (s *session) queueLocalError(requestId string, code codes.Code, message string) {
	if s == nil {
		return
	}
	item := &queuedLocalError{
		requestId: strings.TrimSpace(requestId),
		code:      code,
		message:   message,
	}
	select {
	case s.localErrors <- item:
	default:
		// If the local error queue is full, fall back to immediate send.
		_ = s.sendLocalError(item.requestId, item.code, item.message)
	}
}

func (s *session) localErrorLoop() {
	ticker := time.NewTicker(defaultLocalErrorInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case item := <-s.localErrors:
			if item == nil {
				continue
			}
			// Pace local errors so we don't overwhelm the connection or client.
			select {
			case <-s.ctx.Done():
				return
			case <-ticker.C:
			}
			_ = s.sendLocalError(item.requestId, item.code, item.message)
		}
	}
}

// pingLoop sends periodic WebSocket pings to keep the connection alive.
// This prevents Koyeb's edge/load-balancer from closing idle connections.
func (s *session) pingLoop() error {
	if s.pingInterval <= 0 {
		// Ping disabled, block until context is done
		<-s.ctx.Done()
		return s.ctx.Err()
	}

	ticker := time.NewTicker(s.pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return s.ctx.Err()
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
			err := s.conn.Ping(ctx)
			cancel()
			if err != nil {
				return fmt.Errorf("ping failed: %w", err)
			}
		}
	}
}

func metadataFromRequest(r *http.Request) metadata.MD {
	md := metadata.MD{}
	if r == nil {
		return md
	}
	// Check Authorization header first
	if auth := strings.TrimSpace(r.Header.Get("Authorization")); auth != "" {
		md.Append("authorization", auth)
	} else if bearer := strings.TrimSpace(r.URL.Query().Get("bearer_token")); bearer != "" {
		// Browsers can't set custom headers during the WebSocket upgrade,
		// so the SDK piggybacks the bearer JWT as ?bearer_token=.
		md.Append("authorization", "Bearer "+bearer)
	} else if token := strings.TrimSpace(r.URL.Query().Get("token")); token != "" {
		// Legacy alias for ?bearer_token=.
		md.Append("authorization", "Bearer "+token)
	} else if guest := strings.TrimSpace(r.URL.Query().Get("guest_token")); guest != "" {
		// Guest invite flow: browsers opening /join/<token> send the
		// invitation token as ?guest_token=<token> since WebSocket
		// upgrades can't carry custom headers. The guest-aware stream
		// interceptor validates it against the invitation store.
		md.Append("authorization", "Guest "+guest)
	}
	return md
}

func normalizeRequestId(messageId, requestId string) string {
	if trimmed := strings.TrimSpace(requestId); trimmed != "" {
		return trimmed
	}
	return strings.TrimSpace(messageId)
}

// marshalServerMessage serializes a server message with omission semantics for QueryResult.
// For QueryResult messages, empty/nil fields are omitted from the JSON output.
// For other message types (ServerHello, QueryError, etc.), default protojson marshaling
// with EmitUnpopulated is used to maintain compatibility.
func marshalServerMessage(msg *memqlv1.MemqlServerMessage) ([]byte, error) {
	if msg == nil {
		return []byte("{}"), nil
	}

	// Check if this is a QueryResult message - if so, apply omission semantics
	if qr, ok := msg.GetPayload().(*memqlv1.MemqlServerMessage_QueryResult); ok && qr.QueryResult != nil {
		return marshalQueryResultMessage(msg, qr.QueryResult)
	}

	// For all other message types, use default protojson marshaling with EmitUnpopulated
	// to maintain compatibility with existing clients
	return protojson.MarshalOptions{EmitUnpopulated: true}.Marshal(msg)
}

// marshalQueryResultMessage serializes a QueryResult message with omission semantics.
func marshalQueryResultMessage(msg *memqlv1.MemqlServerMessage, chunk *memqlv1.QueryResultChunk) ([]byte, error) {
	output := make(map[string]any)

	// Always include messageId
	output["messageId"] = msg.GetMessageId()

	// Include correlateTo if non-empty
	if correlate := msg.GetCorrelateTo(); correlate != "" {
		output["correlateTo"] = correlate
	}

	// Build queryResult with omission semantics
	queryResult := make(map[string]any)

	if requestId := chunk.GetRequestId(); requestId != "" {
		queryResult["requestId"] = requestId
	}

	// Result: only include if present
	if result := chunk.GetResult(); result != nil {
		resultMap := make(map[string]any)

		// Bundle: only include if present (not nil)
		if bundle := result.GetBundle(); bundle != nil {
			bundleMap := make(map[string]any)

			// Only add non-empty arrays
			if nodes := bundle.GetNodes(); len(nodes) > 0 {
				nodeList := make([]any, 0, len(nodes))
				for _, n := range nodes {
					if n != nil {
						nodeData, err := protojson.MarshalOptions{}.Marshal(n)
						if err == nil {
							var nodeMap map[string]any
							if json.Unmarshal(nodeData, &nodeMap) == nil {
								nodeList = append(nodeList, nodeMap)
							}
						}
					}
				}
				if len(nodeList) > 0 {
					bundleMap["nodes"] = nodeList
				}
			}

			if edges := bundle.GetEdges(); len(edges) > 0 {
				edgeList := make([]any, 0, len(edges))
				for _, e := range edges {
					if e != nil {
						edgeData, err := protojson.MarshalOptions{}.Marshal(e)
						if err == nil {
							var edgeMap map[string]any
							if json.Unmarshal(edgeData, &edgeMap) == nil {
								edgeList = append(edgeList, edgeMap)
							}
						}
					}
				}
				if len(edgeList) > 0 {
					bundleMap["edges"] = edgeList
				}
			}

			if rootIds := bundle.GetRootIds(); len(rootIds) > 0 {
				bundleMap["rootIds"] = rootIds
			}

			// Only add bundle if it has content
			if len(bundleMap) > 0 {
				resultMap["bundle"] = bundleMap
			}
		}

		// Data: only include if non-empty
		if data := result.GetData(); len(data) > 0 {
			dataList := make([]any, 0, len(data))
			for _, v := range data {
				if v != nil {
					dataList = append(dataList, v.AsInterface())
				}
			}
			if len(dataList) > 0 {
				resultMap["data"] = dataList
			}
		}

		// Meta: include if present in the result
		if meta := result.GetMeta(); meta != nil {
			metaMap := make(map[string]any)
			if meta.Count != nil {
				metaMap["count"] = *meta.Count
			}
			if meta.HasMore {
				metaMap["hasMore"] = true
			}
			if meta.Cursor != "" {
				metaMap["cursor"] = meta.Cursor
			}
			if meta.TookMs > 0 {
				metaMap["took"] = meta.TookMs
			}
			if meta.ClientId != "" {
				metaMap["clientId"] = meta.ClientId
			}
			if meta.ServerId != "" {
				metaMap["serverId"] = meta.ServerId
			}
			if meta.Version > 0 {
				metaMap["version"] = meta.Version
			}
			if len(metaMap) > 0 {
				resultMap["meta"] = metaMap
			}
		}

		// Only add result if it has content
		if len(resultMap) > 0 {
			queryResult["result"] = resultMap
		}
	}

	queryResult["done"] = chunk.GetDone()

	output["queryResult"] = queryResult

	return json.Marshal(output)
}

func firstDuration(input, fallback time.Duration) time.Duration {
	if input <= 0 {
		return fallback
	}
	return input
}

func firstPositiveInt(input, fallback int) int {
	if input <= 0 {
		return fallback
	}
	return input
}

func firstPositiveInt64(input, fallback int64) int64 {
	if input <= 0 {
		return fallback
	}
	return input
}
