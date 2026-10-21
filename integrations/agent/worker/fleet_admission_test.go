package worker

import (
	"context"
	"strings"
	"testing"
	"time"

	memqlengine "github.com/znasllc-io/memql/component/memql"
	workerservice "github.com/znasllc-io/memql/component/worker"
)

// capacityStore is a FleetStore that also supplies admission's evidence.
type capacityStore struct {
	needs  map[string]uint64
	health memqlengine.FleetHealthIndex
	reads  int
}

func (s *capacityStore) WorkersForOwner(context.Context, string) ([]Candidate, error) {
	return nil, nil
}
func (s *capacityStore) RoutingPolicyForOwner(context.Context, string) (*Policy, error) {
	return nil, nil
}
func (s *capacityStore) TouchWorkerSelected(context.Context, string, string) error { return nil }
func (s *capacityStore) ModelMemoryNeeds(context.Context) (map[string]uint64, error) {
	s.reads++
	return s.needs, nil
}
func (s *capacityStore) MachineHealth(context.Context, time.Time) (memqlengine.FleetHealthIndex, error) {
	return s.health, nil
}

// plainStore supplies no admission evidence at all.
type plainStore struct{ capacityStore }

func (plainStore) ModelMemoryNeeds() {}

const gb = uint64(1 << 30)

func admissionCandidate(id string, active int, slots uint32, capacity memqlengine.FleetCapacity) Candidate {
	return Candidate{
		RegistrationId: id,
		ActiveCount:    active,
		Concurrency:    map[string]uint32{workerservice.ModelCapability: slots},
		Capacity:       capacity,
	}
}

func reportedCapacity(now time.Time, free uint64, loaded ...memqlengine.FleetLoadedModel) memqlengine.FleetCapacity {
	return memqlengine.FleetCapacity{MemoryTotalBytes: 32 * gb, MemoryFreeBytes: free, MemoryKind: memqlengine.FleetMemoryUnified, Loaded: loaded, ReportedAt: now}
}

func admissionUnderTest(store FleetStore, now time.Time) *FleetInference {
	return &FleetInference{store: store, clock: func() time.Time { return now }}
}

// A full machine goes behind one with a free slot, and the plan says why.
func TestAdmissionPrefersAMachineWithAFreeSlot(t *testing.T) {
	now := time.Now()
	f := admissionUnderTest(&capacityStore{}, now)
	plan := f.admit(context.Background(), RoutePlan{Candidates: []Candidate{
		admissionCandidate("busy", 1, 1, memqlengine.FleetCapacity{}),
		admissionCandidate("free", 0, 1, memqlengine.FleetCapacity{}),
	}}, "qwen3.8:27b")
	if len(plan.Candidates) != 1 || plan.Candidates[0].RegistrationId != "free" {
		t.Fatalf("candidates %v, want only the machine with a free slot", plan.Candidates)
	}
	if got := plan.Rejected["busy"]; got != "every slot is in use (1 of 1 calls running)" {
		t.Fatalf("considered[busy] = %q", got)
	}
}

// When every machine is full the call still goes ahead, queued on a slot, as
// it did before admission existed.
func TestAdmissionKeepsFullMachinesWhenNoneIsFree(t *testing.T) {
	now := time.Now()
	f := admissionUnderTest(&capacityStore{}, now)
	plan := f.admit(context.Background(), RoutePlan{Candidates: []Candidate{
		admissionCandidate("a", 2, 2, memqlengine.FleetCapacity{}),
		admissionCandidate("b", 1, 1, memqlengine.FleetCapacity{}),
	}}, "m")
	if len(plan.Candidates) != 2 || len(plan.Rejected) != 0 {
		t.Fatalf("candidates %v rejected %v, want both full machines kept", plan.Candidates, plan.Rejected)
	}
}

// A machine without room for the model is ruled out with the figures; one
// that already holds the model is admitted whatever it has free.
func TestAdmissionRulesOutAMachineWithoutMemory(t *testing.T) {
	now := time.Now()
	store := &capacityStore{needs: memqlengine.FleetMemoryNeeds([]memqlengine.CatalogProfile{{ModelId: "qwen3.8:27b", MemoryNeedBytes: int64(20 * gb)}})}
	f := admissionUnderTest(store, now)
	plan := f.admit(context.Background(), RoutePlan{Candidates: []Candidate{
		admissionCandidate("small", 0, 2, reportedCapacity(now, 9*gb)),
		admissionCandidate("holds-it", 0, 2, reportedCapacity(now, 1*gb, memqlengine.FleetLoadedModel{Model: "qwen3.8:27b", SizeBytes: 20 * gb, ActiveCalls: 1})),
		admissionCandidate("silent", 0, 2, memqlengine.FleetCapacity{}),
	}}, "qwen3.8:27b")
	var kept []string
	for _, c := range plan.Candidates {
		kept = append(kept, c.RegistrationId)
	}
	if strings.Join(kept, ",") != "holds-it,silent" {
		t.Fatalf("kept %v, want the loaded machine and the one that reports nothing", kept)
	}
	want := "not enough free memory to load qwen3.8:27b (needs 22 GB with headroom, 9 GB free)"
	if plan.Rejected["small"] != want {
		t.Fatalf("considered[small] = %q, want %q", plan.Rejected["small"], want)
	}

	// The only machine has no room: the plan is empty, so the call is refused
	// naming the reason rather than sent to swap.
	plan = f.admit(context.Background(), RoutePlan{Candidates: []Candidate{admissionCandidate("small", 0, 2, reportedCapacity(now, 9*gb))}}, "qwen3.8:27b")
	if len(plan.Candidates) != 0 || plan.Rejected["small"] != want {
		t.Fatalf("candidates %v rejected %v", plan.Candidates, plan.Rejected)
	}
	if store.reads != 1 {
		t.Fatalf("the catalog was read %d times for two plans, want once", store.reads)
	}
}

// A store with no evidence leaves admission on slots: no machine is ruled out
// for memory it never reported against a need nobody stated.
func TestAdmissionWithoutSignalsAdmitsOnSlots(t *testing.T) {
	now := time.Now()
	f := admissionUnderTest(&plainStore{}, now)
	plan := f.admit(context.Background(), RoutePlan{Candidates: []Candidate{
		admissionCandidate("a", 0, 1, reportedCapacity(now, 1*gb)),
	}}, "qwen3.8:27b")
	if len(plan.Candidates) != 1 {
		t.Fatalf("candidates %v rejected %v, want the machine admitted", plan.Candidates, plan.Rejected)
	}
}

// A degraded machine goes behind a healthy one, and stays usable when it is
// all there is.
func TestAdmissionSetsADegradedMachineAside(t *testing.T) {
	now := time.Now()
	var rows []map[string]any
	for i := 0; i < 5; i++ {
		rows = append(rows, map[string]any{"executionSurface": "fleet:flaky", "model": "m", "outcome": "error", "createdAt": now.Add(-time.Minute).UTC().Format(time.RFC3339Nano)})
	}
	store := &capacityStore{health: memqlengine.FoldFleetHealth(rows, now)}
	f := admissionUnderTest(store, now)
	plan := f.admit(context.Background(), RoutePlan{Candidates: []Candidate{
		admissionCandidate("flaky", 0, 2, memqlengine.FleetCapacity{}),
		admissionCandidate("steady", 0, 2, memqlengine.FleetCapacity{}),
	}}, "m")
	if len(plan.Candidates) != 1 || plan.Candidates[0].RegistrationId != "steady" || plan.Rejected["flaky"] != "degraded: 5 of its last 5 calls failed" {
		t.Fatalf("candidates %v rejected %v", plan.Candidates, plan.Rejected)
	}
	plan = f.admit(context.Background(), RoutePlan{Candidates: []Candidate{admissionCandidate("flaky", 0, 2, memqlengine.FleetCapacity{})}}, "m")
	if len(plan.Candidates) != 1 {
		t.Fatal("a degraded machine that is the only one was dropped; its fleet could never recover")
	}
}
