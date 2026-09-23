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
//
// IT ALSO REMEMBERS THE SHORTEST ROUTE SEEN (memql#5338, D3). Once every
// stream carries events both ways, copies of one event race each other along
// paths of different lengths, and the first to arrive is not always the
// shortest. observe reports a repeat that travelled FEWER hops than every copy
// before it, so the bridge can relay it again -- and the far side of the mesh
// is never starved of hop budget by a copy that won the race the long way
// round.
type eventDedup struct {
	mu        sync.Mutex
	ttl       time.Duration
	seen      map[string]dedupEntry
	now       func() time.Time // injectable for tests; defaults to time.Now
	lastSweep time.Time
}

// dedupEntry is one remembered event: when it was first seen, and the fewest
// hops any copy of it had travelled to reach this node.
type dedupEntry struct {
	at      time.Time
	minHops int32
}

// newEventDedup creates a dedup window with the given TTL. A non-positive ttl
// falls back to defaultDedupTTL.
func newEventDedup(ttl time.Duration) *eventDedup {
	if ttl <= 0 {
		ttl = defaultDedupTTL
	}
	return &eventDedup{
		ttl:  ttl,
		seen: make(map[string]dedupEntry),
		now:  time.Now,
	}
}

// observe records one sighting of eventId by a copy that has travelled hops
// links, and says what it is:
//
//   - first: the id was not seen within the window. Publish it and relay it.
//   - shorter: the id WAS seen, but never by a copy that travelled as few hops
//     as this one. Relay it again; do not publish it again.
//
// Both false is a plain duplicate. The window runs from the FIRST sighting;
// a shorter copy moves the remembered route, never the clock. The origin
// records its own event at hops 0, so no copy can come back to it shorter.
func (d *eventDedup) observe(eventId string, hops int32) (first, shorter bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	if e, ok := d.seen[eventId]; ok && now.Sub(e.at) < d.ttl {
		if hops < e.minHops {
			e.minHops = hops
			d.seen[eventId] = e
			return false, true
		}
		return false, false
	}
	d.seen[eventId] = dedupEntry{at: now, minHops: hops}
	d.sweepLocked(now)
	return true, false
}

// Check returns true if the event ID was seen within the TTL window. If it is
// new (or its prior sighting has expired) it records the id at the current
// time and returns false. The durable substrate's per-subscription windows
// ask only this question; routes are the mesh bridge's concern (observe).
func (d *eventDedup) Check(eventId string) bool {
	first, _ := d.observe(eventId, 0)
	return !first
}

// Contains reports whether the event ID is currently within the dedup window.
func (d *eventDedup) Contains(eventId string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.seen[eventId]
	return ok && d.now().Sub(e.at) < d.ttl
}

// sweepLocked drops expired entries, at most once per ttl, so the map can't
// grow unbounded across many distinct events. Caller holds d.mu.
func (d *eventDedup) sweepLocked(now time.Time) {
	if !d.lastSweep.IsZero() && now.Sub(d.lastSweep) < d.ttl {
		return
	}
	for k, e := range d.seen {
		if now.Sub(e.at) >= d.ttl {
			delete(d.seen, k)
		}
	}
	d.lastSweep = now
}
