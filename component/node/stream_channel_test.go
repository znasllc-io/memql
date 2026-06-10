package node

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// newTestStreamSubstrate builds a Substrate over the in-memory fake store used
// across the substrate tests (no Postgres). dedupTTL is generous so cross-path
// dedup never expires mid-test.
func newTestStreamSubstrate() (*Substrate, *fakeOutboxStore) {
	store := newFakeOutboxStore()
	sub := NewSubstrate(store, time.Minute, nil, nil)
	return sub, store
}

// drainChunks reads up to want chunks (or until a terminal) from the consumer
// channel, Acking each via ack, bounded by a deadline. It returns the chunks in
// delivery order.
func drainChunks(t *testing.T, out <-chan StreamChunk, ack func(int64) error, want int, deadline time.Duration) []StreamChunk {
	t.Helper()
	var got []StreamChunk
	timeout := time.After(deadline)
	for len(got) < want {
		select {
		case c, ok := <-out:
			if !ok {
				return got
			}
			if err := ack(c.Seq); err != nil {
				t.Fatalf("ack seq %d: %v", c.Seq, err)
			}
			got = append(got, c)
			if c.Terminal {
				return got
			}
		case <-timeout:
			t.Fatalf("timed out after %d/%d chunks", len(got), want)
		}
	}
	return got
}

// TestStreamOrdering proves chunks are delivered in produce order with strictly
// increasing seq, including the terminal.
func TestStreamOrdering(t *testing.T) {
	t.Parallel()
	sub, _ := newTestStreamSubstrate()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const streamID = "chat-req-1"
	out, ack, err := SubscribeStream(ctx, sub, streamID, "bff-A")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	pub := NewStreamPublisher(sub, streamID, "agent-1")
	const n = 50
	for i := 0; i < n; i++ {
		if _, err := pub.Append(ctx, "token", fmt.Sprintf("tok-%d", i)); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	if _, err := pub.Close(ctx, "token", ""); err != nil {
		t.Fatalf("close: %v", err)
	}

	got := drainChunks(t, out, ack, n+1, 5*time.Second)
	if len(got) != n+1 {
		t.Fatalf("want %d chunks, got %d", n+1, len(got))
	}
	var lastSeq int64
	for i := 0; i < n; i++ {
		if got[i].Data != fmt.Sprintf("tok-%d", i) {
			t.Fatalf("chunk %d out of order: data=%q", i, got[i].Data)
		}
		if got[i].Seq <= lastSeq {
			t.Fatalf("seq not strictly increasing at %d: %d <= %d", i, got[i].Seq, lastSeq)
		}
		lastSeq = got[i].Seq
		if got[i].Terminal {
			t.Fatalf("non-terminal chunk %d marked terminal", i)
		}
	}
	if !got[n].Terminal {
		t.Fatalf("final chunk not terminal")
	}
}

// TestStreamReplayAcrossReconnect proves the core #1266 guarantee: a consumer
// (the BFF replica owning the WS) that disconnects mid-stream and reconnects --
// possibly as a DIFFERENT replica id reusing the same logical consumerID --
// replays from its cursor and loses no chunks and reorders none, even though the
// producer kept producing while it was gone.
func TestStreamReplayAcrossReconnect(t *testing.T) {
	t.Parallel()
	sub, _ := newTestStreamSubstrate()
	const streamID = "chat-req-2"
	const consumerID = "ws-owner" // stable logical consumer across replica churn

	pub := NewStreamPublisher(sub, streamID, "agent-1")
	bg := context.Background()

	// Produce 10 chunks up front.
	const total = 10
	for i := 0; i < total; i++ {
		if _, err := pub.Append(bg, "token", fmt.Sprintf("t%d", i)); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	// First consumer connects, reads the first 4, Acks them, then "dies".
	ctx1, cancel1 := context.WithCancel(context.Background())
	out1, ack1, err := SubscribeStream(ctx1, sub, streamID, consumerID)
	if err != nil {
		t.Fatalf("subscribe 1: %v", err)
	}
	first := drainChunks(t, out1, ack1, 4, 5*time.Second)
	if len(first) != 4 {
		t.Fatalf("want 4 on first connect, got %d", len(first))
	}
	cancel1() // replica owning the WS dies mid-stream

	// Producer keeps going + terminates while no consumer is attached.
	for i := total; i < total+5; i++ {
		if _, err := pub.Append(bg, "token", fmt.Sprintf("t%d", i)); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	if _, err := pub.Close(bg, "token", ""); err != nil {
		t.Fatalf("close: %v", err)
	}

	// A NEW replica takes over the WS, re-subscribes with the SAME logical
	// consumerID, and must replay exactly chunks 4..14 + terminal, in order,
	// none lost, none duplicated.
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	out2, ack2, err := SubscribeStream(ctx2, sub, streamID, consumerID)
	if err != nil {
		t.Fatalf("subscribe 2: %v", err)
	}
	// 11 remaining data chunks (t4..t14) + 1 terminal = 12.
	rest := drainChunks(t, out2, ack2, 12, 5*time.Second)

	// Reassemble: the two connects together must yield t0..t14 + terminal,
	// each exactly once, in order.
	var all []StreamChunk
	all = append(all, first...)
	all = append(all, rest...)

	dataChunks := 0
	var lastSeq int64
	for i, c := range all {
		if c.Seq <= lastSeq && i > 0 {
			t.Fatalf("seq regressed at %d: %d <= %d", i, c.Seq, lastSeq)
		}
		lastSeq = c.Seq
		if c.Terminal {
			continue
		}
		want := fmt.Sprintf("t%d", dataChunks)
		if c.Data != want {
			t.Fatalf("chunk %d data=%q want %q (lost or reordered)", i, c.Data, want)
		}
		dataChunks++
	}
	if dataChunks != total+5 {
		t.Fatalf("want %d data chunks total, got %d", total+5, dataChunks)
	}
	if !all[len(all)-1].Terminal {
		t.Fatalf("stream did not end with terminal")
	}
}

// TestStreamBackpressure proves a slow consumer back-pressures rather than
// dropping or deadlocking: the producer keeps publishing durable rows (it never
// blocks beyond the synchronous store write), and a consumer that reads slowly
// still receives every chunk in order once it drains. No chunk is lost and the
// producer does not deadlock waiting on a full consumer channel.
func TestStreamBackpressure(t *testing.T) {
	t.Parallel()
	sub, _ := newTestStreamSubstrate()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const streamID = "audio-req-1"
	out, ack, err := SubscribeStream(ctx, sub, streamID, "voice-consumer")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	// Producer publishes far more than the channel buffer (substrateChannelBuffer)
	// up front. If the producer were coupled to the consumer's read rate it would
	// block here; it must not (durable write only).
	pub := NewStreamPublisher(sub, streamID, "voice-1")
	n := substrateChannelBuffer * 3
	produced := make(chan struct{})
	go func() {
		for i := 0; i < n; i++ {
			if _, err := pub.Append(ctx, "delta", fmt.Sprintf("d%d", i)); err != nil {
				t.Errorf("append %d: %v", i, err)
				return
			}
		}
		if _, err := pub.Close(ctx, "delta", ""); err != nil {
			t.Errorf("close: %v", err)
			return
		}
		close(produced)
	}()

	select {
	case <-produced:
	case <-time.After(5 * time.Second):
		t.Fatal("producer deadlocked under a slow/unattached consumer")
	}

	// Now drain slowly; every chunk must arrive in order, none dropped.
	got := drainChunks(t, out, ack, n+1, 10*time.Second)
	if len(got) != n+1 {
		t.Fatalf("want %d chunks, got %d", n+1, len(got))
	}
	for i := 0; i < n; i++ {
		if got[i].Data != fmt.Sprintf("d%d", i) {
			t.Fatalf("chunk %d data=%q (lost/reordered under backpressure)", i, got[i].Data)
		}
	}
	if !got[n].Terminal {
		t.Fatalf("final chunk not terminal")
	}
}

// TestStreamTerminalErrorClosesChannel proves Fail surfaces an error terminal
// and closes the consumer channel.
func TestStreamTerminalError(t *testing.T) {
	t.Parallel()
	sub, _ := newTestStreamSubstrate()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const streamID = "chat-req-err"
	out, ack, err := SubscribeStream(ctx, sub, streamID, "bff-A")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	pub := NewStreamPublisher(sub, streamID, "agent-1")
	if _, err := pub.Append(ctx, "token", "partial"); err != nil {
		t.Fatalf("append: %v", err)
	}
	if _, err := pub.Fail(ctx, "token", "provider exploded"); err != nil {
		t.Fatalf("fail: %v", err)
	}

	got := drainChunks(t, out, ack, 2, 5*time.Second)
	if len(got) != 2 {
		t.Fatalf("want 2 chunks, got %d", len(got))
	}
	if got[1].Err != "provider exploded" {
		t.Fatalf("terminal err = %q, want %q", got[1].Err, "provider exploded")
	}
	if !got[1].Terminal {
		t.Fatalf("error chunk not marked terminal")
	}
	// The channel must close after the terminal.
	select {
	case _, ok := <-out:
		if ok {
			t.Fatal("channel delivered a chunk after terminal")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("channel not closed after terminal error")
	}
}

// TestStreamIdempotentReproduce proves a retried Append (same producer-local
// idx) is an idempotent no-op at the substrate and does not duplicate a chunk.
func TestStreamIdempotentReproduce(t *testing.T) {
	t.Parallel()
	sub, store := newTestStreamSubstrate()
	ctx := context.Background()
	const streamID = "chat-req-idem"

	pub := NewStreamPublisher(sub, streamID, "agent-1")
	seq1, err := pub.Append(ctx, "token", "hello")
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	// Re-publish the SAME chunk (same EventID) directly via the substrate to
	// simulate a fast-path/durable double, mimicking a retried produce.
	key := StreamKey(streamID)
	seq2, _, err := store.Append(ctx, Deliverable{
		EventID: streamID + ":0", // matches the first chunk's EventID
		Key:     key,
		Topic:   "token",
		Payload: map[string]any{streamPayloadData: "hello"},
	})
	if err != nil {
		t.Fatalf("re-append: %v", err)
	}
	if seq1 != seq2 {
		t.Fatalf("idempotent reproduce got a new seq: %d vs %d", seq1, seq2)
	}
	rows, err := store.ReadAfter(ctx, key, 0, 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("idempotent reproduce duplicated the row: %d rows", len(rows))
	}
}
