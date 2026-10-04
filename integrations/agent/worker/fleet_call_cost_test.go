//go:build agent

package worker

// WHAT ONE MODEL CALL COSTS THE DATABASE, on the person path (memql#5660).
//
// The machine-sharing epic (memql#5344) made a person's model call read more:
// the catalog, the plan and the replica-hop receiver each read across owners
// to find machines lent to the caller, and each built a fresh Person, so a
// machine lent to a GROUP cost a membership read in all three. This counts
// every read one call makes, end to end and in process: the router's
// availability read (fleetEntry reads FleetInference.Catalog), the call's own
// plan, the forward, and the receiver's re-check on the replica holding the
// lent machine. Both replicas read one counting engine, which stands in for
// the one database they share; the membership reads go through the installed
// membership source, which is what every reader uses in production.
//
// Run with -v to see the figures. The assertions are a ratchet: a change that
// adds a read to the call fails here, and one that removes a read lowers the
// ceiling.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/events"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	workerservice "github.com/znasllc-io/memql/component/worker"
	fleetcatalog "github.com/znasllc-io/memql/component/worker/fleetcatalog"
)

const (
	queryOwnWorkers     = "query myWorkersWithStatus()"
	queryAllWorkers     = "query allWorkersWithStatus()"
	queryRoutingPolicy  = "query routingPolicyForOwner()"
	costModel           = "llama3.1:8b"
	costLender          = "v1:identity:user:olivia"
	costCaller          = "v1:identity:user:ursula"
	costDesignGroup     = "v1:identity:group:design"
	costLentMachineName = "studio"
	costOwnMachineName  = "laptop"
)

// countingFleetEngine is the database both replicas read, as the fleet store
// sees it: every registration row, answered per query and counted.
type countingFleetEngine struct {
	mu     sync.Mutex
	rows   []map[string]any
	counts map[string]int
}

func (e *countingFleetEngine) Execute(ctx context.Context, query string) (*memqlengine.ExecuteResult, error) {
	e.mu.Lock()
	e.counts[query]++
	e.mu.Unlock()
	switch query {
	case queryOwnWorkers:
		// Owner-scoped, as the query's row authz is: the actor the store
		// scoped the read to, matched in either spelling of the id.
		claims, _ := auth.ClaimsFromContext(ctx)
		sub, _ := claims["sub"].(string)
		own := []any{}
		for _, row := range e.rows {
			if owner, _ := row["ownerUserId"].(string); workerservice.SameSubjectId(owner, sub) {
				own = append(own, row)
			}
		}
		return memqlengine.NewResultWithOutput(own), nil
	case queryAllWorkers:
		all := make([]any, 0, len(e.rows))
		for _, row := range e.rows {
			all = append(all, row)
		}
		return memqlengine.NewResultWithOutput(all), nil
	case queryRoutingPolicy:
		return memqlengine.NewResultWithOutput([]any{}), nil
	}
	return nil, fmt.Errorf("countingFleetEngine: unexpected query %q", query)
}

func (e *countingFleetEngine) take() map[string]int {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := e.counts
	e.counts = map[string]int{}
	return out
}

// costStore is EngineStore over the counting engine. EngineStore holds the
// concrete engine, so it cannot be pointed at a fake; but its two fleet reads
// ARE fleetcatalog.EngineStore's (it delegates to it), and its policy read is
// the one query below -- so the reads counted here are the reads it makes.
type costStore struct {
	*fleetcatalog.EngineStore
	engine *countingFleetEngine
}

func (s *costStore) RoutingPolicyForOwner(ctx context.Context, ownerUserId string) (*Policy, error) {
	_, err := s.engine.Execute(auth.ContextWithUserActor(ctx, ownerUserId), queryRoutingPolicy)
	return nil, err
}

func (s *costStore) TouchWorkerSelected(context.Context, string, string) error { return nil }

func newCostStore(engine *countingFleetEngine) *costStore {
	return &costStore{EngineStore: &fleetcatalog.EngineStore{Engine: engine}, engine: engine}
}

// countingMemberships is the installed membership source, counted: the
// caller is in the design group until removeCaller.
type countingMemberships struct {
	mu      sync.Mutex
	reads   int
	removed bool
}

func (m *countingMemberships) ActiveGroupIdsForUser(_ context.Context, userId string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reads++
	if !m.removed && workerservice.SameSubjectId(userId, costCaller) {
		return []string{costDesignGroup}
	}
	return nil
}

// removeCaller takes the caller out of the design group.
func (m *countingMemberships) removeCaller() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removed = true
}

// installMemberships makes a counting source the installed membership source
// for one test.
func installMemberships(t *testing.T) *countingMemberships {
	t.Helper()
	memberships := &countingMemberships{}
	previous := auth.InstalledMembershipSource()
	auth.SetMembershipSource(memberships)
	t.Cleanup(func() { auth.SetMembershipSource(previous) })
	return memberships
}

// subscribeInstalledMembershipCache subscribes this process's membership cache
// to a bus for one test, the way an agent node's wiring does, and returns the
// bus a membership change is published on.
func subscribeInstalledMembershipCache(t *testing.T) *events.Bus {
	t.Helper()
	bus := events.NewBus()
	t.Cleanup(bus.Close)
	sub := workerservice.NewMembershipCacheSubscriber(testLogger(), bus, workerservice.InstalledMembershipCache())
	sub.Start(context.Background())
	t.Cleanup(func() { sub.Stop(context.Background()) })
	return bus
}

func (m *countingMemberships) take() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := m.reads
	m.reads = 0
	return n
}

// callCost is one model call's reads.
type callCost struct {
	ownReads, allReads, policyReads, membershipReads int
}

func (c callCost) String() string {
	return fmt.Sprintf("myWorkersWithStatus=%d allWorkersWithStatus=%d routingPolicyForOwner=%d membership=%d",
		c.ownReads, c.allReads, c.policyReads, c.membershipReads)
}

// costHop is the person path for one model call: the caller's turn on replica
// A, and a machine lent to the caller's GROUP held by replica B.
type costHop struct {
	f           *FleetInference
	engine      *countingFleetEngine
	memberships *countingMemberships
}

// costMachine is one registration row in the counting engine and, held by
// replica B, its stream.
func costMachine(id, owner string, sharing map[string]any) (map[string]any, *workerservice.Worker) {
	row := map[string]any{
		"id":              id,
		"name":            id,
		"ownerUserId":     owner,
		"capabilities":    []any{workerservice.CapabilityHeadless, workerservice.ModelCapability},
		"labels":          map[string]any{workerservice.ModelLabel(costModel): "ctx=8192"},
		"connectedNodeId": nodeB,
		"lastSeenAt":      fleetNow().Format(time.RFC3339Nano),
		"sharing":         sharing,
		"capabilityDescriptor": map[string]any{
			"inferenceServe": workerservice.InferenceServeCluster,
		},
	}
	w := &workerservice.Worker{
		RegistrationId: id, OwnerUserId: owner, Name: id,
		Capabilities: []string{workerservice.CapabilityHeadless, workerservice.ModelCapability},
		Labels:       map[string]string{workerservice.ModelLabel(costModel): "ctx=8192"},
		Concurrency:  map[string]uint32{workerservice.ModelCapability: 4},
	}
	w.SetModelCallFunc(func(_ context.Context, req workerservice.ModelCallRequest) (*workerservice.ModelCallHandle, error) {
		h, _, finish := workerservice.NewModelCallLoopback(req, func(string) {})
		go finish(workerservice.ModelCallOutcome{FinishReason: workerservice.ModelFinishStop, Content: "served"})
		return h, nil
	})
	return row, w
}

// newCostHop is the cluster the measurements run in: a machine lent to the
// caller's group, plus -- with ownMachine -- one of the caller's own, both
// held by replica B.
func newCostHop(t *testing.T, ownMachine bool) *costHop {
	t.Helper()
	memberships := installMemberships(t)
	regB := workerservice.NewRegistry(testLogger(), fleetNow)

	studio, studioWorker := costMachine(costLentMachineName, costLender,
		map[string]any{"mode": workerservice.SharingModePeople, "groupIds": []any{"design"}})
	rows := []map[string]any{studio}
	regB.Add(studioWorker)
	if ownMachine {
		laptop, laptopWorker := costMachine(costOwnMachineName, costCaller, map[string]any{"mode": workerservice.SharingModeOwner})
		rows = append(rows, laptop)
		regB.Add(laptopWorker)
	}
	engine := &countingFleetEngine{rows: rows, counts: map[string]int{}}

	link := &meshLink{t: t, reachable: true}
	link.handler = NewForwardHandler(regB, newCostStore(engine), testLogger())
	link.router = newForwardRouter(link, func() (string, string) { return nodeA, "agent" }, testLogger())

	f := newFleetInference(t, newCostStore(engine))
	f.selfNodeId, f.forward = nodeA, link.router
	return &costHop{f: f, engine: engine, memberships: memberships}
}

// call is one model call on the person path: the availability read the
// router makes before choosing the provider, then the call itself, pinned to
// one machine when pin names one. It reports what the call read and which
// machine served it.
func (h *costHop) call(t *testing.T, n int, pin string) (callCost, string) {
	t.Helper()
	ctx := authorityCtx(t, costCaller)
	models, err := h.f.Catalog(ctx, costCaller)
	if err != nil || len(models) != 1 || !models[0].Online() {
		t.Fatalf("call %d: the router's availability read must offer the model: %+v, %v", n, models, err)
	}
	res, err := h.f.Call(ctx, memqlengine.FleetCallRequest{
		ActingUserId: costCaller, RegistrationId: pin, ModelId: costModel, Kind: memqlengine.FleetKindChat,
	})
	if err != nil || res.Content != "served" {
		t.Fatalf("call %d: the call must be served: %+v, %v", n, res, err)
	}
	counts := h.engine.take()
	for q := range counts {
		if q != queryOwnWorkers && q != queryAllWorkers && q != queryRoutingPolicy {
			t.Fatalf("call %d: an unexpected read %q", n, q)
		}
	}
	return callCost{
		ownReads:        counts[queryOwnWorkers],
		allReads:        counts[queryAllWorkers],
		policyReads:     counts[queryRoutingPolicy],
		membershipReads: h.memberships.take(),
	}, res.ExecutionSurface
}

// Measured on this harness for memql#5660, before the receiver reused
// anything: every call read myWorkersWithStatus 3 times, allWorkersWithStatus
// 4 times, routingPolicyForOwner once and the membership source 3 times --
// once each for the catalog, the plan and the receiver -- and a membership
// read is two full concept reads in production. The receiver now reads a
// person's groups once and reuses them until a membership or a group changes,
// so the second call and every one after it costs one membership read fewer.
func TestWhatOnePersonModelCallReads(t *testing.T) {
	h := newCostHop(t, false)
	subscribeInstalledMembershipCache(t)
	ceiling := callCost{ownReads: 3, allReads: 4, policyReads: 1}
	var costs []string
	for n := 1; n <= 3; n++ {
		c, _ := h.call(t, n, "")
		costs = append(costs, fmt.Sprintf("call %d: %s", n, c))
		wantMemberships := 2 // the catalog's and the plan's: the sender reads fresh
		if n == 1 {
			wantMemberships = 3 // and the receiver's first, which it keeps
		}
		if c.membershipReads != wantMemberships {
			t.Errorf("call %d: membership reads = %d, want %d", n, c.membershipReads, wantMemberships)
		}
		if c.ownReads > ceiling.ownReads || c.allReads > ceiling.allReads || c.policyReads > ceiling.policyReads {
			t.Errorf("call %d: %s exceeds the measured ceiling %s", n, c, ceiling)
		}
	}
	t.Log("person-path model call to a machine lent to the caller's group, across the hop:\n  " +
		strings.Join(costs, "\n  "))
}

// A PIN TO YOUR OWN MACHINE pays for your own machines only (memql#5660,
// review of memql#5662). The pin plans over everything the caller may use,
// own and lent, but a pin that names one of their own machines is decided by
// the owner-scoped plan alone; reading every registration in the cluster and
// the caller's groups to rule on a machine already in hand was the per-call
// cost this issue set out to cut. The availability read before the call is
// the router's and reads what it always did.
//
// Measured before the fix: own=3 all=2 policy=1 membership=2 -- the plan's
// cross-owner read, and its membership read for the machine lent to a group.
func TestWhatAPinToYourOwnMachineReads(t *testing.T) {
	h := newCostHop(t, true)
	subscribeInstalledMembershipCache(t)
	want := callCost{
		ownReads:        3, // the catalog, the plan, the receiver's ownership re-check
		allReads:        1, // the catalog's, for what is lent to the caller
		policyReads:     1, // the plan's
		membershipReads: 1, // the catalog's, for the machine lent to a group
	}
	for n := 1; n <= 2; n++ {
		c, surface := h.call(t, n, "v1:worker:registration:"+costOwnMachineName)
		t.Logf("call %d, pinned to the caller's own machine: %s", n, c)
		if surface != FleetSurfacePrefix+costOwnMachineName {
			t.Fatalf("call %d: the pin was served by %q", n, surface)
		}
		if c != want {
			t.Errorf("call %d: %s, want %s", n, c, want)
		}
	}
}
