package safety

import (
	"context"
	"errors"
	"testing"
)

// TestDefaultDecisionPolicy is the matrix table-test. Order matters
// in the rows: read top-to-bottom as "given (tier, categories,
// surface, scope), the policy decides X." A regression here is a
// behavioral change to enforcement -- always read the diff carefully.
func TestDefaultDecisionPolicy(t *testing.T) {
	policy := DefaultDecisionPolicy{}

	cases := []struct {
		name    string
		tier    RiskTier
		cats    []Category
		surface Surface
		scope   string
		want    Decision
	}{
		// --- tier baseline (workbench surface, scope=interact) ---
		{"none -> allow", TierNone, nil, SurfaceWorkbench, "interact", DecisionAllow},
		{"low -> allow", TierLow, nil, SurfaceWorkbench, "interact", DecisionAllow},
		{"medium + interact (sandbox) -> allow", TierMedium, nil, SurfaceWorkbench, "interact", DecisionAllow},
		{"medium + full (sandbox) -> allow", TierMedium, nil, SurfaceWorkbench, "full", DecisionAllow},
		{"medium + observe (sandbox) -> ask", TierMedium, nil, SurfaceWorkbench, "observe", DecisionAsk},
		{"medium + empty (sandbox) -> ask", TierMedium, nil, SurfaceWorkbench, "", DecisionAsk},
		{"high -> ask regardless of scope", TierHigh, nil, SurfaceWorkbench, "full", DecisionAsk},
		{"critical -> deny", TierCritical, nil, SurfaceWorkbench, "full", DecisionDeny},

		// --- computer-use is one notch stricter on medium ---
		{"medium + interact on computer-use-headless -> ask", TierMedium, nil, SurfaceComputerUseHeadless, "interact", DecisionAsk},
		{"medium + full on computer-use-embodied -> ask", TierMedium, nil, SurfaceComputerUseEmbodied, "full", DecisionAsk},
		// (high/critical unchanged on computer-use -- already Ask/Deny.)
		{"high on computer-use -> ask", TierHigh, nil, SurfaceComputerUseHeadless, "full", DecisionAsk},
		{"critical on computer-use -> deny", TierCritical, nil, SurfaceComputerUseHeadless, "full", DecisionDeny},

		// --- category floors ---
		{"low + remote_code -> critical (deny)", TierLow, []Category{CategoryRemoteCode}, SurfaceWorkbench, "full", DecisionDeny},
		{"none + remote_code -> critical (deny)", TierNone, []Category{CategoryRemoteCode}, SurfaceWorkbench, "full", DecisionDeny},
		{"low + credential_access -> high (ask)", TierLow, []Category{CategoryCredentialAccess}, SurfaceWorkbench, "full", DecisionAsk},
		{"none + exfiltration -> high (ask)", TierNone, []Category{CategoryExfiltration}, SurfaceWorkbench, "full", DecisionAsk},
		{"low + sandbox_escape -> high (ask)", TierLow, []Category{CategorySandboxEscape}, SurfaceWorkbench, "full", DecisionAsk},
		// Multiple categories take the strictest floor.
		{"low + credential_access + remote_code -> critical (deny)",
			TierLow,
			[]Category{CategoryCredentialAccess, CategoryRemoteCode},
			SurfaceWorkbench, "full", DecisionDeny},
		// Floor does NOT downgrade an already-stricter tier.
		{"critical + (no cats) stays critical", TierCritical, nil, SurfaceWorkbench, "full", DecisionDeny},
		{"high + credential_access stays high (ask)", TierHigh, []Category{CategoryCredentialAccess}, SurfaceWorkbench, "full", DecisionAsk},

		// --- non-floor categories are informational, don't shift ---
		{"low + network_egress -> still allow", TierLow, []Category{CategoryNetworkEgress}, SurfaceWorkbench, "full", DecisionAllow},
		{"low + destructive (no floor for it alone) -> still allow",
			TierLow, []Category{CategoryDestructive}, SurfaceWorkbench, "full", DecisionAllow},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := policy.Decide(DecisionInput{
				Classification: Classification{Tier: tc.tier, Categories: tc.cats},
				Surface:        tc.surface,
				Scope:          tc.scope,
			})
			if got != tc.want {
				t.Errorf("Decide(%v, cats=%v, %v, scope=%q) = %v, want %v",
					tc.tier, tc.cats, tc.surface, tc.scope, got, tc.want)
			}
		})
	}
}

func TestScopeAtLeastInteract(t *testing.T) {
	cases := map[string]bool{
		"":            false,
		"observe":     false,
		"OBSERVE":     false,
		"interact":    true,
		"INTERACT":    true,
		"  interact ": true,
		"full":        true,
		"FULL":        true,
		"administer":  false,
		"unknown":     false,
	}
	for in, want := range cases {
		if got := scopeAtLeastInteract(in); got != want {
			t.Errorf("scopeAtLeastInteract(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestIsComputerUseSurface(t *testing.T) {
	yes := []Surface{SurfaceComputerUseHeadless, SurfaceComputerUseEmbodied}
	no := []Surface{SurfaceWorkbench, SurfaceToolWebhook, SurfaceToolIntegration, "", "weird"}
	for _, s := range yes {
		if !isComputerUseSurface(s) {
			t.Errorf("isComputerUseSurface(%q) should be true", s)
		}
	}
	for _, s := range no {
		if isComputerUseSurface(s) {
			t.Errorf("isComputerUseSurface(%q) should be false", s)
		}
	}
}

// --- Gate integration: the policy is consulted, but shadow mode
// still short-circuits to Allow. The whole point of #231 is that
// enabling the matrix doesn't risk shadow rollouts. ---

// denyAllPolicy is a deterministic test policy that always says
// Deny, so we can verify the Gate honours the result in enforce
// mode and ignores it in shadow.
type denyAllPolicy struct{}

func (denyAllPolicy) Decide(_ DecisionInput) Decision { return DecisionDeny }

// staticVerdictClassifier emits a fixed classification.
type staticVerdictClassifier struct{ cls Classification }

func (s staticVerdictClassifier) Classify(_ context.Context, _ ActionDescriptor) (Classification, error) {
	return s.cls, nil
}

func TestGateConsultsDecisionPolicyInEnforce(t *testing.T) {
	g := NewGate(GateOptions{
		Classifier:     staticVerdictClassifier{cls: Classification{Tier: TierLow}}, // benign verdict
		DecisionPolicy: denyAllPolicy{},                                             // but our policy denies anyway
		Mode:           ModeEnforce,
	})
	dec, _, err := g.Evaluate(context.Background(),
		NewExecAction(SurfaceWorkbench, "ls", CallerContext{}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if dec != DecisionDeny {
		t.Errorf("enforce mode should honour DecisionPolicy verdict, got %v", dec)
	}
}

func TestGateIgnoresDecisionPolicyInShadow(t *testing.T) {
	// Same setup as above but Mode=Shadow -- the recorded decision
	// is Deny (so the recorder can see what would have happened),
	// but the returned decision is Allow. This is the load-bearing
	// invariant: shadow rollouts never block live traffic, even
	// when #231's matrix would have denied.
	g := NewGate(GateOptions{
		Classifier:     staticVerdictClassifier{cls: Classification{Tier: TierCritical}},
		DecisionPolicy: denyAllPolicy{},
		Mode:           ModeShadow,
	})
	dec, _, err := g.Evaluate(context.Background(),
		NewExecAction(SurfaceComputerUseHeadless, "rm -rf /", CallerContext{}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if dec != DecisionAllow {
		t.Errorf("shadow mode must always return Allow regardless of policy verdict, got %v", dec)
	}
}

func TestGateDefaultsToDefaultDecisionPolicy(t *testing.T) {
	// A NewGate with no DecisionPolicy set should use
	// DefaultDecisionPolicy -- not nil-panic, not the old
	// always-Allow stub.
	g := NewGate(GateOptions{
		Classifier: staticVerdictClassifier{cls: Classification{Tier: TierCritical}},
		Mode:       ModeEnforce,
	})
	dec, _, err := g.Evaluate(context.Background(),
		NewExecAction(SurfaceWorkbench, "rm -rf /", CallerContext{}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if dec != DecisionDeny {
		t.Errorf("default policy should deny critical, got %v -- did the always-Allow stub leak back in?", dec)
	}
}

// boomy classifier exercises the error path -- the gate stays
// neutral on classifier error regardless of policy.
type boomyClassifier struct{}

func (boomyClassifier) Classify(_ context.Context, _ ActionDescriptor) (Classification, error) {
	return Classification{}, errors.New("boom")
}

func TestGatePolicyNotConsultedOnClassifierError(t *testing.T) {
	g := NewGate(GateOptions{
		Classifier:     boomyClassifier{},
		DecisionPolicy: denyAllPolicy{},
		Mode:           ModeEnforce,
	})
	dec, _, err := g.Evaluate(context.Background(),
		NewExecAction(SurfaceWorkbench, "ls", CallerContext{}))
	if err == nil {
		t.Error("expected error from classifier to propagate")
	}
	// Classifier error -> gate returns Allow + error; the
	// DecisionPolicy never sees the (empty) verdict. Surface
	// adapters apply per-surface fail-open/closed (per #229).
	if dec != DecisionAllow {
		t.Errorf("on classifier error, gate stays neutral (Allow), got %v", dec)
	}
}
