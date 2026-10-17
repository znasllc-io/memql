package workbench

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

var errWorkbenchQueueFull = errors.New("Workbench is at capacity and its waiting queue is full; no work started")

// capacityGate belongs to the executor, not its caller. All forwarded and
// local commands and package builds share it on this replica. Pipeline Jobs
// have their own Kubernetes admission and never consume these user slots.
// Queue entries hold no workspace or child process. Cancellation removes them
// before a slot can be granted; a grant is retained until child cleanup ends.
type capacityGate struct {
	mu                       sync.Mutex
	active, limit, maxQueued int
	waiting                  []*capacityWaiter
	changed                  chan struct{}
	pressure                 func() (bool, error)
}

type capacityWaiter struct{ weight int }

func newCapacityGate(limit, queued int, pressure func() (bool, error)) *capacityGate {
	return &capacityGate{limit: limit, maxQueued: queued, pressure: pressure, changed: make(chan struct{})}
}

func configuredCapacity() *capacityGate {
	// Invalid settings tighten to the default instead of disabling admission.
	read := func(name string, fallback, maximum int) int {
		n, err := strconv.Atoi(os.Getenv(name))
		if err != nil || n < 1 || n > maximum {
			return fallback
		}
		return n
	}
	return newCapacityGate(read("MEMQL_WORKBENCH_MAX_ACTIVE", 2, 32), read("MEMQL_WORKBENCH_MAX_QUEUED", 32, 256), workbenchMemoryPressure)
}

func (g *capacityGate) signal() { close(g.changed); g.changed = make(chan struct{}) }

func (g *capacityGate) acquire(ctx context.Context, exclusive bool) (func(), error) {
	return g.acquireObserved(ctx, exclusive, nil)
}

func (g *capacityGate) acquireObserved(ctx context.Context, exclusive bool, onWait func() error) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	g.mu.Lock()
	weight := 1
	if exclusive {
		weight = g.limit
	}
	w := &capacityWaiter{weight: weight}
	// A zero-length queue still permits immediately available work.
	if len(g.waiting) >= g.maxQueued && (g.active+weight > g.limit || len(g.waiting) > 0) {
		g.mu.Unlock()
		return nil, errWorkbenchQueueFull
	}
	g.waiting = append(g.waiting, w)
	g.mu.Unlock()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	remove := func() {
		for n, candidate := range g.waiting {
			if candidate == w {
				g.waiting = append(g.waiting[:n], g.waiting[n+1:]...)
				g.signal()
				return
			}
		}
	}
	for {
		g.mu.Lock()
		if err := ctx.Err(); err != nil {
			remove()
			g.mu.Unlock()
			return nil, err
		}
		if g.waiting[0] == w && g.active+weight <= g.limit {
			pressure, err := g.pressure()
			if err != nil {
				remove()
				g.mu.Unlock()
				return nil, fmt.Errorf("Workbench memory capacity could not be verified: %w", err)
			}
			if !pressure {
				g.active += weight
				remove()
				g.mu.Unlock()
				var once sync.Once
				return func() { once.Do(func() { g.mu.Lock(); g.active -= weight; g.signal(); g.mu.Unlock() }) }, nil
			}
		}
		wake := g.changed
		g.mu.Unlock()
		if onWait != nil {
			notify := onWait
			onWait = nil
			if err := notify(); err != nil {
				g.mu.Lock()
				remove()
				g.mu.Unlock()
				return nil, err
			}
			continue
		}
		select {
		case <-ctx.Done():
		case <-wake:
		case <-tick.C:
		}
	}
}

// Admit no new process above 80% of the container's memory limit. The pod's
// cgroup hard limit remains the final boundary; this cushion is an admission
// threshold, not a promise that an already-running process cannot grow.
func workbenchMemoryPressure() (bool, error) {
	read := func(path string) (uint64, bool, error) {
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			return 0, false, nil
		}
		if err != nil {
			return 0, true, err
		}
		value := strings.TrimSpace(string(data))
		if value == "max" {
			return 0, true, nil
		}
		n, err := strconv.ParseUint(value, 10, 64)
		return n, true, err
	}
	limit, present, err := read("/sys/fs/cgroup/memory.max")
	usagePath := "/sys/fs/cgroup/memory.current"
	if !present && err == nil {
		limit, present, err = read("/sys/fs/cgroup/memory/memory.limit_in_bytes")
		usagePath = "/sys/fs/cgroup/memory/memory.usage_in_bytes"
	}
	if err != nil {
		return false, err
	}
	if !present || limit == 0 || limit >= 1<<60 {
		return false, nil
	} // Non-container developer host.
	used, present, err := read(usagePath)
	if err != nil {
		return false, err
	}
	if !present {
		return false, fmt.Errorf("memory usage is unavailable")
	}
	return used >= limit/5*4, nil
}

func (i *Integration) acquireCapacity(ctx context.Context, exclusive bool) (func(), error) {
	return i.acquireCapacityObserved(ctx, exclusive, nil)
}

func (i *Integration) acquireCapacityObserved(ctx context.Context, exclusive bool, onWait func() error) (func(), error) {
	i.capacityOnce.Do(func() {
		if i.capacity == nil {
			i.capacity = configuredCapacity()
		}
	})
	return i.capacity.acquireObserved(ctx, exclusive, onWait)
}
