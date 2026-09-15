package automations

import (
	"context"
	"fmt"
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

// acquire queues before the executor semaphore, so waiters occupy no execution slot.
func (g *modeGate) acquire(ctx context.Context, a *Automation, id string) (context.Context, func(), error) {
	if a.Mode == nil {
		return ctx, func() {}, nil
	}
	run, cancel := context.WithCancel(ctx)
	g.mu.Lock()
	if g.byName == nil {
		g.byName = map[string]*modeState{}
	}
	s := g.byName[a.Name]
	if s == nil {
		s = &modeState{active: map[string]context.CancelFunc{}}
		g.byName[a.Name] = s
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
			delete(g.byName, a.Name)
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
