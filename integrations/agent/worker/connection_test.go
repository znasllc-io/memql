//go:build agent

package worker

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	workerservice "github.com/znasllc-io/memql/component/worker"
)

// Network-aware fleet selection (docs/superpowers/specs/
// 2026-11-24-network-aware-fleet-selection-design.md): a pipeline step tries
// machines whose connection has not dropped recently first, and a fleet step
// waits for a steady machine that is busy with work rather than start on an
// unsteady one (D1, D6). These are the single-replica cases;
// pipeline_connection_hop_test.go has the ones across the mesh.

const window = 2 * time.Hour

func dropAt(id string, at time.Time) workerservice.ConnectionDrop {
	return workerservice.ConnectionDrop{RegistrationId: id, DroppedAt: at, Cause: workerservice.DropCauseReplaced}
}

func TestConnectionHealthCountsDropsInTheWindow(t *testing.T) {
	now := fleetNow()
	h := healthFrom([]workerservice.ConnectionDrop{dropAt("m", now.Add(-10*time.Minute)), dropAt("m", now.Add(-90*time.Minute)), dropAt("m", now.Add(-3*time.Hour))}, now, window, false)
	if h.Drops != 2 || !h.LastDropAt.Equal(now.Add(-10*time.Minute)) || h.Steady() || h.Window != window {
		t.Fatalf("health = %+v, want two drops in the window, the last ten minutes ago", h)
	}
	if got := healthFrom(nil, now, window, false); !got.Steady() || got.RecordValue() != "steady" {
		t.Fatalf("a machine with no drops reads %+v (%q), want steady", got, got.RecordValue())
	}
	if got := h.RecordValue(); got != "2 drops, last "+now.Add(-10*time.Minute).Format(time.RFC3339) {
		t.Fatalf("RecordValue() = %q", got)
	}
	if got := healthFrom([]workerservice.ConnectionDrop{dropAt("m", now.Add(-time.Minute))}, now, window, false).RecordValue(); !strings.HasPrefix(got, "1 drop, last ") {
		t.Fatalf("one drop reads %q", got)
	}
}

func TestConnectionHealthWindowStartIsInclusive(t *testing.T) {
	now := fleetNow()
	if h := healthFrom([]workerservice.ConnectionDrop{dropAt("m", now.Add(-window))}, now, window, false); h.Drops != 1 {
		t.Fatalf("a drop exactly at the window's start was not counted: %+v", h)
	}
	if h := healthFrom([]workerservice.ConnectionDrop{dropAt("m", now.Add(-window-time.Nanosecond))}, now, window, false); h.Drops != 0 {
		t.Fatalf("a drop just before the window counted: %+v", h)
	}
}

// The drop is stamped on the clock of the replica that held the stream, and
// another replica's clock may be a few seconds behind it.
func TestConnectionHealthCountsADropStampedAhead(t *testing.T) {
	now := fleetNow()
	h := healthFrom([]workerservice.ConnectionDrop{dropAt("m", now.Add(5*time.Second))}, now, window, false)
	if h.Drops != 1 || !h.LastDropAt.Equal(now.Add(5*time.Second)) {
		t.Fatalf("a drop stamped five seconds ahead = %+v, want it counted", h)
	}
}

func TestConnectionHealthAtTheCapIsUnsteadyAndSaysSo(t *testing.T) {
	now := fleetNow()
	drops := make([]workerservice.ConnectionDrop, connectionDropReadCap)
	for i := range drops {
		drops[i] = dropAt("m", now.Add(-time.Duration(i)*time.Second))
	}
	h := healthFrom(drops, now, window, true)
	if h.Steady() || h.Drops != connectionDropReadCap || !h.Capped {
		t.Fatalf("health at the cap = %+v", h)
	}
	if got := h.RecordValue(); !strings.HasPrefix(got, "100 or more drops, last ") {
		t.Fatalf("RecordValue() at the cap = %q", got)
	}
	if got := (ConnectionHealth{Unavailable: true}); !got.Steady() || got.RecordValue() != "unavailable" {
		t.Fatalf("an unreadable machine reads %+v (%q), want steady and unavailable", got, got.RecordValue())
	}
}

func plannedOrder(p RoutePlan) []string {
	ids := make([]string, 0, len(p.Candidates))
	for _, c := range p.Candidates {
		ids = append(ids, c.RegistrationId)
	}
	return ids
}

func TestConnectionHealthMatchesBothSpellingsOfTheId(t *testing.T) {
	p := RoutePlan{Candidates: []Candidate{machine("laptop"), machine("desktop")}}
	p.ApplyConnection(map[string]ConnectionHealth{
		"v1:worker:registration:laptop": {Drops: 1, LastDropAt: fleetNow().Add(-time.Minute), Window: window},
		"desktop":                       {Window: window},
	})
	if got := plannedOrder(p); !reflect.DeepEqual(got, []string{"desktop", "laptop"}) {
		t.Fatalf("order = %v, want the steady desktop first", got)
	}
	if h, ok := p.healthOf(machine("v1:worker:registration:laptop")); !ok || h.Steady() {
		t.Fatalf("health of the laptop under its canonical id = %+v, %v", h, ok)
	}
}

func TestApplyConnectionPutsSteadyMachinesFirstInStrategyOrder(t *testing.T) {
	p := RoutePlan{Candidates: []Candidate{machine("a"), machine("b"), machine("c")}}
	p.ApplyConnection(map[string]ConnectionHealth{"a": {Drops: 1, LastDropAt: fleetNow()}, "b": {}, "c": {}})
	if got := plannedOrder(p); !reflect.DeepEqual(got, []string{"b", "c", "a"}) {
		t.Fatalf("order = %v, want b, c (strategy order kept) then the unsteady a", got)
	}
}

func TestApplyConnectionOrdersUnsteadyByFewestThenOldest(t *testing.T) {
	now := fleetNow()
	p := RoutePlan{Candidates: []Candidate{machine("a"), machine("b"), machine("c"), machine("d")}}
	p.ApplyConnection(map[string]ConnectionHealth{
		"a": {Drops: 2, LastDropAt: now.Add(-10 * time.Minute)},
		"b": {Drops: 1, LastDropAt: now.Add(-5 * time.Minute)},
		"c": {Drops: 1, LastDropAt: now.Add(-50 * time.Minute)},
		"d": {},
	})
	if got := plannedOrder(p); !reflect.DeepEqual(got, []string{"d", "c", "b", "a"}) {
		t.Fatalf("order = %v, want the steady d, then fewest drops with the oldest last drop first", got)
	}
}

func TestApplyConnectionLeavesAPlanWithNoDropsUnchanged(t *testing.T) {
	p := RoutePlan{Candidates: []Candidate{machine("b"), machine("a"), machine("c")}}
	p.ApplyConnection(map[string]ConnectionHealth{"a": {}, "b": {}, "c": {}})
	if got := plannedOrder(p); !reflect.DeepEqual(got, []string{"b", "a", "c"}) {
		t.Fatalf("order = %v, want the strategy's order untouched", got)
	}
	if rec := p.Record(); rec.Connection["a"] != "steady" || len(rec.Connection) != 3 {
		t.Fatalf("record connection = %v, want every machine named steady", rec.Connection)
	}
}

// --- dispatch, one replica -----------------------------------------------------

func connectionRequest(waitForSteady bool) Request {
	req := pipelineRequest()
	req.Connection = ConnectionPreference{Window: window, WaitForSteady: waitForSteady}
	return req
}

// connectionFleet is the owner's machines, held here, in the given registration
// order, each named "Box <id>" by its owner; the laptop's connection dropped ten
// minutes ago and every other machine's is steady.
func connectionFleet(t *testing.T, order ...string) *pipelineFleet {
	t.Helper()
	cands := make([]Candidate, 0, len(order))
	for _, id := range order {
		c := machine(id, withLabels(pipelineLabels()))
		c.DisplayName = "Box " + id
		cands = append(cands, c)
	}
	f := newPipelineFleet(t, cands...)
	f.store.fakeFleet.drops = map[string][]workerservice.ConnectionDrop{"laptop": {dropAt("laptop", fleetNow().Add(-10*time.Minute))}}
	return f
}

func TestPipelineTriesASteadyMachineBeforeAnUnsteadyOne(t *testing.T) {
	f := connectionFleet(t, "laptop", "desktop") // the unsteady laptop is first in registration order
	res, err := f.d.Dispatch(asPipelineExecutor(), connectionRequest(true))
	if err != nil || !res.OK || res.WorkerId != "desktop" || len(f.dispatched["laptop"]) != 0 {
		t.Fatalf("result = %+v err = %v laptop dispatches = %d, want the step on the steady desktop", res, err, len(f.dispatched["laptop"]))
	}
	if res.Connection == nil || !res.Connection.Steady() {
		t.Fatalf("result connection = %+v, want the desktop's steady health", res.Connection)
	}
	routing := f.store.lastInvocation(t).Routing
	conn, _ := routing["connection"].(map[string]string)
	if conn["desktop"] != "steady" || !strings.HasPrefix(conn["laptop"], "1 drop, last ") {
		t.Fatalf("routing connection = %v, want both machines named", routing["connection"])
	}
}

func TestPipelineDoesNotWaitForASteadyMachineThatCannotFreeUp(t *testing.T) {
	for _, tc := range []struct {
		name   string
		code   string
		active int
	}{
		{"busy with nothing of ours running (disk or memory)", "pipeline_capacity_busy", 0},
		{"held by an operator's fence", "pipeline_capacity_fenced", 1},
		{"runtime missing", "pipeline_runtime_unavailable", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := connectionFleet(t, "desktop", "laptop")
			f.store.fakeFleet.machines[0].ActiveCount = tc.active
			f.registry.WorkerById("desktop").SetDispatchFunc(pipelineCapacityReply(tc.code, nil), func() {})
			res, err := f.d.Dispatch(asPipelineExecutor(), connectionRequest(true))
			if err != nil || !res.OK || res.WorkerId != "laptop" || len(f.dispatched["laptop"]) != 1 {
				t.Fatalf("result = %+v err = %v, want the step on the laptop: the steady desktop cannot free up by waiting", res, err)
			}
			if res.Connection == nil || res.Connection.Steady() || res.Connection.Drops != 1 {
				t.Fatalf("result connection = %+v, want the laptop's one drop", res.Connection)
			}
		})
	}
}

func TestAPoolStepDoesNotWaitForASteadyMachine(t *testing.T) {
	f := connectionFleet(t, "desktop", "laptop")
	f.store.fakeFleet.machines[0].ActiveCount = 1
	f.registry.WorkerById("desktop").SetDispatchFunc(pipelineCapacityReply("pipeline_capacity_busy", nil), func() {})
	res, err := f.d.Dispatch(asPipelineExecutor(), connectionRequest(false))
	if err != nil || !res.OK || res.WorkerId != "laptop" || len(res.PassedOver) != 0 {
		t.Fatalf("result = %+v err = %v, want the pool step on the free laptop at once (D10)", res, err)
	}
}

func TestAllUnsteadyMachinesRunFewestDropsFirst(t *testing.T) {
	f := connectionFleet(t, "a", "b", "c")
	now := fleetNow()
	f.store.fakeFleet.drops = map[string][]workerservice.ConnectionDrop{
		"a": {dropAt("a", now.Add(-5*time.Minute)), dropAt("a", now.Add(-20*time.Minute))},
		"b": {dropAt("b", now.Add(-5*time.Minute))},
		"c": {dropAt("c", now.Add(-50*time.Minute))},
	}
	res, err := f.d.Dispatch(asPipelineExecutor(), connectionRequest(true))
	if err != nil || !res.OK || res.WorkerId != "c" {
		t.Fatalf("result = %+v err = %v, want c: one drop, the oldest", res, err)
	}
}

func TestAPinnedRetryIsNotReorderedButCarriesItsHealth(t *testing.T) {
	f := connectionFleet(t, "desktop", "laptop")
	req := connectionRequest(true)
	req.PinRegistration = "laptop"
	res, err := f.d.Dispatch(asPipelineExecutor(), req)
	if err != nil || !res.OK || res.WorkerId != "laptop" {
		t.Fatalf("result = %+v err = %v, want the pinned laptop", res, err)
	}
	if res.Connection == nil || res.Connection.Drops != 1 {
		t.Fatalf("result connection = %+v, want the pinned machine's drop recorded", res.Connection)
	}
}

func TestAFailedDropReadLeavesTheOwnersOrder(t *testing.T) {
	f := connectionFleet(t, "laptop", "desktop")
	f.store.fakeFleet.dropsErr = errors.New("database unreachable")
	res, err := f.d.Dispatch(asPipelineExecutor(), connectionRequest(true))
	if err != nil || !res.OK || res.WorkerId != "laptop" {
		t.Fatalf("result = %+v err = %v, want the first machine in the owner's order", res, err)
	}
	if res.Connection == nil || !res.Connection.Unavailable {
		t.Fatalf("result connection = %+v, want unavailable", res.Connection)
	}
	if conn, _ := f.store.lastInvocation(t).Routing["connection"].(map[string]string); conn["laptop"] != "unavailable" {
		t.Fatalf("routing connection = %v", conn)
	}
}

func TestNoDropsAndNoWindowRouteExactlyAsBefore(t *testing.T) {
	plain := connectionFleet(t, "laptop", "desktop")
	plain.store.fakeFleet.drops = nil
	res, err := plain.d.Dispatch(asPipelineExecutor(), pipelineRequest())
	if err != nil || !res.OK || res.WorkerId != "laptop" || res.Connection != nil {
		t.Fatalf("a request with no window = %+v, %v; want today's first machine and no connection read", res, err)
	}
	routing := plain.store.lastInvocation(t).Routing
	if _, present := routing["connection"]; present || len(plain.store.fakeFleet.dropReads) != 0 {
		t.Fatalf("a request with no window read drops %v or recorded %v", plain.store.fakeFleet.dropReads, routing["connection"])
	}

	steady := connectionFleet(t, "laptop", "desktop")
	steady.store.fakeFleet.drops = nil
	res2, err := steady.d.Dispatch(asPipelineExecutor(), connectionRequest(true))
	if err != nil || !res2.OK || res2.WorkerId != res.WorkerId {
		t.Fatalf("with a window and no drops = %+v, %v; want the same machine as without", res2, err)
	}
	if got, want := steady.store.lastInvocation(t).Routing["candidatesConsidered"], routing["candidatesConsidered"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("candidates considered = %v, want %v", got, want)
	}
}

func TestConnectionPreferenceIsRefusedOutsideThePipelinePurpose(t *testing.T) {
	f := newPipelineFleet(t)
	f.store.authorization = &Authorization{ComputerUseScope: "full"}
	req := approvedRequest()
	req.OwnerUserId = pipelineOwner
	req.Connection = ConnectionPreference{Window: window}
	res, err := f.d.Dispatch(asPerson(pipelineOwner), req)
	mustRefuse(t, f, res, err, "denied_pipeline_purpose")
	if !res.RefusedBeforeStart || !res.RefusedByGate {
		t.Fatalf("result = %+v, want a refusal before start by the gate", res)
	}
}
