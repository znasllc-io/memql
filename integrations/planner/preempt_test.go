package planner

import (
	"context"
	"testing"

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
}

func (f *fakePreemptForwarder) Forward(
	_ context.Context, _ string, _ node.NodeType, _ map[string]string, _ string, _ *memqlv1.MemqlClientMessage,
) (<-chan *memqlv1.MemqlServerMessage, error) {
	return nil, nil
}

func (f *fakePreemptForwarder) ForwardContinuation(
	requestId string, _ map[string]string, _ string, envelope *memqlv1.MemqlClientMessage,
) error {
	f.contCalls++
	f.contRequestId = requestId
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
