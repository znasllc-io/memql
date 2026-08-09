package client

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"google.golang.org/protobuf/reflect/protoreflect"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/core/id"
)

// Dispatcher handles message multiplexing over a single gRPC stream.
// It correlates responses to requests by message_id and routes events
// to subscription channels.
//
// Termination is tracked via TWO separate signals so callers can
// distinguish intentional shutdowns from stream failures:
//
//   - stopCh closes on any termination; used internally to stop Run().
//   - unexpectedCh closes ONLY on a stream Recv error. It does NOT fire
//     when Stop() is called. A monitorConnection-style watcher should
//     listen on Unexpected() so a deliberate caller-initiated close
//     (e.g. switching clusters) doesn't trigger reconnect fallout.
type Dispatcher struct {
	stream memqlv1.MemqlService_StreamClient
	logger *slog.Logger

	mu           sync.Mutex
	pending      map[string]chan *memqlv1.MemqlServerMessage
	streams      map[string]chan *memqlv1.MemqlServerMessage // session-keyed (request_id) streaming listeners
	eventCh      chan *memqlv1.MemqlServerMessage            // uncorrelated messages (events, heartbeats)
	stopCh       chan struct{}                               // closed on any termination
	unexpectedCh chan struct{}                               // closed only on stream error
	sendMu       sync.Mutex                                  // serializes writes to the stream

	// recvErr captures the terminal stream.Recv() error (guarded by
	// recvErrMu). Set once by Run() when the stream dies for a reason
	// other than an intentional Stop(); read by SendAndWait so a caller
	// blocked on a correlated reply gets the REAL cause (e.g. an
	// Unauthenticated status when the server rejects a no-token stream)
	// instead of a generic "stream closed" -- otherwise the auth
	// rejection is masked and callers like the cockpit reachability probe
	// misclassify a reachable-but-needs-auth server as down.
	recvErrMu sync.Mutex
	recvErr   error

	// clientTools holds the registered inbound ClientToolCall
	// handler (memql#174). One handler at a time -- callers route
	// per-tool inside their handler body. nil-handler envelopes are
	// dropped + warned; the server-side agent loop times out per
	// the deadline it ships with each call.
	clientTools *clientToolHandlerRegistry

	// unrouted counts server frames that carried a request_id, matched no
	// pending request and no registered stream, and therefore fell through
	// to the uncorrelated event channel. Keyed by the MemqlServerMessage
	// payload oneof field name ("automation_run_event", "ai_chunk", ...).
	// Guarded by mu. See noteUnrouted for why this exists and why it cannot
	// become log spam.
	unrouted map[string]int
}

// NewDispatcher creates a dispatcher for the given stream.
func NewDispatcher(stream memqlv1.MemqlService_StreamClient, logger *slog.Logger) *Dispatcher {
	return &Dispatcher{
		stream:       stream,
		logger:       logger,
		pending:      make(map[string]chan *memqlv1.MemqlServerMessage),
		streams:      make(map[string]chan *memqlv1.MemqlServerMessage),
		eventCh:      make(chan *memqlv1.MemqlServerMessage, 256),
		stopCh:       make(chan struct{}),
		unexpectedCh: make(chan struct{}),
		clientTools:  &clientToolHandlerRegistry{},
		unrouted:     make(map[string]int),
	}
}

// Run reads messages from the stream and routes them to pending requests
// or the event channel. Call in a goroutine. On exit, closes stopCh
// (any termination) and -- ONLY on an unexpected stream error --
// unexpectedCh. Callers who want to react to lost streams (reconnect,
// mark clusters unreachable, redraw topology as stale) should listen
// on Unexpected(), not Done().
func (d *Dispatcher) Run() {
	// Track whether Stop() was the reason we're exiting. We capture
	// this BEFORE closing stopCh so there's no race: if stopCh was
	// already closed by Stop() when Run() entered the defer, we treat
	// termination as intentional; if Run() hits a Recv error and we
	// ourselves close stopCh in the defer, it was unexpected.
	defer func() {
		wasIntentional := false
		select {
		case <-d.stopCh:
			wasIntentional = true
		default:
			close(d.stopCh)
		}
		if !wasIntentional {
			close(d.unexpectedCh)
		}
		// Close eventCh so `for ev := range d.Events()` consumers
		// terminate when the stream goes away instead of blocking
		// forever (one leaked goroutine per closed connection,
		// memql#1842). This is panic-safe: eventCh has exactly one
		// sender -- the non-blocking `select { case d.eventCh <- msg:
		// default: }` in Run()'s loop above -- and it runs on THIS
		// same goroutine. The loop has already returned by the time
		// this defer fires, so no send can race the close. The defer
		// runs exactly once (Run() is called once per dispatcher), so
		// there's no double-close. Receivers see a closed channel:
		// `range` ends, and `<-ch` returns (zero, false).
		close(d.eventCh)
	}()
	for {
		msg, err := d.stream.Recv()
		if err != nil {
			select {
			case <-d.stopCh:
				return // Clean shutdown
			default:
			}
			// Record the real terminal error BEFORE closing pending
			// channels, so a SendAndWait caller unblocking on the closed
			// channel can surface it (happens-before: this write precedes
			// close(ch), which precedes the receiver observing !ok).
			d.setRecvErr(err)
			if d.logger != nil {
				d.logger.Warn("gRPC stream receive error", "error", err)
			}
			// Close all pending requests with nil.
			d.mu.Lock()
			for id, ch := range d.pending {
				close(ch)
				delete(d.pending, id)
			}
			d.mu.Unlock()
			return
		}

		correlate := msg.GetCorrelateTo()
		if correlate != "" {
			d.mu.Lock()
			ch, ok := d.pending[correlate]
			d.mu.Unlock()
			if ok {
				ch <- msg
				continue
			}
		}

		// Inbound client-tool dispatch (memql#174). The server's
		// agent loop pushes ClientToolCall envelopes when it resolves
		// a tool marked client_execution=true. Hand off to the
		// registered handler (if any); the result ships back as a
		// ClientToolResult correlated by call_id from inside
		// dispatchClientToolCall's goroutine. Always handled here --
		// these are not surfaced on the Events() channel because the
		// dispatch contract is "exactly one handler" rather than the
		// fan-out semantics Events() carries.
		if call := msg.GetClientToolCall(); call != nil {
			d.dispatchClientToolCall(call)
			continue
		}

		// Streaming-session routing: messages on multi-frame protocols
		// (AiTranscribeStream*, streaming chat, agent / voice-agent turns,
		// automation runs) carry a session request_id rather than the
		// per-message correlate_to. Route them to any listener registered
		// via RegisterStream. streamRequestId documents which families
		// qualify and why the rest deliberately do not.
		reqId := streamRequestId(msg)
		if reqId != "" {
			d.mu.Lock()
			ch, ok := d.streams[reqId]
			d.mu.Unlock()
			if ok {
				select {
				case ch <- msg:
				default:
					if d.logger != nil {
						d.logger.Warn("stream listener channel full, dropping message", "request_id", reqId)
					}
				}
				continue
			}
		}

		// Uncorrelated message — route to event channel. Before it goes
		// there, record the ones that were request-scoped: a frame with a
		// request_id landing here is either a late/early stream frame or
		// the memql#3414 omission itself, and until now BOTH were silent.
		d.noteUnrouted(msg, reqId)

		select {
		case d.eventCh <- msg:
		default:
			if d.logger != nil {
				d.logger.Warn("event channel full, dropping message")
			}
		}
	}
}

// Send sends a message on the stream and assigns a message_id if empty.
// Returns the message_id.
func (d *Dispatcher) Send(msg *memqlv1.MemqlClientMessage) (string, error) {
	if msg.MessageId == "" {
		msg.MessageId = id.NewShortId()
	}
	d.sendMu.Lock()
	err := d.stream.Send(msg)
	d.sendMu.Unlock()
	if err != nil {
		return "", fmt.Errorf("send: %w", err)
	}
	return msg.MessageId, nil
}

// SendAndWait sends a message and blocks until a correlated response arrives.
func (d *Dispatcher) SendAndWait(ctx context.Context, msg *memqlv1.MemqlClientMessage) (*memqlv1.MemqlServerMessage, error) {
	if msg.MessageId == "" {
		msg.MessageId = id.NewShortId()
	}

	ch := make(chan *memqlv1.MemqlServerMessage, 1)
	d.mu.Lock()
	d.pending[msg.MessageId] = ch
	d.mu.Unlock()

	defer func() {
		d.mu.Lock()
		delete(d.pending, msg.MessageId)
		d.mu.Unlock()
	}()

	d.sendMu.Lock()
	err := d.stream.Send(msg)
	d.sendMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("send: %w", err)
	}

	select {
	case resp, ok := <-ch:
		if !ok {
			// Surface the real terminal cause (e.g. Unauthenticated /
			// authentication required) so callers can classify it, rather
			// than a generic message that hides an auth rejection.
			if re := d.RecvErr(); re != nil {
				return nil, fmt.Errorf("stream closed: %w", re)
			}
			return nil, fmt.Errorf("stream closed while waiting for response")
		}
		return resp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// setRecvErr records the terminal stream.Recv() error. Called once by
// Run() on an unexpected stream death.
func (d *Dispatcher) setRecvErr(err error) {
	d.recvErrMu.Lock()
	d.recvErr = err
	d.recvErrMu.Unlock()
}

// RecvErr returns the terminal stream error captured by Run(), or nil if
// the stream was closed intentionally (Stop) or is still live.
func (d *Dispatcher) RecvErr() error {
	d.recvErrMu.Lock()
	defer d.recvErrMu.Unlock()
	return d.recvErr
}

// ErrRotateAuthRejected is returned by RotateAuth when the server
// accepted the envelope but refused the new token. The wrapped error
// message includes the server's RotateAuthResult.error code +
// description so callers can distinguish transient ("access_load_failed")
// from terminal ("invalid_token") rejections.
var ErrRotateAuthRejected = errors.New("rotate_auth: server rejected new token")

// RotateAuth swaps the bearer on this stream's session without
// tearing the stream down. Called by the cockpit's background token
// refresher after a successful /auth/refresh round-trip: the on-disk
// credential was rolled forward, and we now ask the server to align
// its in-memory identity with the rolled token.
//
// Contract:
//   - Returns nil if and only if the server replied
//     RotateAuthResult{ok: true}.
//   - Returns ErrRotateAuthRejected (wrapped) if the server replied
//     ok=false. The stream stays open; the caller can fall back to
//     dropping the connection + reconnecting, or just keep using the
//     old bearer until the next natural reconnect.
//   - Returns a transport error if the stream is down or the context
//     deadline fires before the reply lands. Same fallback applies.
func (d *Dispatcher) RotateAuth(ctx context.Context, accessToken string) error {
	if d == nil {
		return errors.New("rotate_auth: nil dispatcher")
	}
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return errors.New("rotate_auth: empty access token")
	}
	resp, err := d.SendAndWait(ctx, &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_RotateAuth{
			RotateAuth: &memqlv1.RotateAuthMsg{AccessToken: accessToken},
		},
	})
	if err != nil {
		return fmt.Errorf("rotate_auth: %w", err)
	}
	result := resp.GetRotateAuthResult()
	if result == nil {
		// Wrong reply shape -- shouldn't happen against an updated
		// server, but a forward-compat client should fail loud rather
		// than silently treat the stream as rotated.
		return fmt.Errorf("rotate_auth: server reply missing RotateAuthResult payload")
	}
	if !result.GetOk() {
		code := result.GetError()
		if code == "" {
			code = "unknown"
		}
		return fmt.Errorf("%w: %s: %s", ErrRotateAuthRejected, code, result.GetErrorDescription())
	}
	return nil
}

// Events returns the channel for uncorrelated server messages (events, heartbeats).
// The channel is shared -- there must be exactly one consumer. Wiring a
// second consumer (e.g. a disconnect watcher) steals events and causes
// silent event loss. Use Done() to observe stream termination instead.
func (d *Dispatcher) Events() <-chan *memqlv1.MemqlServerMessage {
	return d.eventCh
}

// RegisterStream registers a listener for server messages whose
// session request_id matches the supplied key. Returns a channel for
// incoming messages and an unregister function the caller must invoke
// when the session is over.
//
// Used by streaming protocols (AiTranscribeStream*, VoiceAgent*,
// polyphon) where multiple replies share a session id rather than the
// per-message message_id used by SendAndWait. The Dispatcher routes
// matching messages here before falling through to the global event
// channel.
//
// The returned channel is buffered (64). If a listener falls behind,
// further messages for that session are dropped with a logged warning;
// protocol-level recovery is the caller's responsibility.
func (d *Dispatcher) RegisterStream(requestId string) (<-chan *memqlv1.MemqlServerMessage, func()) {
	ch := make(chan *memqlv1.MemqlServerMessage, 64)
	d.mu.Lock()
	d.streams[requestId] = ch
	d.mu.Unlock()
	unregister := func() {
		d.mu.Lock()
		if cur, ok := d.streams[requestId]; ok && cur == ch {
			delete(d.streams, requestId)
		}
		d.mu.Unlock()
	}
	return ch, unregister
}

// streamRequestId returns the session request_id carried by a
// request-scoped server message, or "" when the message is not part of an
// exchange a RegisterStream listener can be parked on.
//
// # The coverage rule (memql#3429)
//
// This table lists exactly the families whose frames a caller can be waiting
// for on RegisterStream -- every family participating in an exchange that the
// correlate_to tier structurally CANNOT serve, because that tier resolves one
// reply and unregisters. Concretely:
//
//   - families the engine emits MORE THAN ONCE for a single request: the
//     deltas and the terminal that closes them; and
//   - QueryErrorMsg, which can end any of those exchanges in place of its
//     normal terminal. A caller parked on a stream whose request failed
//     server-side must see the error, or it waits out its own deadline for a
//     terminal frame that is never coming -- the memql#3414 outcome, reached
//     by a different route. `sendQueryError` is how the transcribe-stream and
//     automation-run handlers report a refusal, so this is a live path, not a
//     hypothetical one.
//
// The rule is deliberately NOT "every payload that carries a request_id". At
// the time of writing 59 of the 64 members of MemqlServerMessage.payload carry
// one and only 11 are routed here; the other 48 are single replies that
// SendAndWait resolves on correlate_to before this function is consulted at
// all. Listing them would assert a streaming contract that does not exist and
// would move routing for any caller who sends the request with Send rather
// than SendAndWait. So "has a request_id" is the wrong signal to generate this
// table from, and the ledger in the test file is the classification instead.
//
// The rule is also NOT "every request_id the server can put on the wire".
// VoiceAgentSpeak carries a server-minted request_id for an exchange no
// caller opened; there is no listener to reunite it with and it belongs on
// Events().
//
// # Why this is enforced rather than merely written down
//
// dispatcher_stream_routing_test.go walks MemqlServerMessage.payload by
// protoreflect and fails when a member is neither routed here nor listed as
// deliberately unrouted. Adding a message family to the proto is therefore a
// red test naming the new family, not silence -- which is the whole point:
// the failure mode this table has is invisibility on both sides of the wire.
//
// memql#3414 was that failure mode. AutomationRunEvent was unlisted, so every
// frame of every run -- accepted, steps, complete -- fell through to the
// uncorrelated event channel while the caller waited on a terminal the server
// had already sent. Nothing logged it, because nothing had gone wrong at
// either end; the routing table in the middle simply did not know the family
// existed. It cost a full investigation cycle and a wrong root cause.
//
// The Go and TS tables state the same rule and list the same families; the TS
// half is streamRequestId in sdk/ts/src/client/wire.ts. They diverged once
// already (TS routed streaming chat, Go did not), which is the second half of
// memql#3429.
func streamRequestId(msg *memqlv1.MemqlServerMessage) string {
	if msg == nil {
		return ""
	}
	switch p := msg.Payload.(type) {
	// Streaming transcription. Delta repeats with the accumulated text;
	// Complete is the terminal.
	case *memqlv1.MemqlServerMessage_AiTranscribeStreamDelta:
		return p.AiTranscribeStreamDelta.GetRequestId()
	case *memqlv1.MemqlServerMessage_AiTranscribeStreamComplete:
		return p.AiTranscribeStreamComplete.GetRequestId()

	// Automation run trace (memql#3310). One RunAutomationMsg produces many
	// AutomationRunEvent frames -- accepted, then a step per step, then
	// exactly one complete. Routed since memql#3414.
	case *memqlv1.MemqlServerMessage_AutomationRunEvent:
		return p.AutomationRunEvent.GetRequestId()

	// Streaming chat. N AiStreamChunk token deltas then the terminal
	// AiChatResult carrying the assembled text (component/grpc/ai_handlers.go
	// handleAiChatStream). The TS SDK has routed this pair since it shipped;
	// the Go SDK did not, and that divergence on one wire protocol is what
	// memql#3429 was filed for. The non-streaming aiChat path is unaffected:
	// it uses SendAndWait, which resolves on correlate_to in the tier above.
	case *memqlv1.MemqlServerMessage_AiChunk:
		return p.AiChunk.GetRequestId()
	case *memqlv1.MemqlServerMessage_AiChatResult:
		return p.AiChatResult.GetRequestId()

	// Agent turn streaming (AgentGenerateTurnMsg). Deltas then one complete.
	case *memqlv1.MemqlServerMessage_AgentGenerateTurnDelta:
		return p.AgentGenerateTurnDelta.GetRequestId()
	case *memqlv1.MemqlServerMessage_AgentGenerateTurnComplete:
		return p.AgentGenerateTurnComplete.GetRequestId()

	// Voice-agent turn streaming (VoiceAgentTurnRequest). Deltas then one
	// complete. The shipped voice-agent runs its own read loop in
	// integrations/voice/agent/grpc_client.go rather than this dispatcher, so
	// nothing changes for it -- this is here so the next SDK consumer of the
	// family does not rediscover memql#3414.
	case *memqlv1.MemqlServerMessage_VoiceAgentTurnDelta:
		return p.VoiceAgentTurnDelta.GetRequestId()
	case *memqlv1.MemqlServerMessage_VoiceAgentTurnComplete:
		return p.VoiceAgentTurnComplete.GetRequestId()

	// Query results. The engine emits exactly one QueryResultChunk per query
	// today, but `done` is a chunked contract and the name says so; the day it
	// emits two, the second must not vanish. Costless for today's callers:
	// executeRaw uses SendAndWait, so the frame is served by correlate_to and
	// never reaches here.
	case *memqlv1.MemqlServerMessage_QueryResult:
		return p.QueryResult.GetRequestId()

	// The error terminal for any of the above. See the coverage rule.
	case *memqlv1.MemqlServerMessage_QueryError:
		return p.QueryError.GetRequestId()
	}
	return ""
}

// payloadFamilyAndRequestId names the message's payload oneof member and
// returns the request_id it carries, if any.
//
// Done by protoreflect rather than a type switch on purpose: this is the
// mechanism that has to notice families NOBODY has written a case for, so it
// cannot itself be a hand-maintained list of families. A message family added
// to the proto tomorrow is described correctly by this function today.
func payloadFamilyAndRequestId(msg *memqlv1.MemqlServerMessage) (family, requestId string) {
	if msg == nil {
		return "", ""
	}
	m := msg.ProtoReflect()
	od := m.Descriptor().Oneofs().ByName("payload")
	if od == nil {
		return "", ""
	}
	fd := m.WhichOneof(od)
	if fd == nil {
		return "", ""
	}
	family = string(fd.Name())
	if fd.Kind() != protoreflect.MessageKind {
		return family, ""
	}
	inner := m.Get(fd).Message()
	rf := inner.Descriptor().Fields().ByName("request_id")
	if rf == nil || rf.Kind() != protoreflect.StringKind {
		return family, ""
	}
	return family, inner.Get(rf).String()
}

// noteUnrouted records a server frame that reached the uncorrelated event
// channel while carrying a request_id.
//
// That is the shape of memql#3414: a whole message family missing from
// streamRequestId, every frame of every run falling through in complete
// silence while the caller waited out its deadline. Nothing on either side of
// the wire is in a position to report it -- the server did its job, and a
// message on the event channel is not an error -- so this is the only place
// the omission can be made visible.
//
// Two cases, at deliberately different levels so the loud one is not drowned
// by the ordinary one:
//
//   - routedId != "": the family IS routed; there was simply no listener for
//     that id. A frame arriving before RegisterStream or after unregister is
//     normal (a cancelled PushToTalk still gets its terminal), so this is
//     Debug plus a counter and nothing more.
//
//   - routedId == "" and the payload carries a request_id: the frame belongs
//     to a request-scoped exchange the routing table does not know about.
//     That is the omission itself, so it is Warn -- but ONCE per payload
//     family per dispatcher. A family that misroutes a million frames
//     produces exactly one line; the total is bounded by the number of oneof
//     members in MemqlServerMessage (64 today) for the lifetime of one
//     connection, and in practice is one.
//
// UnroutedFrames() exposes the per-family totals for both cases, so a test or
// an operator can see the volume the log deliberately does not repeat.
func (d *Dispatcher) noteUnrouted(msg *memqlv1.MemqlServerMessage, routedId string) {
	family, requestId := payloadFamilyAndRequestId(msg)
	if family == "" || requestId == "" {
		// Not request-scoped at all: ServerHello, EventNotification,
		// HeartbeatMsg, RotateAuthResult. The event channel is where these
		// belong and there is nothing to report.
		return
	}

	d.mu.Lock()
	first := d.unrouted[family] == 0
	d.unrouted[family]++
	d.mu.Unlock()

	if d.logger == nil {
		return
	}
	if routedId != "" {
		d.logger.Debug("stream frame had no registered listener; delivered to the event channel",
			"family", family, "request_id", requestId)
		return
	}
	if !first {
		return
	}
	d.logger.Warn("server frame carries a request_id but the SDK routing table does not route its family; "+
		"it was delivered to the uncorrelated event channel, so a caller parked on RegisterStream will not see it. "+
		"If this family is a multi-frame exchange, streamRequestId in sdk/go/client/dispatcher.go needs a case for it "+
		"(memql#3414 / memql#3429). Logged once per family per connection; see Dispatcher.UnroutedFrames for totals.",
		"family", family, "request_id", requestId)
}

// UnroutedFrames returns a snapshot of how many request-scoped server frames
// fell through to the event channel, keyed by payload family. See noteUnrouted
// for what lands here and why the log is quieter than the counter.
func (d *Dispatcher) UnroutedFrames() map[string]int {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[string]int, len(d.unrouted))
	for k, v := range d.unrouted {
		out[k] = v
	}
	return out
}

// Done returns a channel that is closed when the stream terminates --
// either because Stop() was called or because stream.Recv() errored.
// Use this for wait-for-termination cleanup. For disconnect detection
// (reconnect logic, UI "stale" markers) use Unexpected() instead --
// Done() fires on caller-initiated shutdown too.
func (d *Dispatcher) Done() <-chan struct{} {
	return d.stopCh
}

// Unexpected returns a channel that is closed ONLY when the stream
// terminates due to a Recv error (network drop, server crash). A
// caller-initiated Stop() does NOT close this channel. Use this to
// drive reconnect behavior and UI "stream lost" indicators without
// firing them on intentional cluster switches.
func (d *Dispatcher) Unexpected() <-chan struct{} {
	return d.unexpectedCh
}

// Stop signals the dispatcher to shut down.
func (d *Dispatcher) Stop() {
	select {
	case <-d.stopCh:
	default:
		close(d.stopCh)
	}
}
