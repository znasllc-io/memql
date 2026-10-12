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
	"sync/atomic"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/core/grpclimits"
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
	defaultClientId       = "memql-ws"
	defaultSDKName        = "memql"
	defaultSDKVersion     = "websocket"
	defaultDialTimeout    = 5 * time.Second
	defaultWriteTimeout   = 10 * time.Second
	defaultMaxConcurrent  = 4
	defaultMaxPending     = 64
	defaultMaxLocalErrors = 256
	// defaultMaxMessageBytes is the largest request the bridge takes from a
	// browser: the server's own message cap, so that a request the server would
	// accept is a request the bridge passes on. A template save sends a document of
	// up to 2 MiB inside a JSON string, which escaping can inflate several-fold.
	defaultMaxMessageBytes int64 = grpclimits.MaxMessageBytes
	defaultPingInterval          = 15 * time.Second
	defaultPingTimeout           = 10 * time.Second
	// writeTimePerMiB is added to a message's write deadline for each MiB of it:
	// a browser on an ordinary connection takes a large answer in seconds, and the
	// deadline for a small one would end the session before it was done.
	writeTimePerMiB = time.Second
	// idPrefixBytes and idTailBytes are how much of the start and of the end of an
	// oversized request are kept, to find the ids of the request that is being
	// refused.
	idPrefixBytes              = 64 << 10
	idTailBytes                = 4 << 10
	defaultOverloadLogInterval = 1 * time.Second
	defaultLocalErrorInterval  = 50 * time.Millisecond
)

// Options configure the WebSocket bridge.
type Options struct {
	Logger                *slog.Logger
	Endpoint              string
	DialTimeout           time.Duration
	MaxConcurrentRequests int
	// MaxMessageBytes is the largest request taken from a browser (default: the
	// server's own message cap). A request over it fails by itself, with a
	// sentence, and the session carries on; the bridge reads and discards up to
	// twice the limit for that, and closes the connection beyond it.
	MaxMessageBytes int64
	// WriteTimeout is the time a small message is given to be written; a larger
	// one is given a second more for each MiB of it.
	WriteTimeout time.Duration
	PingInterval time.Duration
	// PingTimeout is how long a ping waits for its pong (default 10 s). No ping is
	// sent while a message is being written, nor while one is arriving and bytes
	// of it are still coming (the sender's own: what is written to it does not
	// count).
	PingTimeout time.Duration
	// OriginPatterns is the allow-list of Origin header values that may
	// upgrade to a WebSocket. Patterns are passed verbatim to
	// nhooyr.io/websocket's AcceptOptions; that library matches with
	// shell-style globs (so "https://*.example.com" works). Empty means
	// "fall back to the legacy wildcard". Production deployments should
	// always supply an explicit list (e.g. via MEMQL_WEBSOCKET_ORIGIN_PATTERNS
	// in the bootstrap config) so the upgrade can't be triggered from an
	// arbitrary attacker page. See docs/internal/design/auth-threat-model.md.
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
	pingTimeout           time.Duration
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
		pingTimeout:           firstDuration(opts.PingTimeout, defaultPingTimeout),
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
		// PUBLIC READS (epic memql#4541, D4). The 401 below is what makes a
		// hosted site unable to serve a visitor who has not signed in --
		// site-hosting.md recorded it as the gap. With the cluster opted in,
		// the upgrade proceeds and the stream is admitted as the anonymous
		// actor, pinned to public-tier reads and graph subscriptions.
		//
		// THE ADMISSION DECISION IS NOT MADE HERE. This only stops refusing;
		// the gRPC stream interceptor decides, on the same rule, for every
		// caller -- because this bridge is not the only way to reach that
		// stream, and a bridge that told the stream "this one is anonymous"
		// would have to say so over metadata a direct dialler can forge.
		//
		// With the flag off this branch is unreachable and the refusal is
		// byte-for-byte what it has always been.
		if !auth.PublicReadsEnabled() {
			h.logWarn("memql websocket identity missing", "remote", remoteAddr(r))
			http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
			return
		}
		identity = anonymousWebsocketIdentity()
	}
	// Allow any authenticated user (role check removed to support frontend client apps)

	originPatterns := h.originPatterns
	// Editor workers authenticate through the existing bearer subprotocol.
	// This admission adds no cookie or query-token fallback.
	if origin := r.Header.Get("Origin"); auth.IsWebEditorOrigin(origin) {
		// IsWebEditorOrigin validates the entire HTTPS origin first. The
		// WebSocket library matches hosts, not URLs, in OriginPatterns.
		originPatterns = append(append([]string(nil), originPatterns...), strings.TrimPrefix(origin, "https://"))
	}
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
		// Negotiate the credential-scheme subprotocol back to browser
		// clients dialing new WebSocket(url, ["bearer"|"guest", token])
		// (#2511) -- browsers abort the handshake unless the 101 echoes
		// one of their offered entries. Legacy query-param dials offer no
		// subprotocol and negotiate none, unchanged.
		Subprotocols: auth.WSNegotiableSubprotocols(),
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

	conn.SetReadLimit(readLimit(h.maxMessageBytes))
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
		pingTimeout:  h.pingTimeout,
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

	// The receive limit is the server's send limit, not gRPC's 4 MiB default.
	// A query answer is one message, so an answer past the default is a read
	// error on this stream, and a read error ends the whole WebSocket (see
	// writeLoop): every request in flight on the tab goes with the one that
	// was too big. The server measures what it is about to send against the
	// same number and answers an oversize result with a sentence for that one
	// request, so a message this stream cannot receive is never sent.
	conn, err := grpc.DialContext(dialCtx, h.endpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(grpclimits.MaxMessageBytes)),
		grpc.WithBlock())
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
	pingTimeout  time.Duration
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

	trafficMu   sync.Mutex
	lastTraffic time.Time
	// lastRead is the last time bytes of a message arrived from the peer. Unlike
	// lastTraffic, nothing the bridge writes moves it: it is the peer's progress,
	// which is what says whether a message that is arriving has stalled.
	lastRead time.Time

	// reading and writing count the messages being read from the peer and written
	// to it right now. See messageMoving.
	reading atomic.Int32
	writing atomic.Int32
	// turn lets one message at a time be written. See writeMessage.
	turn turnLock
	// pingTimeout is how long a ping may wait for its pong.
	pingTimeout time.Duration

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
	// correlate and metadata are for an error that names the message it answers.
	correlate string
	metadata  map[string]string
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
		pingTimeout:  cfg.pingTimeout,
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

		msgType, reader, err := s.conn.Reader(s.ctx)
		if err != nil {
			return err
		}
		// A message has started to arrive. It is moving while bytes of it keep
		// coming, and no ping is due meanwhile (see messageMoving).
		s.reading.Add(1)
		data, over, err := readRequest(&progressReader{r: reader, session: s}, s.maxBytes)
		s.reading.Add(-1)
		if err != nil {
			return err
		}
		if msgType != websocket.MessageBinary && msgType != websocket.MessageText {
			continue
		}
		s.noteTraffic()
		if over != nil {
			// A request over the limit fails by itself. Returning from here would
			// end the session, and every other request on the socket with it.
			s.refuseOversizedRequest(msgType, over)
			continue
		}

		clientMsg := &memqlv1.MemqlClientMessage{}
		if msgType == websocket.MessageBinary {
			if err := proto.Unmarshal(data, clientMsg); err != nil {
				return err
			}
		} else {
			// DiscardUnknown makes the JSON branch agree with the binary one a
			// few lines up. proto.Unmarshal already retains an unrecognised
			// field as an unknown and returns no error -- that is protobuf's
			// forward-compatibility contract, and the whole reason adding a
			// field is an ADDITIVE change. protojson's default is the opposite,
			// so one wire decoded in two encodings had two different answers to
			// "may a client be newer than me", and only the encoding every
			// browser and Node client uses said no.
			//
			// The cost was not a rejected message: readLoop returns the decode
			// error, and returning from readLoop ENDS THE SESSION. So an
			// unrecognised field severed the stream, took every in-flight
			// request with it, and reached the client as "stream closed" --
			// naming neither the field nor the skew. A VS Code plugin built
			// past v0.18.0 disconnected on every Run, because it sends
			// AuthoringValidateBundleMsg.origin (memql#3800) and every engine
			// released so far predates that field.
			//
			// Ignoring the field leaves the request to be answered by the
			// engine that actually exists -- for `origin`, the documented
			// dir=="" degrade and a legible per-construct diagnostic, which is
			// a far better answer than a dead connection.
			if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(data, clientMsg); err != nil {
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
	// A message's deadline is for writing it, so it starts when it is the message's
	// turn. The WebSocket library writes one message at a time and a message waits
	// for the writer under the same context its write is limited by, so a refusal
	// queued behind an answer that is still going (which may hold the writer for the
	// answer's own scaled deadline, 42 s for 32 MiB) gave up at its own, shorter
	// one: the error was discarded, and the call it was for never settled. Waiting
	// for the turn ends only with the session, and a write that then stalls is cut
	// by its deadline, which closes the connection where it can be seen.
	if err := s.turn.acquire(s.ctx); err != nil {
		return err
	}
	defer s.turn.release()
	ctx, cancel := context.WithTimeout(s.ctx, writeDeadline(s.writeLimit, len(payload)))
	defer cancel()
	// A message being written is its own sign of life: no ping is due while it
	// is, because a ping waits behind a large frame for the write lock. It ends in
	// the deadline above, or in its own error.
	s.writing.Add(1)
	err := s.conn.Write(ctx, websocket.MessageText, payload)
	s.writing.Add(-1)
	if err == nil {
		s.noteTraffic()
	}
	return err
}

// turnLock is a lock whose waiters give up when their context ends. The zero value
// is ready to use.
type turnLock struct {
	once sync.Once
	ch   chan struct{}
}

func (l *turnLock) acquire(ctx context.Context) error {
	l.once.Do(func() { l.ch = make(chan struct{}, 1) })
	select {
	case l.ch <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *turnLock) release() { <-l.ch }

// writeDeadline is the time allowed to write a message of size bytes: the base,
// which a small message gets, and a second more for each MiB of it.
func writeDeadline(base time.Duration, size int) time.Duration {
	return base + time.Duration(int64(size)*int64(writeTimePerMiB)/(1<<20))
}

// messageMoving says that a message is on its way, in either direction, so that a
// ping would only wait behind it: for the write lock behind a large frame, and for
// a pong that a browser cannot slip into the middle of a large frame it is
// sending. A message being written ends in its own deadline or error. One that is
// arriving counts while bytes of it have come within the last two ping intervals
// (the window a missed ping is forgiven in), so a sender that has started a message
// and stopped is not one the keepalive stops watching. Only the sender's own bytes
// count: what the bridge writes to a peer that has stopped sending (live-feed
// events, answers) is not the peer's progress, and counting it would let a peer
// that died mid-message on a socket still receiving them go unpinged until TCP
// gave up.
func (s *session) messageMoving() bool {
	if s.writing.Load() > 0 {
		return true
	}
	return s.reading.Load() > 0 && s.recentRead(2*s.pingInterval)
}

// progressReader notes the peer's progress as the bytes of a message arrive.
type progressReader struct {
	r       io.Reader
	session *session
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.session.noteRead()
	}
	return n, err
}

func (s *session) pingTimeoutOrDefault() time.Duration {
	if s.pingTimeout > 0 {
		return s.pingTimeout
	}
	return defaultPingTimeout
}

// noteRead records bytes of a message arriving from the peer: traffic, and the
// peer's own progress (see messageMoving).
func (s *session) noteRead() {
	if s == nil {
		return
	}
	now := time.Now()
	s.trafficMu.Lock()
	s.lastTraffic = now
	s.lastRead = now
	s.trafficMu.Unlock()
}

func (s *session) recentRead(within time.Duration) bool {
	if s == nil || within <= 0 {
		return false
	}
	s.trafficMu.Lock()
	last := s.lastRead
	s.trafficMu.Unlock()
	return !last.IsZero() && time.Since(last) <= within
}

func (s *session) noteTraffic() {
	if s == nil {
		return
	}
	s.trafficMu.Lock()
	s.lastTraffic = time.Now()
	s.trafficMu.Unlock()
}

func (s *session) recentTraffic(within time.Duration) bool {
	if s == nil || within <= 0 {
		return false
	}
	s.trafficMu.Lock()
	last := s.lastTraffic
	s.trafficMu.Unlock()
	return !last.IsZero() && time.Since(last) <= within
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
			// The refusal names the message it answers (correlateTo): that id is what
			// the browser SDK finds the request by, and a refusal that carries only
			// the request id reaches its event listeners while the call it was for
			// stays pending.
			s.queueLocalError(requestId, msg.GetMessageId(), codes.ResourceExhausted, "too many concurrent MemQL queries")
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

func (s *session) sendQueuedLocalError(item *queuedLocalError) error {
	errMsg := &memqlv1.MemqlServerMessage{
		MessageId:   id.NewShortId(),
		CorrelateTo: item.correlate,
		Payload: &memqlv1.MemqlServerMessage_QueryError{
			QueryError: &memqlv1.QueryErrorMsg{
				RequestId: item.requestId,
				Error: &memqlv1.QueryError{
					Code:     item.code.String(),
					Message:  item.message,
					Metadata: item.metadata,
				},
			},
		},
	}
	payload, err := protojson.MarshalOptions{EmitUnpopulated: true}.Marshal(errMsg)
	if err != nil {
		return err
	}
	s.logDebug("memql ws local error", "request_id", item.requestId, "code", item.code.String())
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

func (s *session) queueLocalError(requestId, correlate string, code codes.Code, message string) {
	s.queueLocalErrorItem(&queuedLocalError{
		requestId: strings.TrimSpace(requestId),
		correlate: strings.TrimSpace(correlate),
		code:      code,
		message:   message,
	})
}

func (s *session) queueLocalErrorItem(item *queuedLocalError) {
	if s == nil {
		return
	}
	select {
	case s.localErrors <- item:
	default:
		// If the local error queue is full, fall back to immediate send.
		_ = s.sendQueuedLocalError(item)
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
			_ = s.sendQueuedLocalError(item)
		}
	}
}

// pingLoop sends periodic WebSocket pings to keep the connection alive
// through idle proxies and LBs. Cadence is defaultPingInterval (15s): long
// enough not to chatter, short enough that a healthy Ask session stays under
// typical 30–60s idle cuts.
//
// A single missed pong does not kill the session when application frames
// arrived recently -- prod aks saw "ping failed… wait for pong: context
// deadline exceeded" right after a successful Ask answer when a transient
// control-frame stall coincided with heavy payload traffic. Two consecutive
// misses (or one miss with a quiet socket) still end the session so a truly
// half-dead connection reconnects.
func (s *session) pingLoop() error {
	if s.pingInterval <= 0 {
		<-s.ctx.Done()
		return s.ctx.Err()
	}

	ticker := time.NewTicker(s.pingInterval)
	defer ticker.Stop()

	misses := 0
	for {
		select {
		case <-s.ctx.Done():
			return s.ctx.Err()
		case <-ticker.C:
			if s.messageMoving() {
				continue
			}
			ctx, cancel := context.WithTimeout(s.ctx, s.pingTimeoutOrDefault())
			err := s.conn.Ping(ctx)
			cancel()
			if err == nil {
				misses = 0
				continue
			}
			misses++
			if misses == 1 && s.recentTraffic(s.pingInterval*2) {
				s.logDebug("websocket ping miss tolerated; recent application traffic",
					"error", err,
					"misses", misses,
				)
				continue
			}
			return fmt.Errorf("ping failed: %w", err)
		}
	}
}

func metadataFromRequest(r *http.Request) metadata.MD {
	md := metadata.MD{}
	if r == nil {
		return md
	}
	// Check Authorization header first
	if header := strings.TrimSpace(r.Header.Get("Authorization")); header != "" {
		md.Append("authorization", header)
		return md
	}
	// Browser clients carry the credential as the second
	// Sec-WebSocket-Protocol entry (["bearer", token], #2511) -- the header
	// channel that stays out of request-line access logs.
	if scheme, cred := auth.WebSocketSubprotocolCredential(r); cred != "" {
		if scheme == auth.WSCredentialSchemeBearer {
			md.Append("authorization", "Bearer "+cred)
			return md
		}
	}
	// DEPRECATED (#2511): query-param token carry, kept until downstream
	// clients migrate to the subprotocol channel (memql-project#16).
	if bearer := strings.TrimSpace(r.URL.Query().Get("bearer_token")); bearer != "" {
		md.Append("authorization", "Bearer "+bearer)
	} else if token := strings.TrimSpace(r.URL.Query().Get("token")); token != "" {
		// Legacy alias for ?bearer_token=.
		md.Append("authorization", "Bearer "+token)
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

// anonymousWebsocketIdentity is the placeholder UserIdentity a
// public-reads session carries THROUGH THIS HANDLER, for its log lines and
// nothing else (epic memql#4541, D4).
//
// It authorizes nothing. This handler's own use of `identity` is entirely
// observational -- three log calls naming the subject -- and every decision
// that matters happens on the gRPC stream, where the interceptor builds the
// real auth.AnonymousActor from the ABSENCE of a credential rather than
// from anything the bridge passed along. That separation is the point: if
// this value were the input to an authorization decision, the bridge would
// be minting an actor, and a direct gRPC dialler would want the same
// treatment via a header it controls.
//
// The subject is the actor's own constant so a log line joins up with the
// engine's `createdBy` vocabulary rather than inventing a second word for
// the same caller.
func anonymousWebsocketIdentity() auth.UserIdentity {
	return auth.UserIdentity{
		Subject: auth.AnonymousUserId,
		Role:    string(auth.RoleAnonymous),
	}
}
