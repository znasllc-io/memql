package planner

import (
	"testing"
	"time"
)

func newTestHandledSet(ttl time.Duration, clock *time.Time) *handledPlanSet {
	s := newHandledPlanSet(ttl)
	s.now = func() time.Time { return *clock }
	return s
}

// The core #1155 property: a plan is dispatched ONCE; re-delivered created
// events for the same id are skipped.
func TestHandledPlanSet_HandleOnce(t *testing.T) {
	now := time.Unix(1000, 0)
	s := newTestHandledSet(30*time.Minute, &now)

	if !s.markIfNew("plan-1") {
		t.Fatal("first sighting of plan-1 must be admitted")
	}
	for i := 0; i < 2588; i++ { // the live storm count
		if s.markIfNew("plan-1") {
			t.Fatalf("re-delivery #%d of plan-1 must be skipped, not handled again", i)
		}
	}
	// A different plan is unaffected.
	if !s.markIfNew("plan-2") {
		t.Fatal("a different plan must be handled")
	}
}

func TestHandledPlanSet_ExpiresAfterTTL(t *testing.T) {
	now := time.Unix(1000, 0)
	s := newTestHandledSet(60*time.Second, &now)
	s.markIfNew("plan-1")
	now = now.Add(61 * time.Second)
	if !s.markIfNew("plan-1") {
		t.Fatal("after the ttl elapses the plan may be handled again")
	}
}

func TestHandledPlanSet_NilAndEmpty_FailOpen(t *testing.T) {
	var s *handledPlanSet
	if !s.markIfNew("plan-1") {
		t.Fatal("nil set must fail open (return true)")
	}
	now := time.Unix(1000, 0)
	s = newTestHandledSet(time.Minute, &now)
	if !s.markIfNew("") {
		t.Fatal("empty plan id must fail open (return true)")
	}
}
