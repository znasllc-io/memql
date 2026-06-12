package node

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// delivery_highwatermark_test.go -- the memql#1328 cursor-initialization
// contract, against the in-memory fake store (the pg path mirrors the same
// EnsureCursor semantics; see pgOutboxStore.EnsureCursor):
//
//   - a BRAND-NEW consumer (no cursor row) starts at the key's current high
//     watermark and sees only events produced after it first subscribed;
//   - WithReplayBacklog opts a subscription into the old contract (replay the
//     retained backlog from seq 0);
//   - an EXISTING consumer resumes from its cursor exactly, regardless of mode
//     (covered by TestSubstrateReplayFromCursor's default-mode resubscribe);
//   - the high-watermark init persists, so a reconnect replays the gap instead
//     of re-pinning at a newer tip (no deliver-then-skip-backwards);
//   - events produced concurrently with the first subscribe are either
//     pre-init (skippable) or delivered -- never lost once delivery starts.

// TestSubstrateNewConsumerStartsAtHighWatermark: the default. Backlog produced
// before the consumer exists is skipped; everything after the subscribe lands.
func TestSubstrateNewConsumerStartsAtHighWatermark(t *testing.T) {
	store := newFakeOutboxStore()
	sub := NewSubstrate(store, time.Minute, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	key := testKey("hwm")

	// Retained backlog from before this consumer existed.
	for i := 1; i <= 5; i++ {
		if _, err := sub.Publish(ctx, mkDeliverable(key, fmt.Sprintf("old%d", i), "t")); err != nil {
			t.Fatalf("Publish backlog %d: %v", i, err)
		}
	}

	ch, err := sub.Subscribe(ctx, key, "fresh-pod")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// The init is synchronous and persisted: the cursor row exists at the
	// watermark before any delivery or Ack.
	if c, _ := store.LoadCursor(ctx, key, "fresh-pod"); c != 5 {
		t.Fatalf("expected cursor pinned at watermark 5, got %d", c)
	}

	// Post-subscribe events are delivered, in order, with their real seqs.
	for i := 6; i <= 7; i++ {
		if _, err := sub.Publish(ctx, mkDeliverable(key, fmt.Sprintf("new%d", i), "t")); err != nil {
			t.Fatalf("Publish new %d: %v", i, err)
		}
	}
	got := recvN(t, ch, 2, 2*time.Second)
	if got[0].EventID != "new6" || got[0].Seq != 6 || got[1].EventID != "new7" || got[1].Seq != 7 {
		t.Fatalf("expected new6(seq6),new7(seq7); got %s(%d),%s(%d)",
			got[0].EventID, got[0].Seq, got[1].EventID, got[1].Seq)
	}

	// And no backlog row ever arrives.
	select {
	case d := <-ch:
		t.Fatalf("backlog leaked past the high watermark: %s (seq %d)", d.EventID, d.Seq)
	case <-time.After(400 * time.Millisecond):
	}
}

// TestSubstrateReplayBacklogOptIn: WithReplayBacklog preserves the old
// first-subscribe contract -- the brand-new consumer replays the retained
// backlog from seq 0 -- and does NOT persist a cursor row before the first
// Ack (the pre-#1328 shape).
func TestSubstrateReplayBacklogOptIn(t *testing.T) {
	store := newFakeOutboxStore()
	sub := NewSubstrate(store, time.Minute, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	key := testKey("optin")

	for i := 1; i <= 3; i++ {
		if _, err := sub.Publish(ctx, mkDeliverable(key, fmt.Sprintf("e%d", i), "t")); err != nil {
			t.Fatalf("Publish %d: %v", i, err)
		}
	}

	ch, err := sub.Subscribe(ctx, key, "replayer", WithReplayBacklog())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if store.hasCursorRow(key, "replayer") {
		t.Fatal("fromStart subscribe must not persist a cursor row before the first Ack")
	}

	got := recvN(t, ch, 3, 2*time.Second)
	for i, d := range got {
		if want := fmt.Sprintf("e%d", i+1); d.EventID != want {
			t.Fatalf("backlog replay order wrong at %d: got %s, want %s", i, d.EventID, want)
		}
	}

	// First Ack creates the cursor row, as before.
	if err := sub.Ack(ctx, key, "replayer", got[2].Seq); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	if c, _ := store.LoadCursor(ctx, key, "replayer"); c != 3 {
		t.Fatalf("expected cursor 3 after ack, got %d", c)
	}
}

// TestSubstrateHighWatermarkInitPersistsAcrossReconnect: the first subscribe
// pins the tip DURABLY. A reconnect under the same consumer id resumes from
// the pinned position and replays the gap produced while disconnected -- it
// must NOT re-initialize at the newer tip (that would silently lose the gap,
// violating "never deliver-then-skip-backwards").
func TestSubstrateHighWatermarkInitPersistsAcrossReconnect(t *testing.T) {
	store := newFakeOutboxStore()
	sub := NewSubstrate(store, time.Minute, nil, nil)
	ctx := context.Background()
	key := testKey("persist")

	// Pre-existing backlog the consumer should never see.
	for i := 1; i <= 2; i++ {
		if _, err := sub.Publish(ctx, mkDeliverable(key, fmt.Sprintf("old%d", i), "t")); err != nil {
			t.Fatalf("Publish backlog %d: %v", i, err)
		}
	}

	// First subscribe pins the watermark (2); disconnect without consuming.
	ctx1, cancel1 := context.WithCancel(ctx)
	if _, err := sub.Subscribe(ctx1, key, "podX"); err != nil {
		t.Fatalf("Subscribe 1: %v", err)
	}
	cancel1()
	time.Sleep(20 * time.Millisecond) // let tail-1 exit

	// Produced while the consumer is away.
	for i := 3; i <= 4; i++ {
		if _, err := sub.Publish(ctx, mkDeliverable(key, fmt.Sprintf("gap%d", i), "t")); err != nil {
			t.Fatalf("Publish gap %d: %v", i, err)
		}
	}

	// Reconnect: must resume from the persisted watermark and replay the gap.
	ctx2, cancel2 := context.WithCancel(ctx)
	defer cancel2()
	ch2, err := sub.Subscribe(ctx2, key, "podX")
	if err != nil {
		t.Fatalf("Subscribe 2: %v", err)
	}
	got := recvN(t, ch2, 2, 2*time.Second)
	if got[0].EventID != "gap3" || got[1].EventID != "gap4" {
		t.Fatalf("expected gap3,gap4 replay after reconnect; got %s,%s", got[0].EventID, got[1].EventID)
	}
	select {
	case d := <-ch2:
		t.Fatalf("unexpected extra deliverable: %s", d.EventID)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestSubstrateHighWatermarkConcurrentProducerNoLoss is the race shape: a
// producer publishes continuously while a brand-new consumer takes its first
// subscribe mid-stream. Events produced before the init MAY be skipped (that
// is the point); but once delivery starts, the delivered seqs must be a
// CONTIGUOUS, strictly-ascending suffix running through the final event --
// nothing produced after the cursor init is ever lost or reordered. Run with
// -race for the memory-model side.
func TestSubstrateHighWatermarkConcurrentProducerNoLoss(t *testing.T) {
	store := newFakeOutboxStore()
	sub := NewSubstrate(store, time.Minute, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	key := testKey("race")

	const n = 200
	producerDone := make(chan struct{})
	go func() {
		defer close(producerDone)
		for i := 1; i <= n; i++ {
			if _, err := sub.Publish(ctx, mkDeliverable(key, fmt.Sprintf("e%d", i), "t")); err != nil {
				t.Errorf("Publish %d: %v", i, err)
				return
			}
		}
	}()

	// Subscribe somewhere in the middle of the produce burst.
	time.Sleep(2 * time.Millisecond)
	ch, err := sub.Subscribe(ctx, key, "racer")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	<-producerDone

	// A sentinel published strictly AFTER Subscribe returned MUST be delivered
	// (the synchronous init guarantee), and gives the collection loop a known
	// final seq even if the consumer's watermark landed at n.
	sentinelSeq, err := sub.Publish(ctx, mkDeliverable(key, "sentinel", "t"))
	if err != nil {
		t.Fatalf("Publish sentinel: %v", err)
	}
	if sentinelSeq != n+1 {
		t.Fatalf("expected sentinel seq %d, got %d", n+1, sentinelSeq)
	}

	var seqs []int64
	deadline := time.After(5 * time.Second)
	for {
		select {
		case d := <-ch:
			seqs = append(seqs, d.Seq)
			if d.Seq == sentinelSeq {
				goto collected
			}
		case <-deadline:
			t.Fatalf("timed out before sentinel; got %d deliverables (last seq %v)", len(seqs), seqs)
		}
	}
collected:
	if len(seqs) == 0 {
		t.Fatal("no deliverables at all (sentinel itself must arrive)")
	}
	// Contiguous strictly-ascending suffix ending at the sentinel: any gap
	// would mean an event produced after the cursor init was lost; any
	// non-monotonic step would violate per-producer order.
	for i := 1; i < len(seqs); i++ {
		if seqs[i] != seqs[i-1]+1 {
			t.Fatalf("delivered seqs not contiguous at %d: %d -> %d (full: %v)", i, seqs[i-1], seqs[i], seqs)
		}
	}
	if seqs[len(seqs)-1] != sentinelSeq {
		t.Fatalf("suffix must end at sentinel seq %d, got %d", sentinelSeq, seqs[len(seqs)-1])
	}
}

// TestEnsureCursorExistingWinsOverBothModes pins the precedence rule at the
// store seam: once a cursor row exists, EnsureCursor returns it unchanged for
// BOTH modes -- the high-watermark default must not re-pin a consumer forward,
// and WithReplayBacklog must not rewind it to 0.
func TestEnsureCursorExistingWinsOverBothModes(t *testing.T) {
	store := newFakeOutboxStore()
	ctx := context.Background()
	key := testKey("precedence")

	// Allocate some seqs so the watermark (5) differs from the cursor (2).
	for i := 1; i <= 5; i++ {
		if _, _, err := store.Append(ctx, mkDeliverable(key, fmt.Sprintf("e%d", i), "t")); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	if err := store.SaveCursor(ctx, key, "c", 2); err != nil {
		t.Fatalf("SaveCursor: %v", err)
	}

	for _, fromStart := range []bool{false, true} {
		seq, initialized, err := store.EnsureCursor(ctx, key, "c", fromStart)
		if err != nil {
			t.Fatalf("EnsureCursor(fromStart=%v): %v", fromStart, err)
		}
		if initialized {
			t.Fatalf("EnsureCursor(fromStart=%v) must not re-initialize an existing cursor", fromStart)
		}
		if seq != 2 {
			t.Fatalf("EnsureCursor(fromStart=%v): expected resume at 2, got %d", fromStart, seq)
		}
	}
}
