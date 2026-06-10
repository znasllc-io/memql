package node

import (
	"context"
	"fmt"
	"strconv"

	"github.com/znasllc-io/memql/component/events"
)

// Streaming over the durable delivery substrate (memql#1266, Phase 2 of epic
// memql#1259). This layer migrates token streaming (agent reply deltas) and
// audio streaming (STT transcription deltas) onto the substrate's streaming
// contract so streamed chunks get the same reliability/ordering guarantees as
// the chat-reply event (memql#1264) and survive replica churn: today a streamed
// chunk pushed to the wrong BFF replica is lost the same way a reply is.
//
// The core insight: a STREAM is exactly a per-key ordered sequence of
// Deliverables. Each stream is addressed by its own logical RoutingKey
// (Kind "stream", ID the stream id); each chunk is a Deliverable whose
// substrate-assigned monotonic Seq provides per-stream ordering, the durable
// outbox provides replay/catch-up on (re)connect, the per-subscription
// eventDedup provides cross-path dedup, and the bounded subscription channel
// plus the synchronous durable Publish provide backpressure. The four substrate
// guarantees (logical addressing, at-least-once + idempotent dedup, per-key
// ordering, replay) are reused verbatim -- this layer only maps chunks <-> the
// Deliverable shape and stamps the terminal marker.

// StreamKind is the RoutingKey.Kind used for stream channels. It is distinct
// from the event kinds (space/session/user) so a stream's per-key seq space and
// cursor never collide with an event subscription's.
const StreamKind = "stream"

// Payload keys carried on a stream chunk Deliverable. Kept small + explicit so
// the durable row's jsonb stays a stable contract across producer/consumer.
const (
	// streamPayloadData carries the opaque chunk bytes (token text, audio
	// transcript delta, etc.) as a base64-free string -- callers serialize
	// their own structure into Data.
	streamPayloadData = "data"
	// streamPayloadTerminal marks the final chunk of a stream. The consumer
	// closes its output channel after delivering a terminal chunk.
	streamPayloadTerminal = "terminal"
	// streamPayloadErr carries a terminal error string (the stream ended
	// abnormally). Mutually exclusive with a normal terminal payload.
	streamPayloadErr = "err"
	// streamPayloadTopic mirrors the chunk's logical topic for observability.
	streamPayloadTopic = "topic"
)

// StreamKey builds the logical routing key for a stream id. The id is the
// caller's stream/request id (e.g. the chat request_id or transcribe
// request_id); it is the SAME on the producing worker and the consuming BFF, so
// "deliver to whoever owns this stream" resolves without naming a node.
func StreamKey(streamID string) RoutingKey {
	return RoutingKey{Kind: StreamKind, ID: streamID}
}

// StreamChunk is one delivered piece of a stream as seen by the consumer. Seq
// is the substrate's per-stream ordering position (strictly increasing,
// gap-tolerant); the consumer Acks Seq to advance its durable cursor. Terminal
// marks the last chunk; Err is set when the stream ended abnormally.
type StreamChunk struct {
	StreamID string
	Seq      int64
	Topic    string
	Data     string
	Terminal bool
	Err      string
}

// StreamPublisher is the worker-side producer. It appends ordered chunks for a
// single stream to the durable substrate; each Append returns the assigned Seq.
// Append is a synchronous durable write, which is the backpressure floor: the
// producer cannot outrun the durable path. A StreamPublisher is single-stream
// and intended to be driven by one producing goroutine; concurrent Append calls
// are serialized by the substrate's per-key seq allocator but may interleave in
// seq order, so a single producer per stream is the contract.
type StreamPublisher struct {
	substrate DeliverySubstrate
	key       RoutingKey
	streamID  string
	origin    string

	// idx is the producer-local chunk index, used only to mint a stable,
	// unique EventID per chunk (streamID:idx) so a retried Append is an
	// idempotent no-op at the substrate (ADR 4.2). It is NOT the ordering
	// position -- the substrate's Seq is.
	idx int64
}

// NewStreamPublisher builds a producer for streamID over the substrate.
// originNode is stamped on each row for audit only (never used for addressing).
func NewStreamPublisher(substrate DeliverySubstrate, streamID, originNode string) *StreamPublisher {
	return &StreamPublisher{
		substrate: substrate,
		key:       StreamKey(streamID),
		streamID:  streamID,
		origin:    originNode,
	}
}

// Append publishes one non-terminal chunk and returns its assigned Seq. The
// returned Seq is the stream-ordering position the consumer will deliver in.
func (p *StreamPublisher) Append(ctx context.Context, topic, data string) (int64, error) {
	return p.publish(ctx, topic, data, false, "")
}

// Close publishes the terminal chunk that ends the stream. data may be empty
// (token streams carry the final text in their preceding chunks); pass it when
// the terminal itself carries a final payload. After Close the consumer closes
// its output channel.
func (p *StreamPublisher) Close(ctx context.Context, topic, data string) (int64, error) {
	return p.publish(ctx, topic, data, true, "")
}

// Fail publishes a terminal error chunk. The consumer surfaces Err and closes
// its output channel. It is the abnormal-end counterpart to Close.
func (p *StreamPublisher) Fail(ctx context.Context, topic, errMsg string) (int64, error) {
	return p.publish(ctx, topic, "", true, errMsg)
}

func (p *StreamPublisher) publish(ctx context.Context, topic, data string, terminal bool, errMsg string) (int64, error) {
	if p == nil || p.substrate == nil {
		return 0, fmt.Errorf("stream publisher: substrate not configured")
	}
	// Stable per-chunk EventID: streamID:idx. Carried byte-identical on the
	// durable row and the mesh fast-path hint so the two paths dedup against
	// each other; a retried Append of the same idx is an idempotent no-op.
	eventID := p.streamID + ":" + strconv.FormatInt(p.idx, 10)
	p.idx++

	payload := map[string]any{
		streamPayloadData:  data,
		streamPayloadTopic: topic,
	}
	if terminal {
		payload[streamPayloadTerminal] = true
	}
	if errMsg != "" {
		payload[streamPayloadErr] = errMsg
	}

	seq, err := p.substrate.Publish(ctx, Deliverable{
		EventID:    eventID,
		Key:        p.key,
		Topic:      topic,
		Kind:       events.KindNodeCreated,
		Payload:    payload,
		OriginNode: p.origin,
	})
	if err != nil {
		return 0, fmt.Errorf("stream publisher: publish chunk %d for %s: %w", p.idx-1, p.streamID, err)
	}
	return seq, nil
}

// StreamID returns the stream id this publisher produces to.
func (p *StreamPublisher) StreamID() string { return p.streamID }

// SubscribeStream is the consumer-side entry point. The BFF replica that owns a
// user's WebSocket calls it with the stream id and a stable consumerID (so a
// reconnect -- even onto a DIFFERENT replica -- replays from the same cursor).
// It returns an ordered, replayed, deduped channel of StreamChunk and an Ack
// func to advance the durable cursor. The channel closes when ctx is cancelled
// OR after a terminal chunk is delivered (whichever comes first).
//
// Replay/catch-up: on (re)connect the substrate replays every chunk with
// seq > cursor in ascending order before tailing live, so no chunk is lost or
// reordered across a replica restart. Backpressure: the returned channel is
// bounded; a slow WS consumer back-pressures the substrate's tail goroutine,
// which back-pressures (via the durable cursor not advancing) the producer's
// effective rate.
func SubscribeStream(
	ctx context.Context,
	substrate DeliverySubstrate,
	streamID string,
	consumerID string,
) (<-chan StreamChunk, func(seq int64) error, error) {
	if substrate == nil {
		return nil, nil, fmt.Errorf("stream consumer: substrate not configured")
	}
	key := StreamKey(streamID)
	in, err := substrate.Subscribe(ctx, key, consumerID)
	if err != nil {
		return nil, nil, fmt.Errorf("stream consumer: subscribe %s: %w", streamID, err)
	}

	out := make(chan StreamChunk, substrateChannelBuffer)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case d, ok := <-in:
				if !ok {
					return // substrate closed (ctx cancelled upstream)
				}
				chunk := deliverableToChunk(streamID, d)
				select {
				case <-ctx.Done():
					return
				case out <- chunk:
				}
				if chunk.Terminal {
					// Stream ended; stop tailing. The caller Acks the
					// terminal seq to mark the stream fully consumed.
					return
				}
			}
		}
	}()

	ack := func(seq int64) error {
		return substrate.Ack(ctx, key, consumerID, seq)
	}
	return out, ack, nil
}

// deliverableToChunk maps a substrate Deliverable back to a StreamChunk. It is
// total: a malformed payload degrades to an empty-data chunk rather than
// dropping the seq (which would stall the cursor).
func deliverableToChunk(streamID string, d Deliverable) StreamChunk {
	chunk := StreamChunk{
		StreamID: streamID,
		Seq:      d.Seq,
		Topic:    d.Topic,
	}
	if d.Payload != nil {
		if s, ok := d.Payload[streamPayloadData].(string); ok {
			chunk.Data = s
		}
		if t, ok := d.Payload[streamPayloadTopic].(string); ok && t != "" {
			chunk.Topic = t
		}
		if term, ok := d.Payload[streamPayloadTerminal].(bool); ok {
			chunk.Terminal = term
		}
		if e, ok := d.Payload[streamPayloadErr].(string); ok && e != "" {
			chunk.Err = e
			chunk.Terminal = true
		}
	}
	return chunk
}
