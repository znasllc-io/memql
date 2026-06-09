package node

import (
	"testing"
	"time"
)

// newTestDedup builds a time-windowed dedup with a controllable clock.
func newTestDedup(ttl time.Duration, clock *time.Time) *eventDedup {
	d := newEventDedup(ttl)
	d.now = func() time.Time { return *clock }
	return d
}

func TestEventDedup_BasicCheck(t *testing.T) {
	now := time.Unix(1000, 0)
	d := newTestDedup(time.Minute, &now)

	if d.Check("evt-1") {
		t.Error("first check of evt-1 should return false (not seen)")
	}
	if !d.Check("evt-1") {
		t.Error("second check of evt-1 should return true (seen within window)")
	}
}

func TestEventDedup_Contains(t *testing.T) {
	now := time.Unix(1000, 0)
	d := newTestDedup(time.Minute, &now)
	d.Check("evt-1")
	if !d.Contains("evt-1") {
		t.Error("Contains should return true for a recorded event")
	}
	if d.Contains("evt-2") {
		t.Error("Contains should return false for an unknown event")
	}
}

// The defining property #1155 needs: an id seen within the window is ALWAYS a
// duplicate, no matter how many OTHER distinct events arrive in between. The
// old fixed-size ring failed exactly here -- volume evicted the id and it was
// re-admitted, re-triggering the storm.
func TestEventDedup_VolumeDoesNotEvictWithinWindow(t *testing.T) {
	now := time.Unix(1000, 0)
	d := newTestDedup(time.Minute, &now)

	d.Check("plan-evt") // record the plan event id
	// 100k distinct OTHER events churn through within the window.
	for i := 0; i < 100000; i++ {
		d.Check("noise-" + string(rune(i%256)) + "-" + time.Duration(i).String())
	}
	// The plan event id, seen moments ago, must STILL be deduped.
	now = now.Add(5 * time.Second)
	if !d.Check("plan-evt") {
		t.Fatal("an id seen within the window must stay deduped regardless of volume (the #1155 bug: fixed-ring eviction re-admitted it)")
	}
}

// After the TTL elapses, an id is forgotten and admitted again (so a genuinely
// new occurrence much later is not falsely suppressed).
func TestEventDedup_ExpiresAfterTTL(t *testing.T) {
	now := time.Unix(1000, 0)
	d := newTestDedup(60*time.Second, &now)

	if d.Check("evt-1") {
		t.Fatal("first sighting should be admitted")
	}
	now = now.Add(30 * time.Second)
	if !d.Check("evt-1") {
		t.Fatal("within the window it must still be deduped")
	}
	now = now.Add(61 * time.Second) // past the window from the last sighting
	if d.Check("evt-1") {
		t.Fatal("after the window elapses the id must be admitted again")
	}
}

// The sweep keeps the map bounded: expired entries are dropped, so memory is
// bounded by rate*ttl rather than growing forever.
func TestEventDedup_SweepBoundsMemory(t *testing.T) {
	now := time.Unix(1000, 0)
	d := newTestDedup(60*time.Second, &now)

	for i := 0; i < 1000; i++ {
		d.Check("old-" + time.Duration(i).String())
	}
	// Jump well past the window, then record one more (triggers a sweep).
	now = now.Add(5 * time.Minute)
	d.Check("fresh")

	d.mu.Lock()
	size := len(d.seen)
	d.mu.Unlock()
	if size > 1 {
		t.Fatalf("expired entries should have been swept; want ~1, got %d", size)
	}
}
