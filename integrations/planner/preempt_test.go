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
	contCalls     int
	contPrincipal auth.ForwardedPrincipal
}

func (f *fakePreemptForwarder) Forward(
	_ context.Context, _ string, _ node.NodeType, _ auth.ForwardedPrincipal, _ *memqlv1.MemqlClientMessage,
) (<-chan *memqlv1.MemqlServerMessage, error) {
	return nil, nil
}

func (f *fakePreemptForwarder) ForwardContinuation(
	requestId string, principal auth.ForwardedPrincipal, envelope *memqlv1.MemqlClientMessage,
) error {
	f.contCalls++
	f.contRequestId = requestId
	f.contEnvelope = envelope
	f.contPrincipal = principal
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

// The planner-side half of memql#3205's acceptance criterion 6. The receiver
// half -- that a REFUSED continuation cannot kill the parent turn -- is in
// component/grpc/ai_forward_parent_stream_test.go. This is the other half: in
// the steady state a preempt is not refused at all, because it now carries an
// assertion the receiver accepts.
//
// It used to pass `nil` claims, with a comment saying that was fine because the
// agent-side handler only sets a pause flag. Under the contract a missing
// assertion is REFUSED, and a refusal on a continuation is DROPPED rather than
// answered -- so a nil here would make "pause a Plan in cluster mode" fail
// SILENTLY: no error anywhere, and the turn simply never pauses.
func TestHandlePlanPreemptCarriesAnAcceptableAuthority(t *testing.T) {
	fwd := &fakePreemptForwarder{}
	p := &PlannerIntegration{logger: testLogger(), agentForwarder: fwd}
	p.executing.Store("v1:planner:plan:p9", "req-auth")

	p.handlePlanPreempt(pausedEvent("v1:planner:plan:p9"))

	if fwd.contCalls != 1 {
		t.Fatalf("want 1 ForwardContinuation call, got %d", fwd.contCalls)
	}
	a := fwd.contPrincipal.Authority
	if a.Kind != auth.ForwardedPrincipalSystem {
		t.Errorf("preempt principal kind = %v, want system", a.Kind)
	}
	if a.Subject != systemActorId {
		t.Errorf("preempt subject = %q, want the planner system actor %q", a.Subject, systemActorId)
	}

	// The check the receiver will actually run.
	if _, err := auth.VerifyForwardedAuthority(a, time.Now()); err != nil {
		t.Errorf("the receiver would REFUSE the preempt's authority: %v.\n\n"+
			"A refused continuation is dropped, not answered, so this failure mode is silent: "+
			"the Plan would stay running and nothing would report why.", err)
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
