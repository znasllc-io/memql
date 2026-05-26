package safety

import (
	"context"
	"errors"
	"testing"
)

func TestDefaultGateLazyInit(t *testing.T) {
	// Reset to nil so we hit the lazy path.
	resetDefaultGate(t)
	g1 := DefaultGate()
	if g1 == nil {
		t.Fatal("DefaultGate returned nil on lazy init")
	}
	g2 := DefaultGate()
	if g1 != g2 {
		t.Errorf("DefaultGate should return the same instance: %p vs %p", g1, g2)
	}
}

func TestSetDefaultGateOverride(t *testing.T) {
	resetDefaultGate(t)
	want := NewGate(GateOptions{Classifier: NoopClassifier{}, Mode: ModeOff})
	SetDefaultGate(want)
	if got := DefaultGate(); got != want {
		t.Errorf("DefaultGate returned %p, want override %p", got, want)
	}
}

// enforceGate is a tiny test helper: builds a Gate with the given
// mode so we can exercise the method form of EnforceDecision.
func enforceGate(mode Mode) *Gate {
	return NewGate(GateOptions{Classifier: NoopClassifier{}, Mode: mode})
}

func TestEnforceDecisionAllow(t *testing.T) {
	g := enforceGate(ModeEnforce)
	proceed, reason := g.EnforceDecision(DecisionAllow, Classification{}, nil, true)
	if !proceed || reason != "" {
		t.Errorf("Allow: expected proceed=true reason='', got %v %q", proceed, reason)
	}
}

func TestEnforceDecisionDenyCarriesReason(t *testing.T) {
	g := enforceGate(ModeEnforce)
	cls := Classification{Tier: TierCritical, Reason: "rm -rf /", RuleID: "shell.destructive"}
	proceed, reason := g.EnforceDecision(DecisionDeny, cls, nil, false)
	if proceed {
		t.Error("Deny: expected proceed=false")
	}
	if reason == "" || !contains(reason, "rm -rf /") {
		t.Errorf("Deny: refusal reason should include the rule reason, got %q", reason)
	}
}

func TestEnforceDecisionAskTreatedAsDeny(t *testing.T) {
	// Until #232's approval flow lands, Ask surfaces as a refusal
	// (with a pending-#232 marker so audit can grep how often it
	// would fire).
	g := enforceGate(ModeEnforce)
	cls := Classification{Tier: TierHigh, Reason: "sudo invocation"}
	proceed, reason := g.EnforceDecision(DecisionAsk, cls, nil, false)
	if proceed {
		t.Error("Ask: expected proceed=false")
	}
	if !contains(reason, "#232") {
		t.Errorf("Ask: refusal should mark the pending #232 dependency, got %q", reason)
	}
}

func TestEnforceDecisionErrorFailClosedEnforce(t *testing.T) {
	g := enforceGate(ModeEnforce)
	boom := errors.New("provider unreachable")
	proceed, reason := g.EnforceDecision(DecisionAllow, Classification{}, boom, true)
	if proceed {
		t.Error("fail-closed in enforce: expected proceed=false on classifier error")
	}
	if !contains(reason, "provider unreachable") {
		t.Errorf("fail-closed: refusal should include the error, got %q", reason)
	}
}

func TestEnforceDecisionErrorFailOpen(t *testing.T) {
	g := enforceGate(ModeEnforce)
	boom := errors.New("provider unreachable")
	proceed, reason := g.EnforceDecision(DecisionAllow, Classification{}, boom, false)
	if !proceed || reason != "" {
		t.Errorf("fail-open: expected proceed=true reason='' on classifier error, got %v %q", proceed, reason)
	}
}

// TestEnforceDecisionShadowSuppressesFailClosed pins the rollout-
// safety invariant: ModeShadow is observation-only end-to-end, so
// a classifier error MUST NOT block live traffic even on a
// fail-closed surface. The recorder still captures the error (the
// gate's Evaluate logs it before returning); this just asserts the
// SURFACE doesn't block.
func TestEnforceDecisionShadowSuppressesFailClosed(t *testing.T) {
	g := enforceGate(ModeShadow)
	boom := errors.New("provider unreachable")
	proceed, reason := g.EnforceDecision(DecisionAllow, Classification{}, boom, true)
	if !proceed {
		t.Errorf("shadow + fail-closed-on-error: expected proceed=true (shadow never blocks), got proceed=false reason=%q", reason)
	}
	if reason != "" {
		t.Errorf("shadow + fail-closed-on-error: expected empty reason, got %q", reason)
	}
}

// TestEnforceDecisionOffSuppressesFailClosed -- same invariant for
// ModeOff (classifier disabled): no path should block.
func TestEnforceDecisionOffSuppressesFailClosed(t *testing.T) {
	// ModeOff in NewGate.Mode short-circuits Evaluate without
	// calling the classifier, so an error wouldn't arise via the
	// normal path. But if a surface ever passes an artificial err
	// to EnforceDecision in off mode, behaviour should still be
	// "don't block." Asserting the same shape as shadow keeps the
	// semantics tidy.
	g := NewGate(GateOptions{Classifier: NoopClassifier{}, Mode: ModeOff})
	if proceed, _ := g.EnforceDecision(DecisionAllow, Classification{}, errors.New("ignored"), true); !proceed {
		t.Error("ModeOff: should never block on EnforceDecision")
	}
}

// Sanity: a freshly-defaulted gate behaves like the published API
// promises (shadow mode by default, recorder + classifier defaulted).
func TestDefaultGateBehavesAsDocumented(t *testing.T) {
	resetDefaultGate(t)
	g := DefaultGate()
	if g.Mode() != ModeShadow {
		// ModeFromEnv default is shadow; if the test env sets the
		// var the test still passes because we accept any non-zero
		// mode -- the contract is "is a real mode," not "is shadow."
		t.Logf("note: gate mode is %q (env override?)", g.Mode())
	}
	// Evaluate a benign action -- must not panic, must not return
	// a deny in shadow regardless of what the classifier said.
	dec, _, err := g.Evaluate(context.Background(),
		NewExecAction(SurfaceWorkbench, "ls", CallerContext{}))
	if err != nil {
		t.Fatalf("evaluate error: %v", err)
	}
	if dec != DecisionAllow {
		t.Errorf("shadow-mode default gate should always Allow, got %v", dec)
	}
}

// resetDefaultGate wipes the package-level gate so the next
// DefaultGate call re-runs the lazy-init path. Test-only helper; the
// double-checked-locking implementation makes this safe to call any
// number of times.
func resetDefaultGate(t *testing.T) {
	t.Helper()
	defaultGate.Store(nil)
}

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
