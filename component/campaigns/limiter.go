package campaigns

import (
	"sync"
	"time"
)

// limiter.go -- OUR rate limit. The provider's is a separate mechanism
// (integrations/email/senderror.go classifies the 429; worker.go parks the
// job until its Retry-After passes), and the issue is explicit that both
// are needed.
//
// # Why both, when either alone looks sufficient
//
// Reacting to 429s alone means DISCOVERING the limit by exceeding it, on
// every send, forever. Each discovery costs a refused request, and a
// refused request is not free: some providers count it against the same
// quota, and a send loop that treats "refused" as "retry now" turns one
// throttle into a storm the recipient eventually sees as duplicates.
//
// Pacing alone means never adapting: the provider's limit varies by
// mailbox, tenant and time of day, and a fixed local rate is either too
// slow (most of the time) or still too fast (exactly when it matters).
//
// So: pace to stay under the expected limit, and obey the provider when
// the expectation is wrong.
//
// # Why a token bucket and not a sleep between sends
//
// A bucket allows a burst up to its capacity after an idle period, which
// is what makes a small campaign go out promptly instead of being drip-fed
// at the steady-state rate. It is also what a provider's own limiter
// looks like, so pacing against the same shape is more accurate than
// pacing against a smooth one.
//
// # Scope: per process, deliberately
//
// This is not a cluster-wide limit. Every replica has its own bucket, so N
// replicas can emit N times the configured rate -- and that is correct for
// what it defends, because the CROSS-REPLICA bound is the send claim: only
// one replica drains a given campaign at a time, so a single campaign is
// paced by exactly one bucket. What N replicas can exceed is the aggregate
// across DIFFERENT campaigns, which is the case the provider's own 429
// handling covers. A distributed limiter would need a shared counter on
// the hot path of every message; the claim already buys the property that
// matters.

// rateLimiter is a token bucket refilled continuously at ratePerMinute.
type rateLimiter struct {
	mu       sync.Mutex
	capacity float64
	tokens   float64
	perSec   float64
	last     time.Time
	now      func() time.Time // test seam
}

// newRateLimiter builds a bucket at the given rate. Capacity is one
// minute's worth, so an idle worker may send a minute's allowance at once
// and then settles to the steady rate.
func newRateLimiter(ratePerMinute int, now func() time.Time) *rateLimiter {
	if ratePerMinute <= 0 {
		ratePerMinute = 1
	}
	if now == nil {
		now = time.Now
	}
	capacity := float64(ratePerMinute)
	return &rateLimiter{
		capacity: capacity,
		tokens:   capacity,
		perSec:   capacity / 60,
		last:     now(),
		now:      now,
	}
}

// Allow consumes one token, reporting whether the send may proceed.
// Non-blocking: the caller ends its batch rather than sleeping, so the
// cross-replica claim is released promptly and a stalled batch cannot
// hold a campaign hostage for the length of its lease.
func (l *rateLimiter) Allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if elapsed := now.Sub(l.last); elapsed > 0 {
		l.tokens += elapsed.Seconds() * l.perSec
		if l.tokens > l.capacity {
			l.tokens = l.capacity
		}
		l.last = now
	}
	if l.tokens < 1 {
		return false
	}
	l.tokens--
	return true
}
