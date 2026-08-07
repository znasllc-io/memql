package planner

import (
	"context"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/events"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/node"
)

func TestIsPreemptTriggerStatus(t *testing.T) {
	for _, s := range []string{"paused", "cancelled"} {
		if !isPreemptTriggerStatus(s) {
			t.Errorf("%q should trigger preempt of an in-flight turn", s)
		}
	}
	for _, s := range []string{"running", "succeeded", "failed", "waitingForSlot", "awaitingFeedback", ""} {
		if isPreemptTriggerStatus(s) {
			t.Errorf("%q should NOT trigger preempt", s)
		}
	}
}

// fakePreemptForwarder captures ForwardContinuation calls so the test can
// assert the planner addressed the right in-flight turn.
type fakePreemptForwarder struct {
	contRequestId string
	contEnvelope  *memqlv1.MemqlClientMessage
	contAuthority *auth.ForwardedAuthority
	contCalls     int
}

func (f *fakePreemptForwarder) Forward(
	_ context.Context, _ string, _ node.NodeType, _ *auth.ForwardedAuthority, _ string, _ *memqlv1.MemqlClientMessage,
) (<-chan *memqlv1.MemqlServerMessage, error) {
	return nil, nil
}

func (f *fakePreemptForwarder) ForwardContinuation(
	requestId string, authority *auth.ForwardedAuthority, _ string, envelope *memqlv1.MemqlClientMessage,
) error {
	f.contCalls++
	f.contRequestId = requestId
	f.contAuthority = authority
	f.contEnvelope = envelope
	return nil
}

func pausedEvent(planId string) events.Event {
	return events.Event{
		Topic:   "graph.node.updated.v1:planner:plan",
		Payload: map[string]any{"id": planId, "status": "paused"},
	}
}

func TestHandlePlanPreempt_SendsSignalForInFlightTurn(t *testing.T) {
	fwd := &fakePreemptForwarder{}
	p := &PlannerIntegration{logger: testLogger(), agentForwarder: fwd}
	p.executing.Store("v1:planner:plan:p1", "req-abc")

	p.handlePlanPreempt(pausedEvent("v1:planner:plan:p1"))

	if fwd.contCalls != 1 {
		t.Fatalf("want 1 ForwardContinuation call, got %d", fwd.contCalls)
	}
	if fwd.contRequestId != "req-abc" {
		t.Fatalf("preempt addressed requestId %q, want req-abc", fwd.contRequestId)
	}
	pre := fwd.contEnvelope.GetAgentPreemptTurn()
	if pre == nil {
		t.Fatalf("envelope is not an AgentPreemptTurn: %+v", fwd.contEnvelope)
	}
	if pre.GetRequestId() != "req-abc" {
		t.Fatalf("AgentPreemptTurn.RequestId = %q, want req-abc", pre.GetRequestId())
	}
}

// TestHandlePlanPreempt_AssertsInternalAuthority pins the pause path against
// the mesh forwarded-auth contract (memql#3205).
//
// This producer used to pass nil, which under the contract is the one thing
// the receiver refuses -- so pausing a Plan in cluster mode would have started
// failing. It asserts AuthorityKindInternal instead: "no principal" as an
// explicit VALUE. The preempt handler only flips a pause flag keyed by request
// id; it binds no actor and persists nothing, so there is nothing to name.
//
// The refusal path matters here in particular: this continuation reuses the
// PARENT turn's request id, so a refusal that closed the stream would blank an
// in-flight reply while the agent kept running. See
// TestRefusalOnAContinuationDoesNotCloseTheParentStream in component/grpc.
func TestHandlePlanPreempt_AssertsInternalAuthority(t *testing.T) {
	fwd := &fakePreemptForwarder{}
	p := &PlannerIntegration{logger: testLogger(), agentForwarder: fwd}
	p.executing.Store("v1:planner:plan:p1", "req-abc")

	p.handlePlanPreempt(pausedEvent("v1:planner:plan:p1"))

	if fwd.contAuthority == nil {
		t.Fatal("pause signal carried a nil authority; the agent node would REFUSE it and the Plan would never pause")
	}
	if fwd.contAuthority.Kind != auth.AuthorityKindInternal {
		t.Errorf("pause authority kind = %q, want internal", fwd.contAuthority.Kind)
	}
	if err := fwd.contAuthority.Validate(time.Now()); err != nil {
		t.Errorf("the agent node would refuse the pause signal: %v", err)
	}
	if ac := fwd.contAuthority.AccessContext(); ac != nil {
		t.Errorf("the pause signal bound an actor (%+v); it must bind none", ac)
	}
}

// TestPlanDispatchNamesThePlannerSystemActor covers the other planner
// producer: the post-approval agent dispatch, which DOES need an actor
// (createWorkerInvocation stamps createdBy through the engine's mutation
// path, and memql#1107 is the bug where it had none).
//
// It drives the REAL executeApprovedPlan and inspects what that call actually
// handed the forwarder. An earlier version of this test constructed
// auth.SystemAuthority(systemActorId) itself and asserted properties of the
// component/auth package -- which is a test of the contract type, not of the
// producer: reverting plan_execution.go to InternalAuthority(), to cognition's
// actor, or to nil would have left it green. That is precisely the failure
// mode memql#3205 called out in the attempt it superseded.
func TestPlanDispatchNamesThePlannerSystemActor(t *testing.T) {
	const planId = "v1:planner:plan:dispatch-authority"
	eng := &fakeEngine{execResponder: guardTestPlanResponder(planId, "running")}
	fwd := &fakeTurnForwarder{}
	p := newGuardTestIntegration(t, eng, fwd, nil, testLogger())

	p.executeApprovedPlan(context.Background(), planId, "req-dispatch")

	if fwd.forwardCount() != 1 {
		t.Fatalf("expected exactly one dispatched agent turn, got %d", fwd.forwardCount())
	}

	authority := fwd.forwardedAuthority()
	if authority == nil {
		t.Fatal("the post-approval dispatch forwarded a nil authority; the agent node would REFUSE it and no turn would run")
	}
	if err := authority.Validate(time.Now()); err != nil {
		t.Fatalf("the agent node would refuse the planner's dispatch: %v", err)
	}
	if authority.Kind != auth.AuthorityKindSystem {
		t.Errorf("dispatch kind = %q, want system", authority.Kind)
	}

	ac := authority.AccessContext()
	if ac == nil {
		t.Fatal("the planner's dispatch bound no actor; createWorkerInvocation would fail with \"no actor found in context\" (memql#1107)")
	}
	if ac.UserId != auth.SystemActorPlanner {
		t.Errorf("dispatch actor = %q, want %q -- a row stamped by this turn must be attributable to the PLANNER, not to cognition",
			ac.UserId, auth.SystemActorPlanner)
	}
	// The role is the receiver's to decide, not the sender's.
	if ac.Role != auth.SystemActorRole {
		t.Errorf("dispatch role = %q, want the receiver-pinned %q", ac.Role, auth.SystemActorRole)
	}
	if ac.IsClusterOwner() {
		t.Error("the planner's system dispatch resolved as cluster owner")
	}
}

func TestHandlePlanPreempt_NoInFlightTurnIsNoOp(t *testing.T) {
	fwd := &fakePreemptForwarder{}
	p := &PlannerIntegration{logger: testLogger(), agentForwarder: fwd}
	// Nothing in the executing map for this plan.
	p.handlePlanPreempt(pausedEvent("v1:planner:plan:not-running"))
	if fwd.contCalls != 0 {
		t.Fatalf("no in-flight turn -> no signal; got %d calls", fwd.contCalls)
	}
}

func TestHandlePlanPreempt_NonPreemptStatusIgnored(t *testing.T) {
	fwd := &fakePreemptForwarder{}
	p := &PlannerIntegration{logger: testLogger(), agentForwarder: fwd}
	p.executing.Store("v1:planner:plan:p2", "req-xyz")
	// A running update for an in-flight plan must NOT preempt it.
	ev := events.Event{
		Topic:   "graph.node.updated.v1:planner:plan",
		Payload: map[string]any{"id": "v1:planner:plan:p2", "status": "running"},
	}
	p.handlePlanPreempt(ev)
	if fwd.contCalls != 0 {
		t.Fatalf("running status must not preempt; got %d calls", fwd.contCalls)
	}
}
