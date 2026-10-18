package worker

// Fleet capacity admission (routes design record, "Fleet capacity: the layer
// under every route"). component/memql's fleet_capacity.go decides one machine;
// this applies the decision to a whole plan, once, after the planner and
// before any candidate is tried. That one place covers every way a fleet call
// is planned -- the person's own machines and the ones lent to them, a call
// pinned to one machine, and the cluster's own work -- so no route reaches a
// machine admission never saw.

import (
	"context"
	"sync"
	"time"

	memqlengine "github.com/znasllc-io/memql/component/memql"
	workerservice "github.com/znasllc-io/memql/component/worker"
)

// CapacitySignalReader is the evidence admission reads beyond the machine
// rows. A store that cannot provide it leaves admission on what the rows say:
// slots, and memory when the catalog is not needed for it.
type CapacitySignalReader interface {
	// ModelMemoryNeeds is the catalog's resident figure per model.
	ModelMemoryNeeds(ctx context.Context) (map[string]uint64, error)
	// MachineHealth folds the recent decision record per machine.
	MachineHealth(ctx context.Context, now time.Time) (memqlengine.FleetHealthIndex, error)
}

// How long each piece of evidence is reused. The catalog changes when an
// operator edits it; the health record moves with every call, and a minute's
// lag on it would keep a recovered machine set aside after it recovered.
const (
	capacityNeedsTTL  = 5 * time.Minute
	capacityHealthTTL = 30 * time.Second
)

// capacityCache holds the evidence for one node. Both halves are read through
// the maintenance principal, so they are the cluster's, not any caller's, and
// one copy serves every call this node plans.
type capacityCache struct {
	mu       sync.Mutex
	needs    map[string]uint64
	needsAt  time.Time
	health   memqlengine.FleetHealthIndex
	healthAt time.Time
}

// ModelMemoryNeeds reads the catalog's resident figure per model.
func (s *EngineStore) ModelMemoryNeeds(ctx context.Context) (map[string]uint64, error) {
	if s == nil || s.Engine == nil {
		return nil, nil
	}
	return memqlengine.ReadFleetMemoryNeeds(ctx, s.Engine)
}

// MachineHealth reads the recent decision record per machine.
func (s *EngineStore) MachineHealth(ctx context.Context, now time.Time) (memqlengine.FleetHealthIndex, error) {
	if s == nil || s.Engine == nil {
		return memqlengine.FleetHealthIndex{}, nil
	}
	return memqlengine.ReadFleetHealth(ctx, s.Engine, now)
}

// capacitySignals returns the evidence, refreshing whichever half is stale. A
// read that fails keeps the last good copy, or none: admission without
// evidence is admission on slots, which is never worse than before it existed.
func (f *FleetInference) capacitySignals(ctx context.Context, now time.Time) (map[string]uint64, memqlengine.FleetHealthIndex) {
	reader, ok := f.store.(CapacitySignalReader)
	if !ok || reader == nil {
		return nil, memqlengine.FleetHealthIndex{}
	}
	c := &f.capacity
	c.mu.Lock()
	needsStale := c.needsAt.IsZero() || now.Sub(c.needsAt) > capacityNeedsTTL
	healthStale := c.healthAt.IsZero() || now.Sub(c.healthAt) > capacityHealthTTL
	c.mu.Unlock()

	if needsStale {
		needs, err := reader.ModelMemoryNeeds(ctx)
		c.mu.Lock()
		if err == nil {
			c.needs = needs
		} else if f.logger != nil {
			f.logger.Warn("fleet admission: could not read the model catalog; memory is judged where it states a need", "error", err)
		}
		c.needsAt = now
		c.mu.Unlock()
	}
	if healthStale {
		health, err := reader.MachineHealth(ctx, now)
		c.mu.Lock()
		if err == nil {
			c.health = health
		} else if f.logger != nil {
			f.logger.Warn("fleet admission: could not read recent decisions; no machine is judged degraded", "error", err)
		}
		c.healthAt = now
		c.mu.Unlock()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.needs, c.health
}

// admit applies fleet capacity to a plan. A machine that cannot load the model
// is ruled out with its reason. A full or degraded machine is set aside while
// another candidate is admitted outright, and otherwise kept, full machines
// before degraded ones: a full machine queues the call on its own slot, as
// every machine did before admission existed.
func (f *FleetInference) admit(ctx context.Context, plan RoutePlan, modelId string) RoutePlan {
	if len(plan.Candidates) == 0 {
		return plan
	}
	now := time.Now()
	if f.clock != nil {
		now = f.clock()
	}
	needs, health := f.capacitySignals(ctx, now)
	need := memqlengine.MemoryNeedFor(needs, modelId)

	var admitted, busy, degraded []Candidate
	setAside := map[string]string{}
	for _, c := range plan.Candidates {
		verdict := memqlengine.AdmitFleetMachine(memqlengine.FleetAdmissionInput{
			ModelId:         modelId,
			ActiveCount:     c.ActiveCount,
			MaxConcurrent:   c.Concurrency[workerservice.ModelCapability],
			Capacity:        c.Capacity,
			MemoryNeedBytes: need,
			Health:          health.For(c.RegistrationId, modelId),
			Now:             now,
		})
		switch {
		case verdict.Refused:
			if plan.Rejected == nil {
				plan.Rejected = map[string]string{}
			}
			plan.Rejected[c.RegistrationId] = verdict.Reason
		case verdict.Kind == memqlengine.FleetAdmitSlot:
			busy = append(busy, c)
			setAside[c.RegistrationId] = verdict.Reason
		case verdict.Kind == memqlengine.FleetAdmitDegraded:
			degraded = append(degraded, c)
			setAside[c.RegistrationId] = verdict.Reason
		default:
			admitted = append(admitted, c)
		}
	}
	if len(admitted) > 0 {
		if plan.Rejected == nil && len(setAside) > 0 {
			plan.Rejected = map[string]string{}
		}
		for id, why := range setAside {
			plan.Rejected[id] = why
		}
		plan.Candidates = admitted
		return plan
	}
	plan.Candidates = append(busy, degraded...)
	return plan
}
