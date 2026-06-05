package automations

// authored_breaker.go -- the per-automation circuit breaker for the authored
// runtime (epic memql#954, issue #959, increment 3).
//
// An authored automation is user-authored code running live in the cluster. A
// misbehaving one (a logic step that always errors, an infinite-retry loop, a
// step that panics) must be FAULT-ISOLATED: its faults can never cascade to
// other owners' authored automations, to the same owner's OTHER automations,
// or to the sealed core. The mechanism is a breaker scoped to a single
// (owner, automation): consecutive failures trip it; on trip it auto-pauses
// THAT automation and surfaces the event, leaving everything else untouched.
//
// This is the per-automation analogue of the process-wide SI guard
// (component/memql/si_guard.go) -- same fail-isolate intent, but keyed per
// authored automation so one bad automation's runs can't starve the others.
//
// State machine (per (owner, name) key):
//
//	closed  --(consecutiveFailures >= threshold)-->  open (tripped)
//	closed  --(any success)-->                       closed (reset)
//	open    --(stays open until the automation is re-activated)
//
// When the breaker opens it calls onTrip, which the scheduler wires to pause
// the automation (stop it firing) and to surface the trip. A re-activation
// (Activate) clears the key, giving a fixed, just-edited automation a fresh
// start.

import (
	"sync"
	"time"
)

// BreakerTripInfo describes a breaker opening, handed to the onTrip callback
// and recorded for the management surface.
type BreakerTripInfo struct {
	Owner      string
	Automation string
	Failures   int
	LastError  string
	TrippedAt  time.Time
}

// BreakerOnTrip is invoked exactly once when an automation's breaker opens.
// The scheduler wires it to pause the offending automation and surface the
// fault. It must not block long (it runs inline on the failing run's
// goroutine) and must not itself fault the breaker -- it is the isolation
// boundary.
type BreakerOnTrip func(info BreakerTripInfo)

// breakerState is the per-automation breaker bookkeeping.
type breakerState struct {
	consecutiveFailures int
	open                bool
	lastError           string
	trippedAt           time.Time
}

// AuthoredBreaker holds one circuit breaker per (owner, automation). It is
// thread-safe; runs from different automations touch independent keys, so a
// storm of failures in one automation never contends with or trips another.
type AuthoredBreaker struct {
	threshold int
	onTrip    BreakerOnTrip

	mu     sync.Mutex
	states map[string]*breakerState
}

// DefaultBreakerThreshold is the consecutive-failure count at which an
// authored automation's breaker trips. Small on purpose: an authored
// automation that fails several times in a row is misbehaving, and pausing it
// early limits blast radius (wasted runs, log noise, downstream side effects).
const DefaultBreakerThreshold = 5

// NewAuthoredBreaker builds a breaker. threshold <= 0 uses
// DefaultBreakerThreshold. onTrip may be nil (the breaker still tracks state
// and reports IsOpen, just with no pause side-effect).
func NewAuthoredBreaker(threshold int, onTrip BreakerOnTrip) *AuthoredBreaker {
	if threshold <= 0 {
		threshold = DefaultBreakerThreshold
	}
	return &AuthoredBreaker{
		threshold: threshold,
		onTrip:    onTrip,
		states:    make(map[string]*breakerState),
	}
}

// Allow reports whether a firing for (owner, name) should run. A tripped
// (open) breaker returns false, so even if a stale subscription fires the run
// is suppressed -- belt-and-suspenders alongside the scheduler pausing it.
func (b *AuthoredBreaker) Allow(owner, name string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	st := b.states[breakerKey(owner, name)]
	return st == nil || !st.open
}

// RecordSuccess resets the consecutive-failure count for (owner, name). A
// success on a still-closed breaker clears any partial failure streak; it does
// NOT re-close an already-open breaker (that needs an explicit Reset, which
// re-activation performs) so a flapping automation can't auto-recover into a
// fault loop.
func (b *AuthoredBreaker) RecordSuccess(owner, name string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	key := breakerKey(owner, name)
	st := b.states[key]
	if st == nil || st.open {
		return
	}
	st.consecutiveFailures = 0
}

// RecordFailure increments the consecutive-failure count for (owner, name) and
// trips the breaker when it reaches the threshold. Returns true when THIS call
// is the one that opened the breaker (so the caller logs/surfaces it once). The
// onTrip callback fires once, outside the lock, on the tripping call.
func (b *AuthoredBreaker) RecordFailure(owner, name, errMsg string) bool {
	b.mu.Lock()
	key := breakerKey(owner, name)
	st := b.states[key]
	if st == nil {
		st = &breakerState{}
		b.states[key] = st
	}
	if st.open {
		// Already open -- a late in-flight run failing again doesn't re-trip.
		b.mu.Unlock()
		return false
	}
	st.consecutiveFailures++
	st.lastError = errMsg
	tripped := st.consecutiveFailures >= b.threshold
	var info BreakerTripInfo
	if tripped {
		st.open = true
		st.trippedAt = time.Now().UTC()
		info = BreakerTripInfo{
			Owner:      owner,
			Automation: name,
			Failures:   st.consecutiveFailures,
			LastError:  errMsg,
			TrippedAt:  st.trippedAt,
		}
	}
	b.mu.Unlock()

	if tripped && b.onTrip != nil {
		b.onTrip(info)
	}
	return tripped
}

// IsOpen reports whether (owner, name)'s breaker is tripped.
func (b *AuthoredBreaker) IsOpen(owner, name string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	st := b.states[breakerKey(owner, name)]
	return st != nil && st.open
}

// Reset clears the breaker state for (owner, name) -- a fresh start. The
// scheduler calls this on (re-)activation so a fixed automation runs again.
func (b *AuthoredBreaker) Reset(owner, name string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.states, breakerKey(owner, name))
}

// breakerKey scopes breaker state per (owner, automation) so faults are
// isolated to a single authored automation.
func breakerKey(owner, name string) string {
	return owner + "\x00" + name
}
