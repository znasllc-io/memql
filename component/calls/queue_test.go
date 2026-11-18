package calls

import (
	"reflect"
	"testing"
	"time"
)

func TestOrderQueueRunsCallsFirstThenScheduledThenNew(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	in := []QueueEntry{
		{ID: "n1", Class: ClassNewCall, EnqueuedAt: t0},
		{ID: "s1", Class: ClassScheduledDue, EnqueuedAt: t0.Add(time.Second)},
		{ID: "r2", Class: ClassRunningCall, EnqueuedAt: t0.Add(time.Minute)},
		{ID: "r1b", Class: ClassRunningCall, EnqueuedAt: t0},
		{ID: "r1a", Class: ClassRunningCall, EnqueuedAt: t0},
	}
	got := OrderQueue(in)
	ids := []string{}
	for _, e := range got {
		ids = append(ids, e.ID)
	}
	if want := []string{"r1a", "r1b", "r2", "s1", "n1"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("got %v want %v", ids, want)
	}
	if in[0].ID != "n1" {
		t.Fatal("OrderQueue mutated its input")
	}
	if pos, ok := Position(got, "s1"); !ok || pos != 3 {
		t.Fatalf("position = %d,%v", pos, ok)
	}
	if _, ok := Position(got, "nope"); ok {
		t.Fatal("unknown id found")
	}
}

func TestExpiredEntriesUseLastPoll(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	ttl := 10 * time.Minute
	got := ExpiredEntries([]QueueEntry{
		{ID: "old-but-polling", EnqueuedAt: now.Add(-time.Hour), LastPollAt: now.Add(-time.Minute)},
		{ID: "silent", EnqueuedAt: now.Add(-time.Minute), LastPollAt: now.Add(-11 * time.Minute)},
		{ID: "boundary", LastPollAt: now.Add(-ttl)},
		{ID: "never-polled-fresh", EnqueuedAt: now.Add(-time.Minute)},
		{ID: "never-polled-stale", EnqueuedAt: now.Add(-20 * time.Minute)},
	}, now, ttl)
	if want := []string{"silent", "boundary", "never-polled-stale"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}
