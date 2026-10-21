package worker

import (
	"strings"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// Capacity is what a machine can take on right now (component/memql's
// fleet_capacity.go holds the type and the admission it feeds).
type Capacity = memqlengine.FleetCapacity

// Bounds on what one report may carry. A report is a cockpit's word about
// itself, so nothing in it is trusted to be small.
const (
	maxCapacityModelIdLen = 256
	maxCapacityRuntimeLen = 32
)

// CapacityFromProto reads a cockpit's capacity report, stamped with when this
// replica received it. A malformed part is dropped rather than refused: the
// report steers admission, and a live stream is not worth losing over it.
func CapacityFromProto(p *memqlv1.Capacity, receivedAt time.Time) Capacity {
	if p == nil || p.GetMemoryTotalBytes() == 0 {
		return Capacity{}
	}
	c := Capacity{
		MemoryTotalBytes: p.GetMemoryTotalBytes(),
		MemoryFreeBytes:  p.GetMemoryFreeBytes(),
		MemoryKind:       strings.TrimSpace(p.GetMemoryKind()),
		ReportedAt:       receivedAt.UTC(),
	}
	// Free memory beyond the pool is a corrupt reading, not a claim.
	if c.MemoryFreeBytes > c.MemoryTotalBytes {
		c.MemoryFreeBytes = c.MemoryTotalBytes
	}
	if !memqlengine.ValidFleetMemoryKind(c.MemoryKind) {
		c.MemoryKind = ""
	}
	if ts := p.GetMeasuredAt(); ts != nil && ts.IsValid() {
		c.MeasuredAt = ts.AsTime().UTC()
	}
	for _, m := range p.GetLoadedModels() {
		if len(c.Loaded) >= memqlengine.MaxFleetLoadedModels {
			break
		}
		model := strings.TrimSpace(m.GetModel())
		if model == "" || len(model) > maxCapacityModelIdLen {
			continue
		}
		runtime := strings.TrimSpace(m.GetRuntime())
		if len(runtime) > maxCapacityRuntimeLen {
			runtime = runtime[:maxCapacityRuntimeLen]
		}
		entry := memqlengine.FleetLoadedModel{
			Model:       model,
			Runtime:     runtime,
			SizeBytes:   m.GetSizeBytes(),
			ActiveCalls: int(m.GetActiveCalls()),
		}
		if ts := m.GetExpiresAt(); ts != nil && ts.IsValid() {
			entry.ExpiresAt = ts.AsTime().UTC()
		}
		c.Loaded = append(c.Loaded, entry)
	}
	return c
}

// SetCapacity replaces what the machine last reported it can take on.
func (w *Worker) SetCapacity(c Capacity) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.capacity = c
}

// Capacity returns what the machine last reported it can take on.
func (w *Worker) Capacity() Capacity {
	if w == nil {
		return Capacity{}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.capacity
}
