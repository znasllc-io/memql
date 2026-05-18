package client

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/google/uuid"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
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
	eventCh      chan *memqlv1.MemqlServerMessage // uncorrelated messages (events, heartbeats)
	stopCh       chan struct{}                    // closed on any termination
	unexpectedCh chan struct{}                    // closed only on stream error
	sendMu       sync.Mutex                       // serializes writes to the stream

	// partitionMu guards partition. Sent under sendMu, set under
	// partitionMu so the two don't deadlock.
	partitionMu sync.RWMutex
	partition   string // stamped onto every outbound MemqlClientMessage
}

// NewDispatcher creates a dispatcher for the given stream.
func NewDispatcher(stream memqlv1.MemqlService_StreamClient, logger *slog.Logger) *Dispatcher {
	return &Dispatcher{
		stream:       stream,
		logger:       logger,
		pending:      make(map[string]chan *memqlv1.MemqlServerMessage),
		eventCh:      make(chan *memqlv1.MemqlServerMessage, 256),
		stopCh:       make(chan struct{}),
		unexpectedCh: make(chan struct{}),
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
	}()
	for {
		msg, err := d.stream.Recv()
		if err != nil {
			select {
			case <-d.stopCh:
				return // Clean shutdown
			default:
			}
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

		// Uncorrelated message — route to event channel.
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
// Returns the message_id. Auto-stamps msg.Partition with the dispatcher's
// configured partition (set via SetPartition) when the caller didn't
// already populate it -- so every outbound query / mutation / subscribe
// runs against the user's currently-selected partition without each
// call site having to remember.
func (d *Dispatcher) Send(msg *memqlv1.MemqlClientMessage) (string, error) {
	if msg.MessageId == "" {
		msg.MessageId = uuid.NewString()
	}
	if msg.Partition == "" {
		d.partitionMu.RLock()
		msg.Partition = d.partition
		d.partitionMu.RUnlock()
	}
	d.sendMu.Lock()
	err := d.stream.Send(msg)
	d.sendMu.Unlock()
	if err != nil {
		return "", fmt.Errorf("send: %w", err)
	}
	return msg.MessageId, nil
}

// SetPartition configures the partition name auto-stamped on every
// subsequent outbound message. Empty string clears it (server then
// uses its own default of "default"). Safe to call from any goroutine.
func (d *Dispatcher) SetPartition(name string) {
	d.partitionMu.Lock()
	d.partition = name
	d.partitionMu.Unlock()
}

// Partition returns the partition name currently being stamped.
func (d *Dispatcher) Partition() string {
	d.partitionMu.RLock()
	defer d.partitionMu.RUnlock()
	return d.partition
}

// SendAndWait sends a message and blocks until a correlated response arrives.
func (d *Dispatcher) SendAndWait(ctx context.Context, msg *memqlv1.MemqlClientMessage) (*memqlv1.MemqlServerMessage, error) {
	if msg.MessageId == "" {
		msg.MessageId = uuid.NewString()
	}
	// Mirror Send()'s partition-stamping behavior so request/response
	// calls also target the user's selected partition.
	if msg.Partition == "" {
		d.partitionMu.RLock()
		msg.Partition = d.partition
		d.partitionMu.RUnlock()
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
			return nil, fmt.Errorf("stream closed while waiting for response")
		}
		return resp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
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
