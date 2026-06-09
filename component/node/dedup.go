package node

import (
	"sync"
	"time"
)

// eventDedup suppresses recently-seen event IDs so the cross-node mesh does
// not re-process / re-forward the same event. It is TIME-WINDOWED
// (memql#1155): an event id seen within `ttl` is ALWAYS reported as a
// duplicate, regardless of how many other events have arrived since.
//
// This replaced a fixed-size ring buffer (8192 slots, FIFO eviction). Under
// high event volume the ring could evict an id and then RE-ADMIT it when a
// relayed copy of the same event arrived a moment later -- re-triggering local
// subscribers (e.g. the planner's HandlePlanCreated) and feeding a self-
// sustaining event/automation storm around a single plan. A time window can't
// be defeated by volume: once an id is recorded it stays deduped for the whole
// window no matter how many distinct events churn through. Thread-safe.
type eventDedup struct {
	mu        sync.Mutex
	ttl       time.Duration
	seen      map[string]time.Time // event id -> first-seen time
	now       func() time.Time     // injectable for tests; defaults to time.Now
	lastSweep time.Time
}

// newEventDedup creates a dedup window with the given TTL. A non-positive ttl
// falls back to defaultDedupTTL.
func newEventDedup(ttl time.Duration) *eventDedup {
	if ttl <= 0 {
		ttl = defaultDedupTTL
	}
	return &eventDedup{
		ttl:  ttl,
		seen: make(map[string]time.Time),
		now:  time.Now,
	}
}

// Check returns true if the event ID was seen within the TTL window. If it is
// new (or its prior sighting has expired) it records the id at the current
// time and returns false.
func (d *eventDedup) Check(eventId string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	if seenAt, ok := d.seen[eventId]; ok && now.Sub(seenAt) < d.ttl {
		return true
	}
	d.seen[eventId] = now
	d.sweepLocked(now)
	return false
}

// Contains reports whether the event ID is currently within the dedup window.
func (d *eventDedup) Contains(eventId string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	seenAt, ok := d.seen[eventId]
	return ok && d.now().Sub(seenAt) < d.ttl
}

// sweepLocked drops expired entries, at most once per ttl, so the map can't
// grow unbounded across many distinct events. Caller holds d.mu.
func (d *eventDedup) sweepLocked(now time.Time) {
	if !d.lastSweep.IsZero() && now.Sub(d.lastSweep) < d.ttl {
		return
	}
	for k, t := range d.seen {
		if now.Sub(t) >= d.ttl {
			delete(d.seen, k)
		}
	}
	d.lastSweep = now
}
