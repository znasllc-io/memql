package node

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// recordingRetentionStore captures SweepOlderThan calls for loop-level tests.
type recordingRetentionStore struct {
	mu      sync.Mutex
	cutoffs []time.Time
	err     error
}

func (r *recordingRetentionStore) SweepOlderThan(_ context.Context, cutoff time.Time) (int64, int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cutoffs = append(r.cutoffs, cutoff)
	return 1, 1, r.err
}

func (r *recordingRetentionStore) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.cutoffs)
}

func (r *recordingRetentionStore) lastCutoff() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.cutoffs) == 0 {
		return time.Time{}
	}
	return r.cutoffs[len(r.cutoffs)-1]
}

// TestOutboxRetentionFromEnv pins the env-knob contract: unset -> 24h default,
// a parsable duration -> that value (including zero/negative, which disable
// the sweep), unparsable -> the default (a typo must not silently disable
// retention).
func TestOutboxRetentionFromEnv(t *testing.T) {
	cases := []struct {
		name  string
		value string // "" means unset
		want  time.Duration
	}{
		{"unset uses default", "", defaultOutboxRetention},
		{"custom duration", "48h", 48 * time.Hour},
		{"sub-day duration", "90m", 90 * time.Minute},
		{"zero disables", "0", 0},
		{"negative disables", "-1h", -time.Hour},
		{"unparsable falls back to default", "tomorrow", defaultOutboxRetention},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(outboxRetentionEnvVar, tc.value)
			if got := outboxRetentionFromEnv(nil); got != tc.want {
				t.Fatalf("outboxRetentionFromEnv(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

// TestOutboxRetention_SweepCutoff verifies one sweep pass uses now-retention
// as the watermark.
func TestOutboxRetention_SweepCutoff(t *testing.T) {
	store := &recordingRetentionStore{}
	r := newOutboxRetention(store, 24*time.Hour, nil)
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }

	r.sweep(context.Background())

	if store.calls() != 1 {
		t.Fatalf("expected exactly one sweep call, got %d", store.calls())
	}
	want := now.Add(-24 * time.Hour)
	if got := store.lastCutoff(); !got.Equal(want) {
		t.Fatalf("sweep cutoff = %v, want %v", got, want)
	}
}

// TestOutboxRetention_SweepErrorIsNonFatal verifies a failing sweep only logs
// (never panics / propagates) so the next tick can retry.
func TestOutboxRetention_SweepErrorIsNonFatal(t *testing.T) {
	store := &recordingRetentionStore{err: fmt.Errorf("db down")}
	r := newOutboxRetention(store, time.Hour, nil)
	r.sweep(context.Background()) // must not panic
	if store.calls() != 1 {
		t.Fatalf("expected the sweep to be attempted once, got %d", store.calls())
	}
}

// TestOutboxRetention_RunLoopSweepsOnInterval drives the run hook with a
// shrunk startup delay + interval and verifies repeated sweeps until the
// context is cancelled.
func TestOutboxRetention_RunLoopSweepsOnInterval(t *testing.T) {
	store := &recordingRetentionStore{}
	r := newOutboxRetention(store, time.Hour, nil)
	r.startupDelay = 5 * time.Millisecond
	r.interval = 5 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = r.run(ctx, func() {})
	}()

	deadline := time.After(2 * time.Second)
	for store.calls() < 3 {
		select {
		case <-deadline:
			t.Fatalf("expected >=3 sweeps within the deadline, got %d", store.calls())
		case <-time.After(2 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("run hook did not exit on context cancellation")
	}
}

// TestOutboxRetention_DisabledNeverSweeps verifies a zero/negative watermark
// turns the loop into a pure ctx-wait: no store calls ever happen.
func TestOutboxRetention_DisabledNeverSweeps(t *testing.T) {
	store := &recordingRetentionStore{}
	r := newOutboxRetention(store, 0, nil)
	r.startupDelay = time.Millisecond
	r.interval = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = r.run(ctx, func() {})
	}()

	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("run hook did not exit on context cancellation")
	}
	if store.calls() != 0 {
		t.Fatalf("disabled retention must never sweep; got %d calls", store.calls())
	}
}

// TestFakeStore_SweepOlderThan exercises the retention semantics end-to-end
// against the in-memory fake (the same seam the substrate tests use): rows
// older than the watermark and stale cursors are deleted, fresh state
// survives, the per-key seq allocator is preserved, and post-sweep produce /
// replay still satisfy the substrate contract.
func TestFakeStore_SweepOlderThan(t *testing.T) {
	ctx := context.Background()
	store := newFakeOutboxStore()
	key := RoutingKey{Kind: "space", ID: "s1"}

	base := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	clock := base
	store.clock = func() time.Time { return clock }

	// Two old rows, an old cursor for a dead consumer.
	for i := 1; i <= 2; i++ {
		if _, _, err := store.Append(ctx, Deliverable{EventID: fmt.Sprintf("old-%d", i), Key: key}); err != nil {
			t.Fatalf("append old row: %v", err)
		}
	}
	if err := store.SaveCursor(ctx, key, "dead-pod", 2); err != nil {
		t.Fatalf("save old cursor: %v", err)
	}

	// A fresh row + fresh cursor 30h later.
	clock = base.Add(30 * time.Hour)
	if _, _, err := store.Append(ctx, Deliverable{EventID: "fresh-1", Key: key}); err != nil {
		t.Fatalf("append fresh row: %v", err)
	}
	if err := store.SaveCursor(ctx, key, "live-pod", 3); err != nil {
		t.Fatalf("save fresh cursor: %v", err)
	}

	// Sweep with a 24h watermark measured at the fresh clock.
	cutoff := clock.Add(-24 * time.Hour)
	outboxDeleted, cursorsDeleted, err := store.SweepOlderThan(ctx, cutoff)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if outboxDeleted != 2 || cursorsDeleted != 1 {
		t.Fatalf("sweep deleted (outbox=%d, cursors=%d), want (2, 1)", outboxDeleted, cursorsDeleted)
	}

	// Only the fresh row remains; a full replay starts there.
	rows, err := store.ReadAfter(ctx, key, 0, 0)
	if err != nil {
		t.Fatalf("read after sweep: %v", err)
	}
	if len(rows) != 1 || rows[0].EventID != "fresh-1" || rows[0].Seq != 3 {
		t.Fatalf("post-sweep replay = %+v, want only fresh-1 at seq 3", rows)
	}

	// The dead consumer's cursor is gone (reads as brand-new); the live one
	// survives.
	if seq, _ := store.LoadCursor(ctx, key, "dead-pod"); seq != 0 {
		t.Fatalf("dead-pod cursor = %d, want 0 (swept)", seq)
	}
	if seq, _ := store.LoadCursor(ctx, key, "live-pod"); seq != 3 {
		t.Fatalf("live-pod cursor = %d, want 3 (retained)", seq)
	}

	// The seq allocator was NOT reset: the next produce on the key continues
	// strictly increasing (seq 4), never restarting at 1 -- the invariant that
	// makes skipping the mesh_key_seq sweep the correct call.
	seq, dup, err := store.Append(ctx, Deliverable{EventID: "post-sweep", Key: key})
	if err != nil || dup {
		t.Fatalf("post-sweep append: seq=%d dup=%v err=%v", seq, dup, err)
	}
	if seq != 4 {
		t.Fatalf("post-sweep seq = %d, want 4 (allocator preserved across sweep)", seq)
	}
}
