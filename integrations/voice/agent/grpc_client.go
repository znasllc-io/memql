package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// grpc_client.go is the Go analog of the Python voice-agent's grpc_client.py.
// It owns one bidirectional MemqlService.Stream connection for a voice-agent
// session: send-side methods enqueue MemqlClientMessage envelopes onto a
// shared channel drained by the write loop; the read loop fans incoming
// server messages out to per-(correlate_to) waiters so a caller can await a
// specific reply by message id, and dispatches unsolicited server pushes
// (e.g. VoiceAgentSpeak) to registered handlers.
//
// Authentication is via metadata header
// `authorization: Bearer <voice_agent_token>`. The token is an
// identity-issued class="voice_agent" JWT; the memQL voice-agent stream
// interceptor (component/grpc/voice_agent_stream_interceptor.go) verifies it
// via the cluster JWKS and admits the resulting identity to the VoiceAgent*
// message surface only.

// sendQueueDepth bounds the send channel, matching the Python agent's
// maxsize=256. Backpressure here means the write loop or the wire has
// stalled; a blocked send surfaces as a slow caller rather than unbounded
// memory growth.
const sendQueueDepth = 256

// streamReplyDepth bounds a per-StreamRequest reply channel. A single
// turn produces a small, bounded run of VoiceAgentTurnDelta messages plus
// one terminal VoiceAgentTurnComplete; the buffer keeps the read loop from
// blocking on a momentarily-slow consumer without risking unbounded growth.
const streamReplyDepth = 16

// ErrClientClosed is returned by send paths after the client is closed.
var ErrClientClosed = errors.New("voice-agent gRPC client is closed")

// streamConn is the minimal bidirectional-stream surface the Client drives.
// It is satisfied by grpc.BidiStreamingClient[MemqlClientMessage,
// MemqlServerMessage] (the generated MemqlService.Stream client) and is an
// interface so tests can inject a fake stream without a real connection.
type streamConn interface {
	Send(*memqlv1.MemqlClientMessage) error
	Recv() (*memqlv1.MemqlServerMessage, error)
	CloseSend() error
}

// Dialer opens a MemqlService.Stream against addr, presenting the
// voice-agent bearer as metadata, and returns the bidi stream plus a
// teardown func that closes the underlying connection. Overridable in tests.
type Dialer func(ctx context.Context, addr, token string) (streamConn, func(), error)

// PushHandler handles an unsolicited (no correlate_to) server message,
// dispatched by its payload oneof name (e.g. "VoiceAgentSpeak"). It is
// invoked on the read-loop goroutine, so it must be fast or schedule its own
// work; a returned error is logged and does not tear down the stream.
type PushHandler func(*memqlv1.MemqlServerMessage) error

// Client is an async wrapper around the bidirectional MemqlService.Stream RPC.
type Client struct {
	addr   string
	token  string
	dial   Dialer
	logger *slog.Logger

	mu            sync.Mutex
	stream        streamConn
	closeConn     func()
	sendCh        chan *memqlv1.MemqlClientMessage
	waiters       map[string]chan *memqlv1.MemqlServerMessage
	streamWaiters map[string]chan *memqlv1.MemqlServerMessage
	pushHandlers  map[string]PushHandler
	closed        bool
	readErr       error
	done          chan struct{}
	writeStopped  chan struct{}
}

// NewClient builds a Client for addr authenticating with token. A nil dialer
// uses the default insecure gRPC dialer (the cluster runs memQL over plain
// in-network gRPC; TLS is terminated at the entry point). A nil logger
// disables logging.
func NewClient(addr, token string, dial Dialer, logger *slog.Logger) *Client {
	if dial == nil {
		dial = defaultDialer
	}
	return &Client{
		addr:          addr,
		token:         token,
		dial:          dial,
		logger:        logger,
		sendCh:        make(chan *memqlv1.MemqlClientMessage, sendQueueDepth),
		waiters:       make(map[string]chan *memqlv1.MemqlServerMessage),
		streamWaiters: make(map[string]chan *memqlv1.MemqlServerMessage),
		pushHandlers:  make(map[string]PushHandler),
		done:          make(chan struct{}),
		writeStopped:  make(chan struct{}),
	}
}

// SetPushHandler registers an unsolicited-message handler keyed by the
// MemqlServerMessage payload oneof name (e.g. "VoiceAgentSpeak", as returned
// by ServerPayloadName). Server-pushed messages arrive with no correlate_to,
// so the per-request waiter map cannot dispatch them; the read loop routes
// them here. Unmatched pushes fall through to a debug log. Must be called
// before Connect to avoid racing the read loop.
func (c *Client) SetPushHandler(payloadName string, h PushHandler) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pushHandlers[payloadName] = h
}

// Connect opens the gRPC stream and starts the read + write loops. Idempotent.
func (c *Client) Connect(ctx context.Context) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClientClosed
	}
	if c.stream != nil {
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()

	if c.logger != nil {
		c.logger.Info("voice-agent connecting to memql gRPC", "addr", c.addr)
	}
	stream, closeConn, err := c.dial(ctx, c.addr, c.token)
	if err != nil {
		return fmt.Errorf("voice-agent gRPC connect %s: %w", c.addr, err)
	}

	c.mu.Lock()
	c.stream = stream
	c.closeConn = closeConn
	c.mu.Unlock()

	go c.writeLoop()
	go c.readLoop()
	return nil
}

// Close flushes any already-queued sends to the wire, then tears down the
// stream + connection and fails any pending waiters. Flushing on Close lets a
// best-effort final message (e.g. VoiceAgentSessionEnd enqueued just before
// teardown) reach the server rather than being dropped by a teardown race.
// Idempotent and safe to call concurrently.
func (c *Client) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	closeConn := c.closeConn
	stream := c.stream
	waiters := c.waiters
	streamWaiters := c.streamWaiters
	c.waiters = make(map[string]chan *memqlv1.MemqlServerMessage)
	c.streamWaiters = make(map[string]chan *memqlv1.MemqlServerMessage)
	c.mu.Unlock()

	// Stop the write loop, then flush whatever it had not yet drained. The
	// write loop's select can race close(done) against a buffered send, so we
	// own the final drain here to make the flush deterministic.
	close(c.done)
	if stream != nil {
		// Wait for the write loop to observe done and exit so the flush owns
		// the send channel exclusively. writeStopped is only closed after the
		// loop is started; if Connect never ran, stream is nil and we skip.
		<-c.writeStopped
		c.flushPending(stream)
		_ = stream.CloseSend()
	}
	if closeConn != nil {
		closeConn()
	}
	// Fail pending waiters so awaiting callers unblock rather than hang.
	for _, ch := range waiters {
		close(ch)
	}
	// Close stream waiters too so a caller ranging over a StreamRequest
	// channel unblocks on teardown rather than hanging.
	for _, ch := range streamWaiters {
		close(ch)
	}
}

// flushPending drains any envelopes still buffered on the send channel to the
// stream. Called from Close after the write loop has been signalled to stop.
func (c *Client) flushPending(stream streamConn) {
	for {
		select {
		case msg := <-c.sendCh:
			if msg == nil {
				return
			}
			if err := stream.Send(msg); err != nil {
				if c.logger != nil {
					c.logger.Warn("voice-agent gRPC flush-on-close send failed", "err", err)
				}
				return
			}
		default:
			return
		}
	}
}

// writeLoop drains the send channel into the bidi stream until the client is
// closed or a send fails. It signals writeStopped on exit so Close can flush
// any remaining buffered sends without racing this goroutine.
func (c *Client) writeLoop() {
	defer close(c.writeStopped)
	for {
		select {
		case <-c.done:
			return
		case msg := <-c.sendCh:
			if msg == nil {
				return
			}
			c.mu.Lock()
			stream := c.stream
			c.mu.Unlock()
			if stream == nil {
				return
			}
			if err := stream.Send(msg); err != nil {
				if c.logger != nil {
					c.logger.Error("voice-agent gRPC send failed", "err", err)
				}
				return
			}
		}
	}
}

// readLoop dispatches server messages to per-correlation waiters or push
// handlers until the stream errors or the client closes.
func (c *Client) readLoop() {
	for {
		c.mu.Lock()
		stream := c.stream
		c.mu.Unlock()
		if stream == nil {
			return
		}
		envelope, err := stream.Recv()
		if err != nil {
			c.mu.Lock()
			c.readErr = err
			c.mu.Unlock()
			select {
			case <-c.done:
			default:
				if c.logger != nil {
					c.logger.Error("voice-agent gRPC read loop ended", "err", err)
				}
			}
			return
		}
		c.dispatch(envelope)
	}
}

// dispatch routes one server message: a correlated reply hands off to the
// matching waiter; an unsolicited message routes by payload name to a push
// handler, falling through to a debug log when none is registered.
func (c *Client) dispatch(envelope *memqlv1.MemqlServerMessage) {
	correlate := envelope.GetCorrelateTo()

	c.mu.Lock()
	waiter, ok := c.waiters[correlate]
	if ok {
		delete(c.waiters, correlate)
	}
	c.mu.Unlock()

	if ok {
		// Non-blocking: the buffered waiter channel always has room for the
		// single reply. A stale double-reply (shouldn't happen) is dropped.
		select {
		case waiter <- envelope:
		default:
		}
		close(waiter)
		return
	}

	// A streaming waiter receives EVERY message correlated to its id and
	// stays registered until the caller releases it (a turn produces a run
	// of TurnDelta + a terminal TurnComplete). Do not close it here.
	c.mu.Lock()
	streamCh, streamOK := c.streamWaiters[correlate]
	c.mu.Unlock()
	if streamOK {
		select {
		case streamCh <- envelope:
		default:
			if c.logger != nil {
				c.logger.Warn("voice-agent stream reply channel full; dropping",
					"correlate_to", correlate, "payload", ServerPayloadName(envelope))
			}
		}
		return
	}

	payloadName := ServerPayloadName(envelope)
	c.mu.Lock()
	h := c.pushHandlers[payloadName]
	c.mu.Unlock()
	if h != nil {
		if err := h(envelope); err != nil && c.logger != nil {
			c.logger.Error("voice-agent push handler failed", "payload", payloadName, "err", err)
		}
		return
	}
	if c.logger != nil {
		c.logger.Debug("voice-agent unawaited server message",
			"correlate_to", correlate, "payload", payloadName)
	}
}

// SendRequest sends a one-shot request and awaits the single matching reply
// (correlated by a generated message id). The caller supplies an envelope
// with its Payload oneof set (e.g.
// &memqlv1.MemqlClientMessage{Payload: &memqlv1.MemqlClientMessage_VoiceAgentSessionStart{...}});
// SendRequest stamps the MessageId, overwriting any caller-set value so
// correlation is always under its control. It returns when the reply lands,
// ctx is done, or the client closes.
func (c *Client) SendRequest(ctx context.Context, envelope *memqlv1.MemqlClientMessage) (*memqlv1.MemqlServerMessage, error) {
	if envelope == nil {
		return nil, fmt.Errorf("voice-agent SendRequest: nil envelope")
	}
	// #1426: every one-shot request/reply round-trip on the voice path is a
	// memQL (and usually DB) call -- time them all at this choke point so the
	// per-call breakdown (SessionStart, ListTools, CallTool, RealtimeOutput,
	// ...) is greppable via voice_timing without instrumenting each caller.
	start := time.Now()
	messageID := uuid.NewString()
	envelope.MessageId = messageID

	waiter := make(chan *memqlv1.MemqlServerMessage, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClientClosed
	}
	c.waiters[messageID] = waiter
	c.mu.Unlock()

	if err := c.enqueue(ctx, envelope); err != nil {
		c.mu.Lock()
		delete(c.waiters, messageID)
		c.mu.Unlock()
		return nil, err
	}

	select {
	case reply, ok := <-waiter:
		if !ok {
			return nil, c.closeReason()
		}
		logVoiceTiming(c.logger, "grpc.request", start,
			"payload", ClientPayloadName(envelope), "reply", ServerPayloadName(reply))
		return reply, nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.waiters, messageID)
		c.mu.Unlock()
		return nil, ctx.Err()
	case <-c.done:
		return nil, c.closeReason()
	}
}

// StreamRequest is the multi-reply analog of SendRequest: it sends one
// request and returns a channel that receives EVERY server message
// correlated to the request's generated message id, until the caller
// invokes the returned release func (or the client closes). This is the
// shape the streaming VoiceAgentTurnRequest needs -- the server pushes a
// run of VoiceAgentTurnDelta messages followed by a terminal
// VoiceAgentTurnComplete, all correlated to the TurnRequest's message id
// (see component/grpc/voice_agent_handlers.go::handleVoiceAgentTurnRequest).
// It is the Go analog of the Python grpc_client's send_stream_request /
// release_stream pair.
//
// The caller MUST call release exactly once (deferred) so the per-id
// stream waiter is unregistered. The returned channel is closed when the
// client tears down; the caller decides termination by inspecting the
// payloads it receives (e.g. stop on VoiceAgentTurnComplete).
func (c *Client) StreamRequest(ctx context.Context, envelope *memqlv1.MemqlClientMessage) (<-chan *memqlv1.MemqlServerMessage, func(), error) {
	if envelope == nil {
		return nil, nil, fmt.Errorf("voice-agent StreamRequest: nil envelope")
	}
	messageID := uuid.NewString()
	envelope.MessageId = messageID

	ch := make(chan *memqlv1.MemqlServerMessage, streamReplyDepth)
	release := func() {
		c.mu.Lock()
		if cur, ok := c.streamWaiters[messageID]; ok && cur == ch {
			delete(c.streamWaiters, messageID)
		}
		c.mu.Unlock()
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, nil, ErrClientClosed
	}
	if c.streamWaiters == nil {
		c.streamWaiters = make(map[string]chan *memqlv1.MemqlServerMessage)
	}
	c.streamWaiters[messageID] = ch
	c.mu.Unlock()

	if err := c.enqueue(ctx, envelope); err != nil {
		release()
		return nil, nil, err
	}
	return ch, release, nil
}

// Send fires an envelope without awaiting a reply (e.g. VoiceAgentSessionEnd
// on a best-effort teardown). It stamps a fresh message id for server-side
// correlation/audit, overwriting any caller-set value.
func (c *Client) Send(ctx context.Context, envelope *memqlv1.MemqlClientMessage) error {
	if envelope == nil {
		return fmt.Errorf("voice-agent Send: nil envelope")
	}
	envelope.MessageId = uuid.NewString()
	return c.enqueue(ctx, envelope)
}

// enqueue places an envelope on the send channel, respecting ctx + closure.
func (c *Client) enqueue(ctx context.Context, envelope *memqlv1.MemqlClientMessage) error {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return ErrClientClosed
	}
	select {
	case c.sendCh <- envelope:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return ErrClientClosed
	}
}

// closeReason returns the most specific error explaining why an awaiting call
// unblocked without a reply.
func (c *Client) closeReason() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.readErr != nil {
		return c.readErr
	}
	return ErrClientClosed
}

// ServerPayloadName returns a stable, log-friendly name for a server
// message's payload oneof (e.g. "VoiceAgentSpeak"). Go protobuf has no
// WhichOneof, so we derive the name from the concrete wrapper type. Used for
// push-handler dispatch and diagnostics.
func ServerPayloadName(envelope *memqlv1.MemqlServerMessage) string {
	if envelope == nil {
		return "<nil>"
	}
	payload := envelope.GetPayload()
	if payload == nil {
		return "<empty>"
	}
	full := fmt.Sprintf("%T", payload)
	// e.g. "*memqlv1.MemqlServerMessage_VoiceAgentSpeak" -> "VoiceAgentSpeak".
	full = strings.TrimPrefix(full, "*memqlv1.MemqlServerMessage_")
	full = strings.TrimPrefix(full, "*gen.MemqlServerMessage_")
	return full
}

// ClientPayloadName returns a stable, log-friendly name for a client
// message's payload oneof (e.g. "VoiceAgentSessionStart"), the send-side
// analog of ServerPayloadName. Used by the voice_timing instrumentation
// (#1426) to stamp WHICH memQL call a measured round-trip was.
func ClientPayloadName(envelope *memqlv1.MemqlClientMessage) string {
	if envelope == nil {
		return "<nil>"
	}
	payload := envelope.GetPayload()
	if payload == nil {
		return "<empty>"
	}
	full := fmt.Sprintf("%T", payload)
	// e.g. "*memqlv1.MemqlClientMessage_VoiceAgentSessionStart" -> "VoiceAgentSessionStart".
	full = strings.TrimPrefix(full, "*memqlv1.MemqlClientMessage_")
	full = strings.TrimPrefix(full, "*gen.MemqlClientMessage_")
	return full
}

// defaultDialer opens an insecure gRPC channel and the MemqlService.Stream
// bidi RPC, attaching the voice-agent bearer to the stream metadata.
func defaultDialer(ctx context.Context, addr, token string) (streamConn, func(), error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	md := metadata.Pairs("authorization", "Bearer "+token)
	streamCtx := metadata.NewOutgoingContext(ctx, md)
	client := memqlv1.NewMemqlServiceClient(conn)
	stream, err := client.Stream(streamCtx)
	if err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("open MemqlService.Stream: %w", err)
	}
	return stream, func() { _ = conn.Close() }, nil
}
