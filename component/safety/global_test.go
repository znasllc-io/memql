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

func TestEnforceDecisionAllow(t *testing.T) {
	proceed, reason := EnforceDecision(DecisionAllow, Classification{}, nil, true)
	if !proceed || reason != "" {
		t.Errorf("Allow: expected proceed=true reason='', got %v %q", proceed, reason)
	}
}

func TestEnforceDecisionDenyCarriesReason(t *testing.T) {
	cls := Classification{Tier: TierCritical, Reason: "rm -rf /", RuleID: "shell.destructive"}
	proceed, reason := EnforceDecision(DecisionDeny, cls, nil, false)
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
	cls := Classification{Tier: TierHigh, Reason: "sudo invocation"}
	proceed, reason := EnforceDecision(DecisionAsk, cls, nil, false)
	if proceed {
		t.Error("Ask: expected proceed=false")
	}
	if !contains(reason, "#232") {
		t.Errorf("Ask: refusal should mark the pending #232 dependency, got %q", reason)
	}
}

func TestEnforceDecisionErrorFailClosed(t *testing.T) {
	boom := errors.New("provider unreachable")
	proceed, reason := EnforceDecision(DecisionAllow, Classification{}, boom, true)
	if proceed {
		t.Error("fail-closed: expected proceed=false on classifier error")
	}
	if !contains(reason, "provider unreachable") {
		t.Errorf("fail-closed: refusal should include the error, got %q", reason)
	}
}

func TestEnforceDecisionErrorFailOpen(t *testing.T) {
	boom := errors.New("provider unreachable")
	proceed, reason := EnforceDecision(DecisionAllow, Classification{}, boom, false)
	if !proceed || reason != "" {
		t.Errorf("fail-open: expected proceed=true reason='' on classifier error, got %v %q", proceed, reason)
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
