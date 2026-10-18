package modelcall

import "sync"

// Limiter is THIS MACHINE's model concurrency ceiling, and there is one of
// it however many clusters the machine serves (memql-cockpit#432).
//
// A multi-home worker runs one Manager per cluster, so that one cluster's
// disconnect cannot end another cluster's generations. The ceilings are not
// the cluster's to hold, though -- they are a claim about this hardware, and
// every home's Register advertises the SAME numbers. With a ceiling per
// Manager, two clusters could each drive the full cap at once: N homes, N
// times the GPU the machine said it had. The Limiter is where the Managers
// meet, so the advertisement stays the whole truth about the hardware and
// the enforcement is the whole machine's.
//
// It is a SEMAPHORE: a call that finds the machine full waits for a slot
// (Manager.admit) rather than being refused, because the engine does not
// reroute a call a worker refuses -- it fails it.
//
// What each Manager keeps to itself is its own live set: StopAll on one
// home's disconnect still ends only that home's calls. The Limiter holds
// nothing but counts.
//
// It counts SLOTS -- calls whose model resolved and was admitted -- not
// calls merely registered. A call being resolved has taken nothing yet, and
// counting it would make two calls that arrive together against a ceiling
// of one each see the other and both wait for a slot neither holds.
type Limiter struct {
	mu       sync.Mutex
	perModel map[string]int
	total    int
	// freed is closed and replaced on every release: a waiter takes the
	// current channel, re-checks, and wakes when it closes. A broadcast
	// rather than a sync.Cond so a waiter can also select on its own
	// deadline and cancellation.
	freed chan struct{}
}

// NewLimiter builds an empty ceiling. Share one across every Manager on
// the machine.
func NewLimiter() *Limiter {
	return &Limiter{perModel: make(map[string]int), freed: make(chan struct{})}
}

// tryAcquire takes one slot for modelID if both ceilings allow it.
// modelCap and machineCap are the ADVERTISED numbers, read from the
// inventory at admission; zero means that ceiling is not stated and does
// not bind.
func (l *Limiter) tryAcquire(modelID string, modelCap, machineCap int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if modelCap > 0 && l.perModel[modelID] >= modelCap {
		return false
	}
	if machineCap > 0 && l.total >= machineCap {
		return false
	}
	l.perModel[modelID]++
	l.total++
	return true
}

// changed is the channel the next release closes.
func (l *Limiter) changed() <-chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.freed
}

// release gives back the slot tryAcquire took for modelID, and wakes
// every waiter to re-check.
func (l *Limiter) release(modelID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if n := l.perModel[modelID]; n <= 1 {
		delete(l.perModel, modelID)
	} else {
		l.perModel[modelID] = n - 1
	}
	if l.total > 0 {
		l.total--
	}
	close(l.freed)
	l.freed = make(chan struct{})
}

// InFlightByModel reports the admitted calls per model id, across every
// Manager sharing this Limiter -- what the capacity report says each loaded
// model is busy with.
func (l *Limiter) InFlightByModel() map[string]int {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]int, len(l.perModel))
	for id, n := range l.perModel {
		if n > 0 {
			out[id] = n
		}
	}
	return out
}

// InFlight reports how many admitted calls hold a slot, across every
// Manager sharing this Limiter.
func (l *Limiter) InFlight() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.total
}
