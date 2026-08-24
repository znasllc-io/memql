package datasync

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
)

var testNow = time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)

// A successful delivery is claimed, delivered, and recorded -- in that
// order, and the attempt is counted at CLAIM time.
func TestASuccessfulDeliveryIsClaimedThenRecorded(t *testing.T) {
	engine := newFakeEngine()
	engine.seed(`query outboxPending`, []map[string]any{outboxRow("e1", "shopify", 0, "")})
	c := &fakeConnector{name: "shopify"}
	w := testWorker(engine, &alwaysClaim{}, c, testNow)

	w.DrainOnce(context.Background())

	if got := c.propagateCount(); got != 1 {
		t.Fatalf("Propagate called %d times, want 1", got)
	}
	if got := engine.countContaining("markOutboxDelivering"); got != 1 {
		t.Errorf("markOutboxDelivering called %d times, want 1", got)
	}
	if got := engine.countContaining("markOutboxDelivered"); got != 1 {
		t.Errorf("markOutboxDelivered called %d times, want 1", got)
	}
	claim := engine.callsContaining("markOutboxDelivering")
	if len(claim) == 0 || !strings.Contains(claim[0], "attempts: 1") {
		t.Errorf("the claim did not count the attempt: %v -- a worker that dies mid-delivery must still spend one of the entry's lives", claim)
	}
	delivered, failed, dead := w.Counters()
	if delivered != 1 || failed != 0 || dead != 0 {
		t.Errorf("counters = %d/%d/%d, want 1/0/0", delivered, failed, dead)
	}
	// The idempotency key the entry stored is what reaches the receiver
	// -- not one recomputed here, which could differ.
	if key := c.propagateCalls[0].IdempotencyKey; !strings.Contains(key, "row-e1") {
		t.Errorf("the stored idempotency key did not reach Propagate: %q", key)
	}
}

// A failure schedules a retry with backoff and does NOT dead-letter
// while attempts remain.
func TestAFailedDeliveryIsRetriedWithBackoff(t *testing.T) {
	engine := newFakeEngine()
	engine.seed(`query outboxPending`, []map[string]any{outboxRow("e1", "shopify", 0, "")})
	c := &fakeConnector{name: "shopify", propagateResults: []error{errors.New("receiver said no")}}
	w := testWorker(engine, &alwaysClaim{}, c, testNow)

	w.DrainOnce(context.Background())

	failedCalls := engine.callsContaining("markOutboxFailed")
	if len(failedCalls) != 1 {
		t.Fatalf("markOutboxFailed called %d times, want 1", len(failedCalls))
	}
	if engine.countContaining("markOutboxDead") != 0 {
		t.Error("the entry was dead-lettered on its FIRST failure -- attempts remained")
	}
	want := testNow.Add(w.cfg.BackoffBase).UTC().Format(time.RFC3339)
	if !strings.Contains(failedCalls[0], want) {
		t.Errorf("retry scheduled at the wrong time: %q, want it to contain %q (the first failure waits one base interval, not two)", failedCalls[0], want)
	}
	if _, failed, _ := w.Counters(); failed != 1 {
		t.Errorf("failed counter = %d, want 1", failed)
	}
}

// The backoff doubles and is capped, so the eighth attempt is not an
// hour away.
func TestBackoffDoublesAndIsCapped(t *testing.T) {
	cfg := Config{BackoffBase: 30 * time.Second, BackoffMax: 300 * time.Second}
	cases := []struct {
		attempts int
		want     time.Duration
	}{
		{0, 30 * time.Second}, // clamped to the first attempt
		{1, 30 * time.Second},
		{2, 60 * time.Second},
		{3, 120 * time.Second},
		{4, 240 * time.Second},
		{5, 300 * time.Second}, // capped
		{9, 300 * time.Second},
	}
	for _, tc := range cases {
		if got := cfg.backoffFor(tc.attempts); got != tc.want {
			t.Errorf("backoffFor(%d) = %s, want %s", tc.attempts, got, tc.want)
		}
	}
}

// A receiver that states its own Retry-After outranks our schedule when
// it asks for LONGER -- retrying sooner than a rate limit asks is how a
// temporary limit becomes a permanent one.
func TestAReceiverRetryAfterOutranksTheScheduleWhenItIsLonger(t *testing.T) {
	engine := newFakeEngine()
	engine.seed(`query outboxPending`, []map[string]any{outboxRow("e1", "shopify", 0, "")})
	c := &fakeConnector{
		name:             "shopify",
		propagateResults: []error{errors.New("429")},
		propagateRetry:   10 * time.Minute,
	}
	w := testWorker(engine, &alwaysClaim{}, c, testNow)

	w.DrainOnce(context.Background())

	want := testNow.Add(10 * time.Minute).UTC().Format(time.RFC3339)
	calls := engine.callsContaining("markOutboxFailed")
	if len(calls) != 1 || !strings.Contains(calls[0], want) {
		t.Errorf("the receiver's Retry-After was not honoured: %v, want it to contain %q", calls, want)
	}
}

// Attempts exhausted means dead-lettered -- terminal, and never picked
// up again automatically.
func TestAnEntryIsDeadLetteredWhenItsAttemptsAreExhausted(t *testing.T) {
	engine := newFakeEngine()
	// attempts=7 with a max of 8 means this attempt is the eighth.
	engine.seed(`query outboxPending`, []map[string]any{outboxRow("e1", "shopify", 7, "")})
	c := &fakeConnector{name: "shopify", propagateResults: []error{errors.New("still broken")}}
	w := testWorker(engine, &alwaysClaim{}, c, testNow)
	w.cfg.MaxAttempts = 8

	w.DrainOnce(context.Background())

	if engine.countContaining("markOutboxDead") != 1 {
		t.Fatalf("the entry was not dead-lettered on attempt %d of %d", 8, w.cfg.MaxAttempts)
	}
	if engine.countContaining("markOutboxFailed") != 0 {
		t.Error("a dead-lettered entry was ALSO scheduled for retry -- dead means an operator decides, not the worker")
	}
	if _, _, dead := w.Counters(); dead != 1 {
		t.Errorf("dead counter = %d, want 1", dead)
	}
}

// An entry whose retry time has not come is skipped -- not delivered
// early, and not counted as an attempt.
func TestAnEntryIsSkippedUntilItsRetryTimeComes(t *testing.T) {
	engine := newFakeEngine()
	future := testNow.Add(5 * time.Minute).Format(time.RFC3339)
	engine.seed(`query outboxPending`, []map[string]any{outboxRow("e1", "shopify", 1, future)})
	c := &fakeConnector{name: "shopify"}
	w := testWorker(engine, &alwaysClaim{}, c, testNow)

	w.DrainOnce(context.Background())

	if c.propagateCount() != 0 {
		t.Error("an entry was delivered before its scheduled retry time")
	}
	if engine.countContaining("markOutboxDelivering") != 0 {
		t.Error("an entry not yet due was claimed, spending one of its attempts")
	}
}

// A connector that does not implement outbound delivery parks its
// entries; it must not dead-letter every one of them.
func TestAnUnimplementedPropagateParksRatherThanDeadLetters(t *testing.T) {
	engine := newFakeEngine()
	engine.seed(`query outboxPending`, []map[string]any{outboxRow("e1", "shopify", 7, "")})
	c := &fakeConnector{
		name:             "shopify",
		propagateResults: []error{memqlsync.NotImplemented("shopify", "Propagate")},
	}
	w := testWorker(engine, &alwaysClaim{}, c, testNow)
	w.cfg.MaxAttempts = 8

	w.DrainOnce(context.Background())

	if engine.countContaining("markOutboxDead") != 0 {
		t.Fatal("an entry was DEAD-LETTERED because the connector does not implement outbound delivery yet -- " +
			"a connector filling in one direction at a time would lose every entry it was ever handed")
	}
	if engine.countContaining("markOutboxFailed") != 1 {
		t.Error("the entry was not parked for a later retry")
	}
	if _, _, dead := w.Counters(); dead != 0 {
		t.Errorf("dead counter = %d, want 0", dead)
	}
}

// Exactly one replica drains a connector: a worker that loses the claim
// does nothing at all, not even a read.
func TestAReplicaThatLosesTheClaimDrainsNothing(t *testing.T) {
	engine := newFakeEngine()
	engine.seed(`query outboxPending`, []map[string]any{outboxRow("e1", "shopify", 0, "")})
	c := &fakeConnector{name: "shopify"}
	claimer := &neverClaim{}
	w := testWorker(engine, claimer, c, testNow)

	w.DrainOnce(context.Background())

	if claimer.calls != 1 {
		t.Fatalf("the claim was attempted %d times, want 1", claimer.calls)
	}
	if c.propagateCount() != 0 {
		t.Error("a replica without the claim delivered anyway -- two replicas would double-deliver")
	}
	if engine.countContaining("outboxPending") != 0 {
		t.Error("a replica without the claim still read the queue; the claim is meant to come first")
	}
}

// The batch size bounds one pass, so a long queue does not hold the
// cluster claim indefinitely.
func TestOnePassIsBoundedByTheBatchSize(t *testing.T) {
	engine := newFakeEngine()
	var rows []map[string]any
	for i := 0; i < 10; i++ {
		rows = append(rows, outboxRow(string(rune('a'+i)), "shopify", 0, ""))
	}
	engine.seed(`query outboxPending`, rows)
	c := &fakeConnector{name: "shopify"}
	w := testWorker(engine, &alwaysClaim{}, c, testNow)
	w.cfg.BatchSize = 3

	w.DrainOnce(context.Background())

	if got := c.propagateCount(); got != 3 {
		t.Errorf("one pass delivered %d entries, want the batch size 3", got)
	}
}

// A worker with the runtime disabled appends nothing and delivers
// nothing -- and says so rather than looking healthy.
func TestADisabledWorkerDoesNotStart(t *testing.T) {
	engine := newFakeEngine()
	c := &fakeConnector{name: "shopify"}
	w := testWorker(engine, &alwaysClaim{}, c, testNow)
	w.cfg.Enabled = false

	w.Start(context.Background())
	<-w.Ready()
	defer w.Stop(context.Background())

	if w.IsRunning() {
		t.Error("a disabled worker reports itself running")
	}
	if engine.countContaining("outboxPending") != 0 {
		t.Error("a disabled worker read the queue")
	}
}

// Due() reads an absent nextAttemptAt as DUE. A fresh pending entry has
// never been tried, and reading absence as "not due" would deliver
// nothing, ever.
func TestAnEntryWithNoScheduledRetryIsDue(t *testing.T) {
	if !(OutboxEntry{}).Due(testNow) {
		t.Error("an entry with no nextAttemptAt is not due -- nothing would ever be delivered on the first pass")
	}
	if (OutboxEntry{NextAttemptAt: testNow.Add(time.Minute)}).Due(testNow) {
		t.Error("an entry scheduled in the future reported itself due")
	}
	if !(OutboxEntry{NextAttemptAt: testNow.Add(-time.Minute)}).Due(testNow) {
		t.Error("an entry whose retry time has passed reported itself not due")
	}
}
