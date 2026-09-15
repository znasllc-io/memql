package automations

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// ModeRefusal describes a fire refused by the process-local concurrency mode.
type ModeRefusal struct {
	Automation, Mode, InFlightRunId string
	Max                             int
}

func (r *ModeRefusal) Error() string {
	return fmt.Sprintf("mode %s: automation %s has run %s in flight (max %d); this fire is refused [mode_refused]", r.Mode, r.Automation, r.InFlightRunId, r.Max)
}

type modeWaiter struct {
	id     string
	ready  chan struct{}
	cancel context.CancelFunc
}
type modeState struct {
	active  map[string]context.CancelFunc
	waiting []*modeWaiter
}
type modeGate struct {
	mu     sync.Mutex
	byName map[string]*modeState
}

var sharedModeGate = &modeGate{}

// Authored names are local to their owner; they must never cancel or block
// another owner's same-name automation or a shipped definition.
func automationModeIdentity(a *Automation) string {
	if strings.HasPrefix(a.Origin, "authored:") {
		return a.Origin + "\x00" + a.Name
	}
	return a.Name
}

type modeAncestryKey struct{}
type modeAncestor struct{ identity, runID string }

// This context ancestry represents synchronous callers still waiting for us.
// Event causes alone do not: asynchronous child events may safely queue until
// their publisher returns, so causal ancestry must not reject those fires.
func modeRunContext(ctx context.Context, identity, runID string) context.Context {
	prior, _ := ctx.Value(modeAncestryKey{}).([]modeAncestor)
	chain := make([]modeAncestor, len(prior), len(prior)+1)
	copy(chain, prior)
	return context.WithValue(ctx, modeAncestryKey{}, append(chain, modeAncestor{identity, runID}))
}

// acquire queues before the executor semaphore, so waiters occupy no execution slot.
func (g *modeGate) acquire(ctx context.Context, a *Automation, id string) (context.Context, func(), error) {
	if a.Mode == nil {
		return ctx, func() {}, nil
	}
	identity := automationModeIdentity(a)
	run, cancel := context.WithCancel(modeRunContext(ctx, identity, id))
	g.mu.Lock()
	if g.byName == nil {
		g.byName = map[string]*modeState{}
	}
	s := g.byName[identity]
	if s == nil {
		s = &modeState{active: map[string]context.CancelFunc{}}
		g.byName[identity] = s
	}
	kind, max := a.Mode.Kind, a.Mode.Max
	if kind == "queued" && max == 0 {
		max = envIntDefault("MEMQL_AUTOMATION_QUEUED_MODE_DEFAULT_MAX", 10)
		if max < 1 {
			max = 10
		}
	}
	refused := false
	switch kind {
	case "single":
		refused = len(s.active) > 0
	case "parallel":
		refused = max > 0 && len(s.active) >= max
	case "queued":
		refused = len(s.active) > 0 && len(s.waiting) >= max
		ancestors, _ := ctx.Value(modeAncestryKey{}).([]modeAncestor)
		for _, ancestor := range ancestors {
			if ancestor.identity == identity {
				if _, active := s.active[ancestor.runID]; active {
					refused = true
					break
				}
			}
		}
	case "restart":
		for _, stop := range s.active {
			stop()
		}
		s.active = map[string]context.CancelFunc{}
	}
	if refused {
		inFlight := ""
		for key := range s.active {
			if inFlight == "" || key < inFlight {
				inFlight = key
			}
		}
		g.mu.Unlock()
		cancel()
		return nil, nil, &ModeRefusal{a.Name, kind, inFlight, max}
	}
	release := func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		cancel()
		if _, ok := s.active[id]; !ok {
			return
		}
		delete(s.active, id)
		if len(s.active) == 0 && len(s.waiting) > 0 {
			w := s.waiting[0]
			s.waiting = s.waiting[1:]
			s.active[w.id] = w.cancel
			close(w.ready)
		}
		if len(s.active) == 0 && len(s.waiting) == 0 {
			delete(g.byName, identity)
		}
	}
	if kind == "queued" && len(s.active) > 0 {
		w := &modeWaiter{id, make(chan struct{}), cancel}
		s.waiting = append(s.waiting, w)
		g.mu.Unlock()
		select {
		case <-w.ready:
			if err := ctx.Err(); err != nil {
				release()
				return nil, nil, err
			}
			return run, release, nil
		case <-ctx.Done():
			g.mu.Lock()
			for i, v := range s.waiting {
				if v == w {
					s.waiting = append(s.waiting[:i], s.waiting[i+1:]...)
					break
				}
			}
			g.mu.Unlock()
			release()
			return nil, nil, ctx.Err()
		}
	}
	s.active[id] = cancel
	g.mu.Unlock()
	return run, release, nil
}
