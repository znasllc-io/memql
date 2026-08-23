//go:build agent

package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	workerservice "github.com/znasllc-io/memql/component/worker"
)

// fakeStore is the whole Store: the four gate reads plus the fleet reads. The
// gates are set to PERMIT so these tests are about the routing loop and
// nothing else -- the gate paths have their own coverage, and mixing the two
// is how a routing regression hides behind a denial.
type fakeStore struct {
	*fakeFleet
	invocations []workerservice.InvocationRow
	writeErr    error
}

func (s *fakeStore) UserPreferences(context.Context, string) (Preferences, error) {
	return Preferences{ComputerUseEnabled: true}, nil
}

func (s *fakeStore) AgentAuthorization(context.Context, string, string) (*Authorization, error) {
	return &Authorization{ComputerUseScope: "full"}, nil
}

func (s *fakeStore) PlanScope(context.Context, string) (string, error) { return "", nil }

func (s *fakeStore) WriteInvocation(_ context.Context, row workerservice.InvocationRow) error {
	s.invocations = append(s.invocations, row)
	return s.writeErr
}

func (s *fakeStore) lastInvocation(t *testing.T) workerservice.InvocationRow {
	t.Helper()
	if len(s.invocations) == 0 {
		t.Fatal("no invocation was recorded -- every terminal path must write one")
	}
	return s.invocations[len(s.invocations)-1]
}

// liveMachine registers a connected machine in a real Registry with a
// scripted dispatch function, so Acquire / Release and the concurrency valve
// are the production ones rather than a stand-in.
func liveMachine(t *testing.T, reg *workerservice.Registry, id string, concurrency uint32, fn workerservice.DispatchFunc) *workerservice.Worker {
	t.Helper()
	w := &workerservice.Worker{
		RegistrationId: id,
		OwnerUserId:    "user-1",
		Name:           id,
		Capabilities:   []string{workerservice.CapabilityHeadless},
		Concurrency:    map[string]uint32{workerservice.CapabilityHeadless: concurrency},
	}
	w.SetDispatchFunc(fn, func() {})
	reg.Add(w)
	return w
}

func okResult(callId string) *memqlv1.ToolResult {
	return &memqlv1.ToolResult{
		CallId: callId,
		Payload: &memqlv1.ToolResult_Success{
			Success: &memqlv1.Success{ResultJson: []byte(`{"ok":true}`)},
		},
	}
}

func approvedRequest() Request {
	return Request{
		Tool:        "workerHost",
		Action:      "fs_read",
		Args:        map[string]any{"path": "/tmp/x"},
		AgentId:     "agent-1",
		OwnerUserId: "user-1",
		// The per-task approval gate requires a PlanId; without one every
		// dispatch is denied before the router runs.
		PlanId:  "plan-1",
		Timeout: 2 * time.Second,
	}
}

func newTestDispatcher(t *testing.T, store *fakeStore, reg *workerservice.Registry, selfNodeId string, remote RemoteDispatcher) *Dispatcher {
	t.Helper()
	d, err := NewDispatcher(Options{
		Logger:     testLogger(),
		Registry:   reg,
		Store:      store,
		Clock:      fleetNow,
		SelfNodeId: selfNodeId,
		Remote:     remote,
	})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	return d
}

// --- re-pick before side effects -------------------------------------------

func TestRePicksWhenTheFirstMachineIsBusy(t *testing.T) {
	reg := workerservice.NewRegistry(testLogger(), fleetNow)
	// "busy" has a cap of 1 and that slot is already taken, so Acquire blocks
	// and the short per-call timeout turns it into a refusal. Nothing runs on
	// it, which is the whole precondition for trying the next machine.
	busy := liveMachine(t, reg, "busy", 1, func(context.Context, *memqlv1.ToolDispatch, func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error) {
		t.Fatal("the busy machine must never be dispatched to")
		return nil, nil
	})
	if err := busy.Acquire(context.Background(), workerservice.CapabilityHeadless); err != nil {
		t.Fatalf("pre-acquiring the only slot: %v", err)
	}

	dispatched := false
	liveMachine(t, reg, "free", 4, func(_ context.Context, d *memqlv1.ToolDispatch, _ func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error) {
		dispatched = true
		return okResult(d.GetCallId()), nil
	})

	store := &fakeStore{fakeFleet: &fakeFleet{
		machines: []Candidate{machine("busy"), machine("free")},
		policy:   &Policy{Id: "p", Strategy: StrategyFirstFit, Fallback: FallbackNextMatching},
	}}
	d := newTestDispatcher(t, store, reg, "", nil)

	req := approvedRequest()
	req.Timeout = 150 * time.Millisecond
	res, err := d.Dispatch(context.Background(), req)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !res.OK {
		t.Fatalf("result = %+v, want the call to land on the second machine", res)
	}
	if !dispatched {
		t.Fatal("the free machine was never dispatched to")
	}

	row := store.lastInvocation(t)
	if row.WorkerId != "free" {
		t.Fatalf("invocation.workerId = %q, want the machine that ran it", row.WorkerId)
	}
	if got := row.Routing["attempts"]; got != 2 {
		t.Fatalf("routing.attempts = %v, want 2", got)
	}
	if got := row.Routing["reroutedFrom"]; got != "worker:busy" {
		t.Fatalf("routing.reroutedFrom = %v, want worker:busy -- the record is what makes "+
			"\"why did this run there\" answerable after the fact", got)
	}
	if got := row.Routing["selectedBy"]; got != SelectedByReroute {
		t.Fatalf("routing.selectedBy = %v, want %q", got, SelectedByReroute)
	}
}

func TestFallbackNoneDoesNotRePick(t *testing.T) {
	reg := workerservice.NewRegistry(testLogger(), fleetNow)
	busy := liveMachine(t, reg, "busy", 1, func(context.Context, *memqlv1.ToolDispatch, func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error) {
		return nil, errors.New("unreachable")
	})
	_ = busy.Acquire(context.Background(), workerservice.CapabilityHeadless)
	liveMachine(t, reg, "free", 4, func(context.Context, *memqlv1.ToolDispatch, func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error) {
		t.Fatal("fallback=none must not try a second machine")
		return nil, nil
	})

	store := &fakeStore{fakeFleet: &fakeFleet{
		machines: []Candidate{machine("busy"), machine("free")},
		policy:   &Policy{Strategy: StrategyFirstFit, Fallback: FallbackNone},
	}}
	d := newTestDispatcher(t, store, reg, "", nil)
	req := approvedRequest()
	req.Timeout = 150 * time.Millisecond
	res, _ := d.Dispatch(context.Background(), req)
	if res.ErrorCode != "worker_busy" {
		t.Fatalf("errorCode = %q, want worker_busy reported rather than routed around", res.ErrorCode)
	}
}

func TestNeverRePicksAfterTheCallStarted(t *testing.T) {
	// THE D5 INVARIANT. The machine took the dispatch and then its stream
	// died. The command may have run. Trying the next machine here would be a
	// second side effect wearing the costume of a retry.
	reg := workerservice.NewRegistry(testLogger(), fleetNow)
	liveMachine(t, reg, "first", 4, func(context.Context, *memqlv1.ToolDispatch, func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error) {
		return nil, workerservice.ErrWorkerDisconnected
	})
	liveMachine(t, reg, "second", 4, func(context.Context, *memqlv1.ToolDispatch, func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error) {
		t.Fatal("a machine that lost its stream MID-CALL must not be routed around: the call may have run")
		return nil, nil
	})

	store := &fakeStore{fakeFleet: &fakeFleet{
		machines: []Candidate{machine("first"), machine("second")},
		policy:   &Policy{Strategy: StrategyFirstFit, Fallback: FallbackNextMatching},
	}}
	d := newTestDispatcher(t, store, reg, "", nil)
	res, _ := d.Dispatch(context.Background(), approvedRequest())
	if res.OK {
		t.Fatal("want the disconnect reported as a failure")
	}
	if got := store.lastInvocation(t).Routing["attempts"]; got != 1 {
		t.Fatalf("routing.attempts = %v, want 1 -- exactly one machine was tried", got)
	}
}

func TestLastSelectedAtIsStampedOnlyOnAMachineThatRan(t *testing.T) {
	reg := workerservice.NewRegistry(testLogger(), fleetNow)
	busy := liveMachine(t, reg, "busy", 1, func(context.Context, *memqlv1.ToolDispatch, func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error) {
		return nil, nil
	})
	_ = busy.Acquire(context.Background(), workerservice.CapabilityHeadless)
	liveMachine(t, reg, "free", 4, func(_ context.Context, d *memqlv1.ToolDispatch, _ func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error) {
		return okResult(d.GetCallId()), nil
	})

	f := &fakeFleet{
		machines: []Candidate{machine("busy"), machine("free")},
		policy:   &Policy{Strategy: StrategyFirstFit, Fallback: FallbackNextMatching},
	}
	store := &fakeStore{fakeFleet: f}
	d := newTestDispatcher(t, store, reg, "", nil)
	req := approvedRequest()
	req.Timeout = 150 * time.Millisecond
	if _, err := d.Dispatch(context.Background(), req); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(f.touched) != 1 || f.touched[0] != "free" {
		t.Fatalf("touched = %v, want only the machine that ran -- stamping a machine that "+
			"refused would rotate roundRobin on a pick that never happened", f.touched)
	}
}

// --- no candidates ----------------------------------------------------------

func TestNoCandidateMessageNamesTheMachinesAndTheReasons(t *testing.T) {
	stale := machine("laptop")
	stale.LastSeenAt = fleetNow().Add(-time.Hour)
	store := &fakeStore{fakeFleet: &fakeFleet{machines: []Candidate{stale}}}
	d := newTestDispatcher(t, store, workerservice.NewRegistry(testLogger(), fleetNow), "", nil)

	res, _ := d.Dispatch(context.Background(), approvedRequest())
	if res.ErrorCode != "no_worker_available" {
		t.Fatalf("errorCode = %q", res.ErrorCode)
	}
	for _, want := range []string{"1 paired machine", "laptop", "offline"} {
		if !contains(res.ErrorMessage, want) {
			t.Errorf("message %q does not mention %q -- \"no worker available\" on its own is the "+
				"least useful true sentence available to someone looking at a machine they can see is on",
				res.ErrorMessage, want)
		}
	}
	if got := store.lastInvocation(t).Outcome; got != "no_worker_available" {
		t.Fatalf("outcome = %q", got)
	}
}

func TestNoMachinesAtAllSaysSo(t *testing.T) {
	store := &fakeStore{fakeFleet: &fakeFleet{}}
	d := newTestDispatcher(t, store, workerservice.NewRegistry(testLogger(), fleetNow), "", nil)
	res, _ := d.Dispatch(context.Background(), approvedRequest())
	if !contains(res.ErrorMessage, "no machines are paired") {
		t.Fatalf("message = %q, want it to distinguish an empty fleet from a filtered one", res.ErrorMessage)
	}
}

// --- local versus remote ----------------------------------------------------

type fakeRemote struct {
	calls   []string
	result  Result
	outcome ForwardOutcome
	err     error
}

func (r *fakeRemote) ForwardDispatch(_ context.Context, nodeId string, _ Request, registrationId, _ string, _ time.Duration) (Result, ForwardOutcome, error) {
	r.calls = append(r.calls, nodeId+"/"+registrationId)
	return r.result, r.outcome, r.err
}

func TestAMachineHeldByAnotherReplicaIsForwarded(t *testing.T) {
	remote := &fakeRemote{result: Result{OK: true, OutputJSON: `{"ok":true}`}, outcome: ForwardCompleted}
	elsewhere := machine("laptop")
	elsewhere.ConnectedNodeId = "agent-2"

	store := &fakeStore{fakeFleet: &fakeFleet{machines: []Candidate{elsewhere}}}
	d := newTestDispatcher(t, store, workerservice.NewRegistry(testLogger(), fleetNow), "agent-1", remote)

	res, _ := d.Dispatch(context.Background(), approvedRequest())
	if !res.OK {
		t.Fatalf("result = %+v, want the forwarded call to succeed", res)
	}
	if len(remote.calls) != 1 || remote.calls[0] != "agent-2/laptop" {
		t.Fatalf("forwards = %v, want one to the replica named by connectedNodeId", remote.calls)
	}
}

func TestAMachineHeldElsewhereIsSkippedRatherThanRunLocallyWhenThereIsNoForward(t *testing.T) {
	// Without a forward this node holds no stream for the machine, so a local
	// dispatch cannot work -- and would fail in a way that blames the machine.
	elsewhere := machine("laptop")
	elsewhere.ConnectedNodeId = "agent-2"
	here := machine("desktop")
	here.ConnectedNodeId = "agent-1"

	reg := workerservice.NewRegistry(testLogger(), fleetNow)
	ran := false
	liveMachine(t, reg, "desktop", 4, func(_ context.Context, d *memqlv1.ToolDispatch, _ func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error) {
		ran = true
		return okResult(d.GetCallId()), nil
	})

	store := &fakeStore{fakeFleet: &fakeFleet{
		machines: []Candidate{elsewhere, here},
		policy:   &Policy{Strategy: StrategyFirstFit, Fallback: FallbackNextMatching},
	}}
	d := newTestDispatcher(t, store, reg, "agent-1", nil)

	res, _ := d.Dispatch(context.Background(), approvedRequest())
	if !res.OK || !ran {
		t.Fatalf("result = %+v ran=%v, want the call to fall through to the machine this replica holds", res, ran)
	}
	if got := store.lastInvocation(t).WorkerId; got != "desktop" {
		t.Fatalf("workerId = %q, want the locally-held machine", got)
	}
}

func TestSingleNodeTreatsEveryMachineAsLocal(t *testing.T) {
	// SelfNodeId empty means one replica. connectedNodeId may be anything --
	// including a stale value from a previous deployment -- and treating it as
	// remote would make a working single-process install stop dispatching.
	reg := workerservice.NewRegistry(testLogger(), fleetNow)
	ran := false
	liveMachine(t, reg, "laptop", 4, func(_ context.Context, d *memqlv1.ToolDispatch, _ func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error) {
		ran = true
		return okResult(d.GetCallId()), nil
	})
	stale := machine("laptop")
	stale.ConnectedNodeId = "some-node-that-no-longer-exists"

	store := &fakeStore{fakeFleet: &fakeFleet{machines: []Candidate{stale}}}
	d := newTestDispatcher(t, store, reg, "", nil)
	if _, err := d.Dispatch(context.Background(), approvedRequest()); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !ran {
		t.Fatal("a single-replica install must dispatch locally regardless of connectedNodeId")
	}
}

// --- the consent card -------------------------------------------------------

func TestConsentCardNamesTheRequirementAndTheCurrentChoice(t *testing.T) {
	mac := machine("Jose's MacBook", withLabels(map[string]string{"os": "darwin"}))
	store := &fakeStore{fakeFleet: &fakeFleet{machines: []Candidate{mac}}}
	d := newTestDispatcher(t, store, workerservice.NewRegistry(testLogger(), fleetNow), "", nil)

	got := d.ConsentCardTarget(context.Background(), "user-1", workerservice.CapabilityHeadless,
		map[string]string{"os": "darwin"}, nil)
	want := "on any of your machines matching os=darwin -- currently Jose's MacBook"
	if got != want {
		t.Fatalf("card text = %q, want %q -- the user's Allow covers the task on ANY matching "+
			"machine, so the card must describe the set as well as today's pick", got, want)
	}
}

func TestConsentCardSaysSoWhenNothingIsOnline(t *testing.T) {
	offline := machine("laptop")
	offline.LastSeenAt = fleetNow().Add(-time.Hour)
	store := &fakeStore{fakeFleet: &fakeFleet{machines: []Candidate{offline}}}
	d := newTestDispatcher(t, store, workerservice.NewRegistry(testLogger(), fleetNow), "", nil)
	got := d.ConsentCardTarget(context.Background(), "user-1", workerservice.CapabilityHeadless, nil, nil)
	if got != "on any of your machines -- none are online right now" {
		t.Fatalf("card text = %q", got)
	}
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
