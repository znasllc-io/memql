package planner

import (
	"sync"
	"time"
)

// defaultHandledPlanTTL is how long a plan id is remembered as "already
// dispatched". The re-delivery window is seconds; 30 minutes is generous
// headroom and bounds memory. A planner restart starts clean, which is safe:
// old plan-created events are not re-delivered after a restart.
const defaultHandledPlanTTL = 30 * time.Minute

// handledPlanSet records plan ids whose plan-created event the loop has
// already dispatched, so a RE-DELIVERED created event (cross-node mesh re-
// delivery, memql#1155) does not re-run decompose/dispatch for the same plan.
// Entries expire after ttl so the set can't grow unbounded. Thread-safe.
type handledPlanSet struct {
	mu        sync.Mutex
	ttl       time.Duration
	at        map[string]time.Time
	now       func() time.Time // injectable for tests; defaults to time.Now
	lastSweep time.Time
}

func newHandledPlanSet(ttl time.Duration) *handledPlanSet {
	if ttl <= 0 {
		ttl = defaultHandledPlanTTL
	}
	return &handledPlanSet{ttl: ttl, at: map[string]time.Time{}, now: time.Now}
}

// markIfNew returns true if planId has NOT been handled within the ttl window
// (the caller should proceed) and records it; false if it is a duplicate
// re-delivery (the caller should skip). A nil set or empty id always returns
// true so the guard fails open.
func (s *handledPlanSet) markIfNew(planId string) bool {
	if s == nil || planId == "" {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if t, ok := s.at[planId]; ok && now.Sub(t) < s.ttl {
		return false
	}
	s.at[planId] = now
	if s.lastSweep.IsZero() || now.Sub(s.lastSweep) >= s.ttl {
		for k, v := range s.at {
			if now.Sub(v) >= s.ttl {
				delete(s.at, k)
			}
		}
		s.lastSweep = now
	}
	return true
}
