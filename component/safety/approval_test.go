package safety

import (
	"context"
	"strings"
	"testing"
)

// stubApprovalSink lets tests pin a verdict.
type stubApprovalSink struct {
	verdict ApprovalVerdict
	calls   int
}

func (s *stubApprovalSink) Check(_ context.Context, _ ActionDescriptor, _ Classification) ApprovalVerdict {
	s.calls++
	return s.verdict
}

// ruleClsClassifier returns a fixed Ask-tier verdict so the policy
// matrix routes to Ask without needing real rules. The default
// matrix returns Ask on high tier (and on medium with insufficient
// scope / computer_use); high+no-floor is the simplest path.
type ruleClsClassifier struct{}

func (ruleClsClassifier) Classify(_ context.Context, _ ActionDescriptor) (Classification, error) {
	return Classification{
		Tier:   TierHigh,
		Source: SourceRule,
		Reason: "test rule",
		RuleID: "test.rule",
	}, nil
}

func newAskGateWithSink(sink ApprovalSink) *Gate {
	return NewGate(GateOptions{
		Classifier:   ruleClsClassifier{},
		Mode:         ModeEnforce,
		ApprovalSink: sink,
	})
}

func newExecDesc() ActionDescriptor {
	return NewExecAction(SurfaceWorkbench, "rm -rf /tmp/test", CallerContext{
		AgentID: "a1", OwnerUserID: "u1",
	})
}

func TestEvaluateConsultsApprovalSinkOnAskInEnforce(t *testing.T) {
	sink := &stubApprovalSink{}
	g := newAskGateWithSink(sink)
	_, _, _ = g.Evaluate(context.Background(), newExecDesc())
	if sink.calls != 1 {
		t.Errorf("expected sink to be consulted exactly once on Ask, got %d", sink.calls)
	}
}

func TestEvaluateApprovedFlipsToAllow(t *testing.T) {
	sink := &stubApprovalSink{verdict: ApprovalVerdict{
		State:             ApprovalStateApproved,
		ApprovalRequestID: "v1:safety:approvalRequest:abc123",
	}}
	g := newAskGateWithSink(sink)
	decision, cls, _ := g.Evaluate(context.Background(), newExecDesc())
	if decision != DecisionAllow {
		t.Errorf("approved -> decision should be Allow, got %s", decision)
	}
	if !strings.Contains(cls.Reason, "approved bypass") {
		t.Errorf("reason should mark this as an approved bypass: %q", cls.Reason)
	}
	if !strings.Contains(cls.Reason, "v1:safety:approvalRequest:abc123") {
		t.Errorf("reason should carry the approvalRequest id: %q", cls.Reason)
	}
}

func TestEvaluateDeniedFlipsToDeny(t *testing.T) {
	sink := &stubApprovalSink{verdict: ApprovalVerdict{
		State:             ApprovalStateDenied,
		ApprovalRequestID: "v1:safety:approvalRequest:xyz",
	}}
	g := newAskGateWithSink(sink)
	decision, cls, _ := g.Evaluate(context.Background(), newExecDesc())
	if decision != DecisionDeny {
		t.Errorf("denied -> decision should be Deny, got %s", decision)
	}
	if !strings.Contains(cls.Reason, "denied by approver") {
		t.Errorf("reason should mark this as denied: %q", cls.Reason)
	}
}

func TestEvaluatePendingKeepsAskWithId(t *testing.T) {
	sink := &stubApprovalSink{verdict: ApprovalVerdict{
		State:             ApprovalStatePending,
		ApprovalRequestID: "v1:safety:approvalRequest:pending",
	}}
	g := newAskGateWithSink(sink)
	decision, cls, _ := g.Evaluate(context.Background(), newExecDesc())
	if decision != DecisionAsk {
		t.Errorf("pending -> decision should be Ask, got %s", decision)
	}
	if !strings.Contains(cls.Reason, "awaiting approval") {
		t.Errorf("reason should mark this as awaiting approval: %q", cls.Reason)
	}
	if !strings.Contains(cls.Reason, "v1:safety:approvalRequest:pending") {
		t.Errorf("reason should carry the approvalRequest id: %q", cls.Reason)
	}
	// EnforceDecision should refuse Ask + surface the reason that now
	// carries the request id.
	proceed, refusal := g.EnforceDecision(decision, cls, nil, false)
	if proceed {
		t.Errorf("pending Ask should refuse, proceed=%v", proceed)
	}
	if !strings.Contains(refusal, "v1:safety:approvalRequest:pending") {
		t.Errorf("refusal text should expose the approvalRequest id: %q", refusal)
	}
}

func TestEvaluateUnconfiguredLeavesAskUnchanged(t *testing.T) {
	// Default (no sink in options) -- NoopApprovalSink returns
	// Unconfigured. Behavior pre-#232 must be intact.
	g := NewGate(GateOptions{
		Classifier: ruleClsClassifier{},
		Mode:       ModeEnforce,
	})
	decision, cls, _ := g.Evaluate(context.Background(), newExecDesc())
	if decision != DecisionAsk {
		t.Errorf("unconfigured -> Ask preserved, got %s", decision)
	}
	if strings.Contains(cls.Reason, "approvalRequest=") {
		t.Errorf("unconfigured sink should NOT decorate reason with an id: %q", cls.Reason)
	}
	if cls.Reason != "test rule" {
		t.Errorf("unconfigured should leave reason untouched: %q", cls.Reason)
	}
}

func TestShadowModeSkipsApprovalSink(t *testing.T) {
	// Load-bearing safety invariant: shadow mode NEVER side-effects
	// (no row creation, no engine writes). The sink must not be
	// consulted in shadow even when the policy would route to Ask.
	sink := &stubApprovalSink{verdict: ApprovalVerdict{
		State: ApprovalStateApproved, ApprovalRequestID: "x"}}
	g := NewGate(GateOptions{
		Classifier:   ruleClsClassifier{},
		Mode:         ModeShadow,
		ApprovalSink: sink,
	})
	decision, _, _ := g.Evaluate(context.Background(), newExecDesc())
	if decision != DecisionAllow {
		t.Errorf("shadow should always Allow, got %s", decision)
	}
	if sink.calls != 0 {
		t.Errorf("shadow must NOT consult the approval sink: calls=%d", sink.calls)
	}
}

func TestEvaluateAllowSkipsApprovalSink(t *testing.T) {
	// Non-Ask decisions must not consult the sink -- the sink only
	// covers the Ask flow. Pin to allow via a low-tier rule.
	type allowCls struct{}
	allow := classifierFunc(func(_ context.Context, _ ActionDescriptor) (Classification, error) {
		return Classification{Tier: TierLow, Source: SourceRule}, nil
	})
	sink := &stubApprovalSink{}
	g := NewGate(GateOptions{
		Classifier:   allow,
		Mode:         ModeEnforce,
		ApprovalSink: sink,
	})
	decision, _, _ := g.Evaluate(context.Background(), newExecDesc())
	if decision != DecisionAllow {
		t.Errorf("low-tier should Allow, got %s", decision)
	}
	if sink.calls != 0 {
		t.Errorf("Allow path must NOT consult the sink: calls=%d", sink.calls)
	}
}

// classifierFunc adapts a func to the Classifier interface for the
// inline test above. Local to this file; not exported.
type classifierFunc func(ctx context.Context, desc ActionDescriptor) (Classification, error)

func (f classifierFunc) Classify(ctx context.Context, desc ActionDescriptor) (Classification, error) {
	return f(ctx, desc)
}

func TestApprovalCorrelationKeyStableAcrossCallerContext(t *testing.T) {
	// Caller context (agent / user / plan) must NOT affect the key:
	// the "same action by the same agent retried" case is the
	// idempotency case -- a key that varied with caller context
	// would create a fresh approvalRequest per retry, defeating the
	// whole point.
	a := NewExecAction(SurfaceWorkbench, "rm /tmp/foo", CallerContext{AgentID: "agent1"})
	b := NewExecAction(SurfaceWorkbench, "rm /tmp/foo", CallerContext{AgentID: "agent2", OwnerUserID: "u9"})
	if ApprovalCorrelationKey(a) != ApprovalCorrelationKey(b) {
		t.Errorf("correlation key must be stable across caller context")
	}
}

func TestApprovalCorrelationKeyDiffersByCommand(t *testing.T) {
	a := NewExecAction(SurfaceWorkbench, "rm /tmp/foo", CallerContext{})
	b := NewExecAction(SurfaceWorkbench, "rm /tmp/bar", CallerContext{})
	if ApprovalCorrelationKey(a) == ApprovalCorrelationKey(b) {
		t.Errorf("different commands MUST produce different keys")
	}
}

func TestApprovalCorrelationKeyStableAcrossArgsOrder(t *testing.T) {
	a := NewToolAction(SurfaceToolWebhook, "x",
		map[string]any{"a": 1, "b": 2, "c": 3}, CallerContext{})
	b := NewToolAction(SurfaceToolWebhook, "x",
		map[string]any{"c": 3, "b": 2, "a": 1}, CallerContext{})
	if ApprovalCorrelationKey(a) != ApprovalCorrelationKey(b) {
		t.Errorf("args map order must not affect key (sorted keys)")
	}
}

func TestNoopApprovalSinkAlwaysUnconfigured(t *testing.T) {
	v := NoopApprovalSink{}.Check(context.Background(), ActionDescriptor{}, Classification{})
	if v.State != ApprovalStateUnconfigured {
		t.Errorf("noop sink must return Unconfigured, got %s", v.State)
	}
}
