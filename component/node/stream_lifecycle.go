package node

import (
	"context"
	"encoding/json"
	"fmt"
)

// Stream lifecycle over the durable substrate streaming contract (memql#1266,
// Phase 2 of epic memql#1259). This file MIGRATES the two concrete streaming
// paths -- token streaming (the agent reply token-by-token delta stream) and
// audio streaming (the STT transcription delta stream) -- ONTO the substrate
// streaming primitives in stream_channel.go (StreamPublisher / SubscribeStream).
//
// stream_channel.go gives us the raw ordered/backpressured/replayed chunk
// contract: a stream is a logical key stream:<id>, each chunk a Deliverable
// whose substrate Seq is the per-stream ordering, the durable outbox is replay,
// and the bounded channel + synchronous Publish are the backpressure floor.
//
// What the two real paths need ON TOP of raw chunks is a STREAM LIFECYCLE: a
// stream is not just an opaque chunk sequence, it is
//
//	Start (carries the stream's opening metadata)
//	  -> Delta* (the token / transcript chunks, in order)
//	  -> Complete (the final text + terminal marker)   |  Error  |  Cancel
//
// Both migrated paths share that exact shape:
//
//   - Token streaming:   Start{turn opens} -> Delta{token text} ... ->
//     Complete{final assembled text}. (component/grpc/si_forward.go relays the
//     AiStreamChunk deltas + the terminal AiChatResult / AgentGenerateTurnComplete.)
//   - Audio streaming:   Start{stt session opens} -> Delta{interim transcript,
//     isFinal, confidence} ... -> Complete{final transcript}. (component/grpc/
//     si_transcribe_stream.go: SITranscribeStreamStart/Delta/Complete.)
//
// This layer encodes that lifecycle as typed phases on the chunk Topic and a
// small reserved payload schema, so the producer side calls Start/Delta/
// Complete/Fail and the consumer side reads a typed StreamFrame channel whose
// frames carry the phase. Ordering, exactly-once, backpressure, and replay-
// across-replica-restart are ALL inherited verbatim from the substrate stream
// contract (proven by stream_channel_test.go) -- this layer adds ONLY the
// lifecycle phase + the start/complete metadata envelope, nothing else.
//
// Scope: this is the node-library realization of "migrate token + audio
// streaming onto the streaming contract", at the same altitude the sibling
// request/response RPC migration (memql#1265, substrate_rpc.go) lands at. The
// live grpc-handler cutover (pointing si_forward.go's token relay and
// si_transcribe_stream.go's audio relay at a StreamSession/StreamSink instead of
// the ad-hoc SIForwardResponse mesh push) and the retirement of the superseded
// forwards ride memql#1267, which is gated on #1264 #1265 #1266 together -- the
// same per-path gating used for the RPC pattern.

// StreamPhase is the lifecycle position of a delivered StreamFrame. It is
// carried on the chunk so the consumer can drive a start/delta/complete state
// machine without inspecting payload shape.
type StreamPhase string

const (
	// StreamPhaseStart is the opening frame of a stream. It carries the
	// stream's opening metadata (provider, format, the turn/request descriptor)
	// in its Meta map and no Data. Exactly one Start per stream, first in seq
	// order.
	StreamPhaseStart StreamPhase = "start"
	// StreamPhaseDelta is an incremental chunk: a token of the reply, or an
	// interim transcript. Zero or more, in seq order, between Start and the
	// terminal.
	StreamPhaseDelta StreamPhase = "delta"
	// StreamPhaseComplete is the normal terminal frame. It carries the final
	// assembled text (token streaming) / final transcript (audio streaming) plus
	// any completion metadata, and is marked terminal so the consumer closes.
	StreamPhaseComplete StreamPhase = "complete"
	// StreamPhaseError is the abnormal terminal frame: the stream ended on a
	// provider/transport error. Carries the error string; terminal.
	StreamPhaseError StreamPhase = "error"
	// StreamPhaseCancel is the caller-initiated terminal frame (the client
	// hung up / the turn was pre-empted). Distinguished from Error so the
	// consumer can surface "cancelled" rather than "failed"; terminal.
	StreamPhaseCancel StreamPhase = "cancel"
)

// terminal reports whether a phase ends the stream (the consumer closes its
// output channel after delivering a terminal frame).
func (p StreamPhase) terminal() bool {
	return p == StreamPhaseComplete || p == StreamPhaseError || p == StreamPhaseCancel
}

// Stream lifecycle payload keys. Carried inside the substrate chunk's Data
// payload alongside the raw chunk text, under reserved keys so the lifecycle
// envelope can never collide with the application payload.
const (
	// streamFramePhase carries the StreamPhase string.
	streamFramePhase = "phase"
	// streamFrameMeta carries the Start frame's opening metadata map and the
	// Complete frame's completion metadata map.
	streamFrameMeta = "meta"
)

// StreamFrame is one delivered lifecycle frame as the consumer sees it. It wraps
// the raw StreamChunk's ordering + data with the decoded lifecycle Phase and the
// Start/Complete metadata.
type StreamFrame struct {
	// StreamID is the logical stream id (chat request id / transcribe request id).
	StreamID string
	// Seq is the substrate's per-stream ordering position. The consumer Acks Seq
	// to advance its durable cursor (so a replica restart replays seq > cursor).
	Seq int64
	// Phase is the lifecycle position (start / delta / complete / error / cancel).
	Phase StreamPhase
	// Topic mirrors the producer's logical topic (e.g. "token", "transcript").
	Topic string
	// Data is the chunk's incremental text on a Delta, the final text on a
	// Complete, and empty on Start / Cancel.
	Data string
	// Meta carries the Start frame's opening metadata and the Complete frame's
	// completion metadata. Nil on Delta frames.
	Meta map[string]any
	// Err is the error string on an Error terminal; empty otherwise.
	Err string
	// Terminal is true on the complete / error / cancel frame.
	Terminal bool
}

// StreamSession is the producer-side handle for ONE lifecycle stream, owned by
// the node producing it (the agent node for token streaming; the voice node for
// audio streaming). It wraps a StreamPublisher and drives the Start -> Delta* ->
// terminal lifecycle. Single-producer per stream, exactly like StreamPublisher.
//
// Each method is a synchronous durable Publish, which is the backpressure floor
// (the producer cannot outrun the durable path); the consumer reads in seq order
// and Acks, so a slow consumer back-pressures via the bounded channel rather
// than unbounded buffering.
type StreamSession struct {
	pub   *StreamPublisher
	topic string

	started  bool
	finished bool
}

// NewStreamSession opens a producer-side lifecycle session for streamID over the
// substrate. topic is the logical chunk topic stamped on every frame
// ("token" for the reply token stream, "transcript" for the STT stream).
// originNode is audit-only (never used for addressing).
func NewStreamSession(substrate DeliverySubstrate, streamID, topic, originNode string) *StreamSession {
	return &StreamSession{
		pub:   NewStreamPublisher(substrate, streamID, originNode),
		topic: topic,
	}
}

// Start publishes the opening frame carrying the stream's metadata (the turn /
// request descriptor: provider, format, sample rate, etc.). It must be called
// once, before any Delta. meta may be nil.
func (s *StreamSession) Start(ctx context.Context, meta map[string]any) (int64, error) {
	if s == nil || s.pub == nil {
		return 0, fmt.Errorf("stream session: not configured")
	}
	if s.started {
		return 0, fmt.Errorf("stream session %s: Start called twice", s.pub.StreamID())
	}
	s.started = true
	return s.pub.Append(ctx, s.topic, encodeFrame(StreamPhaseStart, "", meta))
}

// Delta publishes one incremental chunk -- a reply token or an interim
// transcript. Returns the assigned Seq (the consumer's delivery order). Deltas
// before Start or after a terminal are rejected so the lifecycle stays well-formed.
func (s *StreamSession) Delta(ctx context.Context, data string) (int64, error) {
	if s == nil || s.pub == nil {
		return 0, fmt.Errorf("stream session: not configured")
	}
	if !s.started {
		return 0, fmt.Errorf("stream session %s: Delta before Start", s.pub.StreamID())
	}
	if s.finished {
		return 0, fmt.Errorf("stream session %s: Delta after terminal", s.pub.StreamID())
	}
	return s.pub.Append(ctx, s.topic, encodeFrame(StreamPhaseDelta, data, nil))
}

// Complete publishes the normal terminal frame carrying the final assembled text
// (token streaming) / final transcript (audio streaming) and optional completion
// metadata. After Complete the consumer closes its channel.
func (s *StreamSession) Complete(ctx context.Context, finalText string, meta map[string]any) (int64, error) {
	return s.finish(ctx, StreamPhaseComplete, finalText, "", meta)
}

// Fail publishes the abnormal terminal frame carrying an error string. The
// consumer surfaces Err and closes its channel.
func (s *StreamSession) Fail(ctx context.Context, errMsg string) (int64, error) {
	return s.finish(ctx, StreamPhaseError, "", errMsg, nil)
}

// Cancel publishes the caller-initiated terminal frame (client hung up / turn
// pre-empted). The consumer surfaces a cancel terminal and closes its channel.
func (s *StreamSession) Cancel(ctx context.Context) (int64, error) {
	return s.finish(ctx, StreamPhaseCancel, "", "", nil)
}

func (s *StreamSession) finish(ctx context.Context, phase StreamPhase, data, errMsg string, meta map[string]any) (int64, error) {
	if s == nil || s.pub == nil {
		return 0, fmt.Errorf("stream session: not configured")
	}
	if s.finished {
		return 0, fmt.Errorf("stream session %s: already terminal", s.pub.StreamID())
	}
	s.finished = true
	// The terminal frame rides the substrate's terminal chunk: Fail/Error maps to
	// the publisher's Fail (carries the err string + terminal marker); Complete /
	// Cancel map to Close (terminal marker, carries the lifecycle phase + final
	// text in the frame envelope). Either way the substrate marks the chunk
	// terminal so SubscribeStream closes the consumer channel after delivering it.
	if phase == StreamPhaseError {
		return s.pub.Fail(ctx, s.topic, errMsg)
	}
	return s.pub.Close(ctx, s.topic, encodeFrame(phase, data, meta))
}

// StreamID returns the logical stream id this session produces to.
func (s *StreamSession) StreamID() string {
	if s == nil || s.pub == nil {
		return ""
	}
	return s.pub.StreamID()
}

// SubscribeStreamFrames is the consumer-side entry point. The node owning the
// client (the bff replica that holds the user's WebSocket) calls it with the
// stream id + a stable consumerID. It returns an ordered, replayed, deduped
// channel of typed StreamFrame and an Ack func to advance the durable cursor.
// The channel closes after a terminal frame (complete / error / cancel) or on
// ctx cancellation.
//
// Cross-replica guarantee (inherited from the substrate): on (re)connect the
// substrate replays every chunk with seq > cursor in ascending order before
// tailing live, so a stream produced on node A is consumed in order, exactly
// once, by whichever replica owns the client -- even if that replica restarts
// mid-stream and a DIFFERENT replica reusing the same logical consumerID takes
// over. Backpressure: the channel is bounded; a slow client back-pressures the
// substrate tail, which (via the cursor not advancing) bounds the producer's
// effective rate.
func SubscribeStreamFrames(
	ctx context.Context,
	substrate DeliverySubstrate,
	streamID string,
	consumerID string,
) (<-chan StreamFrame, func(seq int64) error, error) {
	in, ack, err := SubscribeStream(ctx, substrate, streamID, consumerID)
	if err != nil {
		return nil, nil, err
	}

	out := make(chan StreamFrame, substrateChannelBuffer)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case chunk, ok := <-in:
				if !ok {
					return
				}
				frame := chunkToFrame(chunk)
				select {
				case <-ctx.Done():
					return
				case out <- frame:
				}
				if frame.Terminal {
					return
				}
			}
		}
	}()
	return out, ack, nil
}

// --- frame <-> chunk envelope ----------------------------------------------

// encodeFrame serializes a lifecycle frame into the substrate chunk's data
// string payload. The substrate chunk's Data is a single string, so the
// lifecycle envelope (phase + optional meta) is carried in a small,
// deterministic wire form: the phase prefixes the payload and the meta rides a
// reserved key. To stay within the existing StreamChunk.Data string contract
// (no schema change to stream_channel.go) the encoding is a compact tagged
// string: "<phase>\x1f<data>" for the common no-meta case, and a JSON object
// only when meta is present. Decoding is total (a malformed frame degrades to a
// delta) so a bad row never stalls the cursor.
func encodeFrame(phase StreamPhase, data string, meta map[string]any) string {
	if len(meta) == 0 {
		// Hot path (every Delta + most Starts/Completes): a single record
		// separator between the phase tag and the data. Cheap, append-friendly,
		// allocation-light -- streaming is volume-sensitive so the common frame
		// stays a string concat, not a JSON marshal.
		return string(phase) + frameSep + data
	}
	return encodeFrameJSON(phase, data, meta)
}

// frameSep is the US (unit separator) control char delimiting the phase tag from
// the payload in the compact encoding. Chosen because it never appears in token
// text / transcript text.
const frameSep = "\x1f"

// chunkToFrame decodes a delivered StreamChunk back into a typed StreamFrame.
// It is total: a chunk whose data does not parse as a lifecycle frame degrades
// to a Delta carrying the raw data, and the substrate's own terminal/err markers
// (StreamChunk.Terminal / .Err) are honored so a terminal is never lost even if
// the envelope is malformed.
func chunkToFrame(c StreamChunk) StreamFrame {
	frame := StreamFrame{
		StreamID: c.StreamID,
		Seq:      c.Seq,
		Topic:    c.Topic,
		Terminal: c.Terminal,
		Err:      c.Err,
	}

	// A substrate-level error terminal (StreamSession.Fail -> StreamPublisher.Fail)
	// carries no frame envelope in Data; honor it directly.
	if c.Err != "" {
		frame.Phase = StreamPhaseError
		frame.Terminal = true
		return frame
	}

	phase, data, meta := decodeFrame(c.Data)
	frame.Phase = phase
	frame.Data = data
	frame.Meta = meta
	// Trust the lifecycle phase for terminality, but never DOWNGRADE a substrate
	// terminal: if the substrate marked the chunk terminal (Close), the stream is
	// over regardless of how the envelope decoded.
	if phase.terminal() || c.Terminal {
		frame.Terminal = true
		if !phase.terminal() {
			// Terminal chunk with a non-terminal/garbled phase -> treat as Complete
			// so the consumer closes cleanly rather than hanging.
			frame.Phase = StreamPhaseComplete
		}
	}
	return frame
}

// decodeFrame parses the compact-or-JSON frame encoding. Total: an unparseable
// payload is returned as a Delta carrying the raw string.
func decodeFrame(payload string) (StreamPhase, string, map[string]any) {
	if payload == "" {
		return StreamPhaseDelta, "", nil
	}
	// JSON form (meta present) is detected by a leading '{'.
	if payload[0] == '{' {
		if phase, data, meta, ok := decodeFrameJSON(payload); ok {
			return phase, data, meta
		}
		// Not actually our JSON envelope (e.g. a token that happens to start with
		// '{') -- fall through to compact / raw handling.
	}
	if i := indexByte(payload, frameSep[0]); i >= 0 {
		phase := StreamPhase(payload[:i])
		data := payload[i+1:]
		if validPhase(phase) {
			return phase, data, nil
		}
	}
	// No recognizable envelope: treat the whole payload as delta data.
	return StreamPhaseDelta, payload, nil
}

func validPhase(p StreamPhase) bool {
	switch p {
	case StreamPhaseStart, StreamPhaseDelta, StreamPhaseComplete, StreamPhaseError, StreamPhaseCancel:
		return true
	}
	return false
}

// indexByte is a tiny strings.IndexByte avoiding an extra import churn; the
// compact decode is on the consumer hot path so it stays allocation-free.
func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// frameEnvelope is the JSON wire form used only for frames that carry meta (the
// Start descriptor + the Complete completion metadata). Deltas never use it (the
// compact encoding keeps the hot path cheap).
type frameEnvelope struct {
	Phase string         `json:"p"`
	Data  string         `json:"d,omitempty"`
	Meta  map[string]any `json:"m,omitempty"`
}

func encodeFrameJSON(phase StreamPhase, data string, meta map[string]any) string {
	b, err := json.Marshal(frameEnvelope{Phase: string(phase), Data: data, Meta: meta})
	if err != nil {
		// Marshal of a string + map[string]any effectively never fails; degrade
		// to the compact no-meta form rather than dropping the frame's phase.
		return string(phase) + frameSep + data
	}
	return string(b)
}

func decodeFrameJSON(payload string) (StreamPhase, string, map[string]any, bool) {
	var env frameEnvelope
	if err := json.Unmarshal([]byte(payload), &env); err != nil {
		return "", "", nil, false
	}
	phase := StreamPhase(env.Phase)
	if !validPhase(phase) {
		return "", "", nil, false
	}
	return phase, env.Data, env.Meta, true
}
