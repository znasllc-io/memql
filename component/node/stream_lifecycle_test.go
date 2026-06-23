package node

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// drainFrames reads up to want frames (or until a terminal) from a frame
// consumer channel, Acking each, bounded by a deadline. Returns frames in
// delivery order.
func drainFrames(t *testing.T, out <-chan StreamFrame, ack func(int64) error, want int, deadline time.Duration) []StreamFrame {
	t.Helper()
	var got []StreamFrame
	timeout := time.After(deadline)
	for len(got) < want {
		select {
		case f, ok := <-out:
			if !ok {
				return got
			}
			if err := ack(f.Seq); err != nil {
				t.Fatalf("ack seq %d: %v", f.Seq, err)
			}
			got = append(got, f)
			if f.Terminal {
				return got
			}
		case <-timeout:
			t.Fatalf("timed out after %d/%d frames", len(got), want)
		}
	}
	return got
}

// TestStreamLifecycleTokenStream proves the token-streaming path migrated onto
// the substrate: Start (turn metadata) -> Delta* (tokens, in order) ->
// Complete (final assembled text), consumed in seq order with the correct
// lifecycle phases.
func TestStreamLifecycleTokenStream(t *testing.T) {
	t.Parallel()
	sub, _ := newTestStreamSubstrate()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const streamID = "chat-req-token"
	out, ack, err := SubscribeStreamFrames(ctx, sub, streamID, "bff-A")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	sess := NewStreamSession(sub, streamID, "token", "agent-1")
	if _, err := sess.Start(ctx, map[string]any{"partitionId": "space-1", "agentId": "faye"}); err != nil {
		t.Fatalf("start: %v", err)
	}
	tokens := []string{"Hello", ", ", "world", "!"}
	for _, tok := range tokens {
		if _, err := sess.Delta(ctx, tok); err != nil {
			t.Fatalf("delta %q: %v", tok, err)
		}
	}
	if _, err := sess.Complete(ctx, "Hello, world!", map[string]any{"tokens": 4}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	// Start + 4 deltas + complete = 6 frames.
	got := drainFrames(t, out, ack, 6, 5*time.Second)
	if len(got) != 6 {
		t.Fatalf("want 6 frames, got %d", len(got))
	}
	if got[0].Phase != StreamPhaseStart {
		t.Fatalf("frame 0 phase=%q want start", got[0].Phase)
	}
	if got[0].Meta["partitionId"] != "space-1" {
		t.Fatalf("start meta lost: %+v", got[0].Meta)
	}
	for i, tok := range tokens {
		f := got[i+1]
		if f.Phase != StreamPhaseDelta {
			t.Fatalf("frame %d phase=%q want delta", i+1, f.Phase)
		}
		if f.Data != tok {
			t.Fatalf("frame %d data=%q want %q (out of order)", i+1, f.Data, tok)
		}
	}
	final := got[5]
	if final.Phase != StreamPhaseComplete || !final.Terminal {
		t.Fatalf("final phase=%q terminal=%v want complete+terminal", final.Phase, final.Terminal)
	}
	if final.Data != "Hello, world!" {
		t.Fatalf("final text=%q want assembled", final.Data)
	}
	// Strictly increasing seq across the whole lifecycle.
	var last int64
	for i, f := range got {
		if i > 0 && f.Seq <= last {
			t.Fatalf("seq not increasing at %d: %d <= %d", i, f.Seq, last)
		}
		last = f.Seq
	}
}

// TestStreamLifecycleAudioStream proves the audio/transcription path migrated
// onto the substrate: Start (stt session) -> Delta* (interim transcripts) ->
// Complete (final transcript). Same contract, different payload shape -- the
// point of the migration is one ordered/backpressured streaming contract for both.
func TestStreamLifecycleAudioStream(t *testing.T) {
	t.Parallel()
	sub, _ := newTestStreamSubstrate()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const streamID = "transcribe-req-1"
	out, ack, err := SubscribeStreamFrames(ctx, sub, streamID, "bff-A")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	sess := NewStreamSession(sub, streamID, "transcript", "voice-1")
	if _, err := sess.Start(ctx, map[string]any{"provider": "openai-realtime", "sampleRate": 16000}); err != nil {
		t.Fatalf("start: %v", err)
	}
	interims := []string{"the", "the quick", "the quick brown fox"}
	for _, it := range interims {
		if _, err := sess.Delta(ctx, it); err != nil {
			t.Fatalf("delta: %v", err)
		}
	}
	if _, err := sess.Complete(ctx, "the quick brown fox", map[string]any{"durationMs": 1200}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	got := drainFrames(t, out, ack, 5, 5*time.Second)
	if len(got) != 5 {
		t.Fatalf("want 5 frames, got %d", len(got))
	}
	if got[0].Phase != StreamPhaseStart || got[0].Meta["provider"] != "openai-realtime" {
		t.Fatalf("start frame wrong: %+v", got[0])
	}
	for i, it := range interims {
		if got[i+1].Phase != StreamPhaseDelta || got[i+1].Data != it {
			t.Fatalf("interim %d wrong: %+v", i, got[i+1])
		}
	}
	if !got[4].Terminal || got[4].Phase != StreamPhaseComplete || got[4].Data != "the quick brown fox" {
		t.Fatalf("complete frame wrong: %+v", got[4])
	}
}

// TestStreamLifecycleCrossReplicaReplay is the core #1266 guarantee at the
// lifecycle layer: a stream produced on node A is consumed in seq order,
// exactly once, by a consumer on node B -- and survives the consuming replica
// dying mid-stream and a DIFFERENT replica (reusing the same logical
// consumerID) taking over. Durable-store-only (nil fast-path), mirroring #1264's
// cross-replica test style.
func TestStreamLifecycleCrossReplicaReplay(t *testing.T) {
	t.Parallel()
	sub, _ := newTestStreamSubstrate()
	const streamID = "chat-req-replay"
	const consumerID = "ws-owner" // stable logical consumer across replica churn

	// Producer on node A: Start + several deltas up front.
	sess := NewStreamSession(sub, streamID, "token", "agent-A")
	bg := context.Background()
	if _, err := sess.Start(bg, map[string]any{"partitionId": "s1"}); err != nil {
		t.Fatalf("start: %v", err)
	}
	const firstBatch = 6
	for i := 0; i < firstBatch; i++ {
		if _, err := sess.Delta(bg, fmt.Sprintf("t%d", i)); err != nil {
			t.Fatalf("delta %d: %v", i, err)
		}
	}

	// Consumer replica B1 connects, reads Start + first 3 deltas, Acks, then dies.
	ctx1, cancel1 := context.WithCancel(context.Background())
	out1, ack1, err := SubscribeStreamFrames(ctx1, sub, streamID, consumerID)
	if err != nil {
		t.Fatalf("subscribe 1: %v", err)
	}
	first := drainFrames(t, out1, ack1, 4, 5*time.Second) // start + t0,t1,t2
	if len(first) != 4 {
		t.Fatalf("want 4 on first connect, got %d", len(first))
	}
	cancel1() // replica owning the WS dies mid-stream

	// Producer keeps streaming + completes while no consumer is attached.
	for i := firstBatch; i < firstBatch+4; i++ {
		if _, err := sess.Delta(bg, fmt.Sprintf("t%d", i)); err != nil {
			t.Fatalf("delta %d: %v", i, err)
		}
	}
	if _, err := sess.Complete(bg, "final", nil); err != nil {
		t.Fatalf("complete: %v", err)
	}

	// Replica B2 takes over with the SAME logical consumerID; must replay
	// everything after the cursor in order, none lost / reordered / duplicated.
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	out2, ack2, err := SubscribeStreamFrames(ctx2, sub, streamID, consumerID)
	if err != nil {
		t.Fatalf("subscribe 2: %v", err)
	}
	// remaining: t3..t9 (7 deltas) + complete = 8 frames.
	rest := drainFrames(t, out2, ack2, 8, 5*time.Second)

	var all []StreamFrame
	all = append(all, first...)
	all = append(all, rest...)

	// Validate: exactly one Start, t0..t9 in order, one terminal Complete.
	starts, deltas, terminals := 0, 0, 0
	var lastSeq int64
	for i, f := range all {
		if i > 0 && f.Seq <= lastSeq {
			t.Fatalf("seq regressed at %d: %d <= %d", i, f.Seq, lastSeq)
		}
		lastSeq = f.Seq
		switch f.Phase {
		case StreamPhaseStart:
			starts++
		case StreamPhaseDelta:
			want := fmt.Sprintf("t%d", deltas)
			if f.Data != want {
				t.Fatalf("delta %d data=%q want %q (lost/reordered across replica restart)", deltas, f.Data, want)
			}
			deltas++
		case StreamPhaseComplete:
			terminals++
		}
	}
	if starts != 1 {
		t.Fatalf("want exactly 1 start, got %d", starts)
	}
	if deltas != firstBatch+4 {
		t.Fatalf("want %d deltas, got %d", firstBatch+4, deltas)
	}
	if terminals != 1 || !all[len(all)-1].Terminal {
		t.Fatalf("want exactly 1 terminal complete; terminals=%d", terminals)
	}
}

// TestStreamLifecycleBackpressure proves a slow consumer bounds the producer's
// outstanding window rather than unbounded buffering: the producer publishes far
// more than the channel buffer (synchronous durable writes, never blocked on the
// consumer), and a consumer that drains slowly still receives every frame in
// order, none dropped.
func TestStreamLifecycleBackpressure(t *testing.T) {
	t.Parallel()
	sub, _ := newTestStreamSubstrate()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const streamID = "audio-req-bp"
	out, ack, err := SubscribeStreamFrames(ctx, sub, streamID, "voice-consumer")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	sess := NewStreamSession(sub, streamID, "transcript", "voice-1")
	n := substrateChannelBuffer * 3
	produced := make(chan struct{})
	go func() {
		if _, err := sess.Start(ctx, nil); err != nil {
			t.Errorf("start: %v", err)
			return
		}
		for i := 0; i < n; i++ {
			if _, err := sess.Delta(ctx, fmt.Sprintf("d%d", i)); err != nil {
				t.Errorf("delta %d: %v", i, err)
				return
			}
		}
		if _, err := sess.Complete(ctx, "done", nil); err != nil {
			t.Errorf("complete: %v", err)
			return
		}
		close(produced)
	}()

	select {
	case <-produced:
	case <-time.After(5 * time.Second):
		t.Fatal("producer deadlocked under a slow/unattached consumer (no backpressure bound)")
	}

	// start + n deltas + complete.
	got := drainFrames(t, out, ack, n+2, 10*time.Second)
	if len(got) != n+2 {
		t.Fatalf("want %d frames, got %d", n+2, len(got))
	}
	if got[0].Phase != StreamPhaseStart {
		t.Fatalf("frame 0 not start")
	}
	for i := 0; i < n; i++ {
		if got[i+1].Phase != StreamPhaseDelta || got[i+1].Data != fmt.Sprintf("d%d", i) {
			t.Fatalf("delta %d lost/reordered under backpressure: %+v", i, got[i+1])
		}
	}
	if !got[n+1].Terminal {
		t.Fatalf("final frame not terminal")
	}
}

// TestStreamLifecycleError proves Fail surfaces an error terminal and closes the
// consumer channel.
func TestStreamLifecycleError(t *testing.T) {
	t.Parallel()
	sub, _ := newTestStreamSubstrate()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const streamID = "chat-req-fail"
	out, ack, err := SubscribeStreamFrames(ctx, sub, streamID, "bff-A")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	sess := NewStreamSession(sub, streamID, "token", "agent-1")
	if _, err := sess.Start(ctx, nil); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := sess.Delta(ctx, "partial"); err != nil {
		t.Fatalf("delta: %v", err)
	}
	if _, err := sess.Fail(ctx, "provider exploded"); err != nil {
		t.Fatalf("fail: %v", err)
	}

	got := drainFrames(t, out, ack, 3, 5*time.Second)
	if len(got) != 3 {
		t.Fatalf("want 3 frames, got %d", len(got))
	}
	if got[2].Phase != StreamPhaseError || got[2].Err != "provider exploded" || !got[2].Terminal {
		t.Fatalf("error terminal wrong: %+v", got[2])
	}
	// Channel must close after the terminal.
	select {
	case _, ok := <-out:
		if ok {
			t.Fatal("frame delivered after terminal error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("channel not closed after terminal error")
	}
}

// TestStreamLifecycleCancel proves Cancel surfaces a cancel terminal (distinct
// from error) and closes the channel.
func TestStreamLifecycleCancel(t *testing.T) {
	t.Parallel()
	sub, _ := newTestStreamSubstrate()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const streamID = "transcribe-req-cancel"
	out, ack, err := SubscribeStreamFrames(ctx, sub, streamID, "bff-A")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	sess := NewStreamSession(sub, streamID, "transcript", "voice-1")
	if _, err := sess.Start(ctx, nil); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := sess.Cancel(ctx); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	got := drainFrames(t, out, ack, 2, 5*time.Second)
	if len(got) != 2 {
		t.Fatalf("want 2 frames, got %d", len(got))
	}
	if got[1].Phase != StreamPhaseCancel || !got[1].Terminal {
		t.Fatalf("cancel terminal wrong: %+v", got[1])
	}
	if got[1].Err != "" {
		t.Fatalf("cancel should carry no Err, got %q", got[1].Err)
	}
}

// TestStreamLifecycleGuards proves the producer rejects out-of-lifecycle calls
// (Delta before Start, Delta after terminal, double Start, double terminal) so a
// well-formed start->delta*->terminal sequence is the only producible shape.
func TestStreamLifecycleGuards(t *testing.T) {
	t.Parallel()
	sub, _ := newTestStreamSubstrate()
	ctx := context.Background()

	sess := NewStreamSession(sub, "chat-req-guard", "token", "agent-1")
	if _, err := sess.Delta(ctx, "x"); err == nil {
		t.Fatal("Delta before Start must error")
	}
	if _, err := sess.Start(ctx, nil); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := sess.Start(ctx, nil); err == nil {
		t.Fatal("double Start must error")
	}
	if _, err := sess.Complete(ctx, "done", nil); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if _, err := sess.Delta(ctx, "y"); err == nil {
		t.Fatal("Delta after terminal must error")
	}
	if _, err := sess.Complete(ctx, "again", nil); err == nil {
		t.Fatal("double terminal must error")
	}
}

// TestStreamFrameEncodingRoundTrip proves the compact (no-meta) and JSON
// (with-meta) encodings both round-trip, and that arbitrary token text that
// happens to contain the separator or a leading brace decodes back as a delta
// rather than corrupting the lifecycle.
func TestStreamFrameEncodingRoundTrip(t *testing.T) {
	t.Parallel()

	// Compact, no meta.
	p, d, m := decodeFrame(encodeFrame(StreamPhaseDelta, "hello world", nil))
	if p != StreamPhaseDelta || d != "hello world" || m != nil {
		t.Fatalf("compact round-trip: phase=%q data=%q meta=%v", p, d, m)
	}

	// JSON, with meta.
	p, d, m = decodeFrame(encodeFrame(StreamPhaseStart, "", map[string]any{"k": "v"}))
	if p != StreamPhaseStart || m["k"] != "v" {
		t.Fatalf("json round-trip: phase=%q meta=%v", p, m)
	}

	// A token that itself starts with '{' but is not our envelope -> delta.
	p, d, _ = decodeFrame(encodeFrame(StreamPhaseDelta, "{not json", nil))
	if p != StreamPhaseDelta || d != "{not json" {
		t.Fatalf("brace token mis-decoded: phase=%q data=%q", p, d)
	}

	// A raw payload with no envelope at all (defensive: a foreign chunk) -> delta.
	p, d, _ = decodeFrame("just text")
	if p != StreamPhaseDelta || d != "just text" {
		t.Fatalf("raw payload mis-decoded: phase=%q data=%q", p, d)
	}
}
