package memql

// The bound on hold-line writes (final review, Important 2).
//
// WHAT TRIGGERS IT. Every hold-line WRITE desktopAccess would make: a new
// reference (the caller passed back no valid one) and a rewrite under a
// passed reference whose reasons changed, alike. The steady state, a valid
// passed reference whose reasons match, writes nothing and never asks, so a
// held page checking every 30 seconds spends nothing.
//
// WHY IT EXISTS. The line goes through logstore's InsertNow, which skips the
// log store's queue, level floor and per-second bucket so that a reference a
// person reads out can always be found. The builtin is open to every
// signed-in caller with no capability, and the server does not rate-limit the
// stream, so without a bound any held account, or an SDK caller in a loop,
// could write one log_line row per call (kept 30 days) and pay a cluster-wide
// registration read for each.
//
// THE BOUND: a burst of accessHoldBurst writes per person, then one every
// accessHoldRefill, as a token bucket refilled continuously. Past it,
// desktopAccess writes nothing and answers with the reference and digest the
// caller passed when the reference is valid for them, else with neither.
//
// WHY REPLICA-LOCAL IS ENOUGH. Its job is to keep one caller from filling
// log_line, and a per-replica bound does that, as the edge's per-replica
// limits do; N replicas allow N times the rate, which is acceptable. Nothing is
// shared or persisted, so nothing survives a replica change: another replica,
// or this one after a restart, starts the person with a full bucket.
//
// WHAT IT COSTS. A person held past the bound gets no new line until their
// bucket refills: with a valid reference they keep it, and their next check
// after the refill writes the changed reasons; without one they see the page
// with no reference until then. Memory is one entry per person who wrote
// recently: an entry is dropped once it has been idle long enough for its
// bucket to be full again (it then says nothing a fresh entry would not), and
// there are at most accessHoldBoundMaxPeople entries, about a megabyte. A
// person who would be one past the cap while every entry is still refilling is
// treated as past the bound: the cap is reached only when that many people
// were held within a few minutes on one replica, and writing nothing is the
// bound's own answer then.
//
// Tests: desktop_access_bound_test.go.

import (
	"sync"
	"time"
)

const (
	// accessHoldBurst is how many hold lines one person can have written at
	// once on a replica.
	accessHoldBurst = 3
	// accessHoldRefill is how often a further line is allowed after the burst.
	accessHoldRefill = time.Minute
	// accessHoldBoundMaxPeople caps the limiter's entries: about 100 bytes
	// each, so about a megabyte at the cap.
	accessHoldBoundMaxPeople = 10000
)

// accessHoldLimiter is the per-person token bucket, kept as the generic cell
// rate algorithm: one time per person, the moment their bucket is full again
// ("full at"). Integer time, so a write exactly one refill interval later is
// allowed exactly then. The clock is injectable so a test can move time.
type accessHoldLimiter struct {
	now   func() time.Time
	burst int
	every time.Duration
	max   int

	mu     sync.Mutex
	fullAt map[string]time.Time
	swept  time.Time
}

func newAccessHoldLimiter(now func() time.Time) *accessHoldLimiter {
	if now == nil {
		now = time.Now
	}
	return &accessHoldLimiter{now: now, burst: accessHoldBurst, every: accessHoldRefill, max: accessHoldBoundMaxPeople, fullAt: map[string]time.Time{}}
}

// accessHoldBound is this engine's limiter: one per process, which is one per
// replica.
func (e *MemQLEngine) accessHoldBound() *accessHoldLimiter {
	if l := e.accessHolds.Load(); l != nil {
		return l
	}
	e.accessHolds.CompareAndSwap(nil, newAccessHoldLimiter(time.Now))
	return e.accessHolds.Load()
}

// allow spends one write for person, or reports that they are past the bound
// and changes nothing.
//
// A person's bucket is full again at fullAt; each write moves that one refill
// interval later. A write is allowed while fullAt is at most (burst - 1)
// intervals ahead of now, which is "at least one token left".
func (l *accessHoldLimiter) allow(person string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	// Forget the people whose buckets are full again, at most once a refill
	// interval (or when the cap is reached), so the map holds only people who
	// wrote recently.
	if now.Sub(l.swept) >= l.every || len(l.fullAt) >= l.max {
		l.sweepLocked(now)
	}
	full, known := l.fullAt[person]
	if !known && len(l.fullAt) >= l.max {
		return false
	}
	if full.Before(now) {
		full = now
	}
	if full.Sub(now) > time.Duration(l.burst-1)*l.every {
		return false
	}
	l.fullAt[person] = full.Add(l.every)
	return true
}

// sweepLocked drops every entry whose bucket is full again.
func (l *accessHoldLimiter) sweepLocked(now time.Time) {
	for person, full := range l.fullAt {
		if !full.After(now) {
			delete(l.fullAt, person)
		}
	}
	l.swept = now
}

// size is how many people the limiter remembers, for tests.
func (l *accessHoldLimiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.fullAt)
}
