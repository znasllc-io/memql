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
	}}, memqlengine.FleetCallRequest{ModelId: "qwen3.8:27b"})
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
	}}, memqlengine.FleetCallRequest{ModelId: "m"})
	if len(plan.Candidates) != 2 || len(plan.Rejected) != 0 {
		t.Fatalf("candidates %v rejected %v, want both full machines kept", plan.Candidates, plan.Rejected)
	}
}

// A machine without room for the model is ruled out with the figures; one
// that already holds the model needs only the reserve free (2 GiB of 32).
func TestAdmissionRulesOutAMachineWithoutMemory(t *testing.T) {
	now := time.Now()
	store := &capacityStore{needs: memqlengine.FleetMemoryNeeds([]memqlengine.CatalogProfile{{ModelId: "qwen3.8:27b", MemoryNeedBytes: int64(20 * gb)}})}
	f := admissionUnderTest(store, now)
	plan := f.admit(context.Background(), RoutePlan{Candidates: []Candidate{
		admissionCandidate("small", 0, 2, reportedCapacity(now, 9*gb)),
		admissionCandidate("holds-it", 0, 2, reportedCapacity(now, 2*gb, memqlengine.FleetLoadedModel{Model: "qwen3.8:27b", SizeBytes: 20 * gb, ActiveCalls: 1})),
		admissionCandidate("silent", 0, 2, memqlengine.FleetCapacity{}),
	}}, memqlengine.FleetCallRequest{ModelId: "qwen3.8:27b"})
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
	plan = f.admit(context.Background(), RoutePlan{Candidates: []Candidate{admissionCandidate("small", 0, 2, reportedCapacity(now, 9*gb))}}, memqlengine.FleetCallRequest{ModelId: "qwen3.8:27b"})
	if len(plan.Candidates) != 0 || plan.Rejected["small"] != want {
		t.Fatalf("candidates %v rejected %v", plan.Candidates, plan.Rejected)
	}
	if store.reads != 1 {
		t.Fatalf("the catalog was read %d times for two plans, want once", store.reads)
	}
}

// Memory is judged at the window the machine would be asked to allocate, which is
// the next power of two above what the call needs. The machine's gate charges
// for that window, so judging the call's own figure would admit a call it then
// declines. 32 GiB keeps a 2 GiB reserve: 20 GiB at 32K wants 22 free, and a
// 64K window another 2.
func TestAdmissionJudgesTheWindowTheMachineWouldBeAskedFor(t *testing.T) {
	const model = "qwen3.8:27b"
	now := time.Now()
	store := &capacityStore{needs: memqlengine.FleetMemoryNeeds([]memqlengine.CatalogProfile{{ModelId: model, MemoryNeedBytes: int64(20 * gb)}})}
	f := admissionUnderTest(store, now)
	for _, tc := range []struct {
		name     string
		free     uint64
		request  memqlengine.FleetCallRequest
		rejected string
	}{
		{name: "a call that fits the baseline window", free: 22 * gb, request: memqlengine.FleetCallRequest{ModelId: model, ContextTokens: 32000}},
		{
			name:     "a call a little over it is asked for the whole next window",
			free:     23 * gb,
			request:  memqlengine.FleetCallRequest{ModelId: model, ContextTokens: 33000},
			rejected: "not enough free memory to load qwen3.8:27b (needs 24 GB with headroom, 23 GB free)",
		},
		{
			name:     "65537 tokens are charged as the 128K window a chat allocates",
			free:     26 * gb,
			request:  memqlengine.FleetCallRequest{ModelId: model, ContextTokens: 65537},
			rejected: "not enough free memory to load qwen3.8:27b (needs 28 GB with headroom, 26 GB free)",
		},
		{name: "an embedding call allocates no window, so it is charged its own figure", free: 26 * gb, request: memqlengine.FleetCallRequest{ModelId: model, Kind: memqlengine.FleetKindEmbedding, ContextTokens: 65537}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			machine := admissionCandidate("machine", 0, 2, reportedCapacity(now, tc.free))
			machine.Labels = map[string]string{workerservice.ModelLabel(model): (ModelAttributes{ContextWindow: 262144}).String()}
			plan := f.admit(context.Background(), RoutePlan{Candidates: []Candidate{machine}}, tc.request)
			if got := plan.Rejected["machine"]; got != tc.rejected {
				t.Fatalf("rejected %q, want %q", got, tc.rejected)
			}
		})
	}
}

// A store with no evidence leaves admission on slots: no machine is ruled out
// for memory it never reported against a need nobody stated.
func TestAdmissionWithoutSignalsAdmitsOnSlots(t *testing.T) {
	now := time.Now()
	f := admissionUnderTest(&plainStore{}, now)
	plan := f.admit(context.Background(), RoutePlan{Candidates: []Candidate{
		admissionCandidate("a", 0, 1, reportedCapacity(now, 1*gb)),
	}}, memqlengine.FleetCallRequest{ModelId: "qwen3.8:27b"})
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
	}}, memqlengine.FleetCallRequest{ModelId: "m"})
	if len(plan.Candidates) != 1 || plan.Candidates[0].RegistrationId != "steady" || plan.Rejected["flaky"] != "degraded: 5 of its last 5 calls failed" {
		t.Fatalf("candidates %v rejected %v", plan.Candidates, plan.Rejected)
	}
	plan = f.admit(context.Background(), RoutePlan{Candidates: []Candidate{admissionCandidate("flaky", 0, 2, memqlengine.FleetCapacity{})}}, memqlengine.FleetCallRequest{ModelId: "m"})
	if len(plan.Candidates) != 1 {
		t.Fatal("a degraded machine that is the only one was dropped; its fleet could never recover")
	}
}
