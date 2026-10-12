package campaigns

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
)

// busy_gate_test.go -- a lock that was busy is "nothing attempted, try later", on
// every path that sends mail (memql#6022, F9 fix round 3).

var attemptsArg = regexp.MustCompile(`attempts:\s*(\d+)`)

// deadlineBeforeTimer models a socket deadline firing before context's timer
// publishes cancellation. Err is still nil even though the deadline is past.
type deadlineBeforeTimer struct {
	context.Context
	deadline time.Time
}

func (c deadlineBeforeTimer) Deadline() (time.Time, bool) { return c.deadline, true }

func TestSignupRecognizesADeadlineBeforeItsContextTimerRuns(t *testing.T) {
	phase := deadlineBeforeTimer{Context: context.Background(), deadline: time.Now().Add(-time.Second)}
	if phase.Err() != nil {
		t.Fatal("the regression needs a context whose timer has not run")
	}
	if !signupContextEnded(phase) {
		t.Fatal("a socket timeout at the phase deadline would leak instead of returning the signup retry message")
	}
	phase.deadline = time.Now().Add(time.Minute)
	if signupContextEnded(phase) || signupContextEnded(context.Background()) {
		t.Fatal("an unrelated transport error must not be treated as an expired signup phase")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if !signupContextEnded(canceled) {
		t.Fatal("a canceled caller must keep its own error instead of the signup retry message")
	}
}

// TestABusyLockIsNeitherAnAttemptNorAFailureForTheBulkDrain: a send that finds the
// pod's lock busy was never judged on its merits. It costs the recipient no attempt
// (three attempts end a delivery as failed, so a busy pod would fail mail that was
// never tried) and the job waits a moment, instead of spending the same busy lock
// on every recipient of the batch.
func TestABusyLockIsNeitherAnAttemptNorAFailureForTheBulkDrain(t *testing.T) {
	engine := &fakeEngine{
		jobs:     []map[string]any{jobRow()},
		campaign: campaignRow(),
		template: templateRow(),
		roster: []map[string]any{
			recipientRow("r-1", "a@example.test", "subscribed"),
			recipientRow("r-2", "b@example.test", "subscribed"),
		},
	}
	sender := &recordingSender{fail: func(int) error { return memql.ErrWriteGateBusy }}
	w := newTestWorker(t, engine, sender)
	frozen := time.Now().UTC()
	w.now = func() time.Time { return frozen }

	w.DrainOnce(context.Background())

	var parkedUntil string
	for _, c := range engine.mutations("updateSendJob") {
		if v := argOf(c.query, "throttledUntil"); v != "" {
			parkedUntil = v
		}
	}
	if want := frozen.Add(gateBusyRetry).Format(time.RFC3339); parkedUntil != want {
		t.Errorf("the job is parked until %q, want %q: a busy lock is a wait, and the job waits it out", parkedUntil, want)
	}
	pending := 0
	for _, c := range engine.mutations("recordCampaignDelivery") {
		switch argOf(c.query, "status") {
		case "failed":
			t.Errorf("a recipient was recorded as FAILED for a lock that was busy: %s", c.query)
		case "pending":
			pending++
			if m := attemptsArg.FindStringSubmatch(c.query); m != nil && m[1] != "0" {
				t.Errorf("a busy lock spent an attempt (%s): %s", m[1], c.query)
			}
		}
	}
	if pending == 0 {
		t.Error("the recipient the busy lock refused left no retryable row")
	}
	if got := sender.count(); got != 0 {
		t.Errorf("%d messages were sent through a busy lock", got)
	}
	if len(engine.mutations("recordCampaignDelivery")) > 1 {
		t.Errorf("the batch went on to the next recipient after a busy lock: %d delivery rows", len(engine.mutations("recordCampaignDelivery")))
	}
}
