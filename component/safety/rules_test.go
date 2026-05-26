package safety

import (
	"context"
	"errors"
	"testing"
)

// fixedRule returns a Rule that always emits the given verdict.
func fixedRule(id string, tier RiskTier, reason string) Rule {
	return Rule{ID: id, Fn: func(_ ActionDescriptor) (Classification, error) {
		return Classification{Tier: tier, Reason: reason, Confidence: 1.0}, nil
	}}
}

// silentRule never has an opinion -- exercises the escalation path.
func silentRule(id string) Rule {
	return Rule{ID: id, Fn: func(_ ActionDescriptor) (Classification, error) {
		return Classification{}, ErrNoClassifierOpinion
	}}
}

// boomRule returns a non-escalation error -- exercises propagation.
func boomRule(id string, err error) Rule {
	return Rule{ID: id, Fn: func(_ ActionDescriptor) (Classification, error) {
		return Classification{}, err
	}}
}

func TestRuleClassifierFirstVerdictWins(t *testing.T) {
	r := NewRuleClassifier(
		fixedRule("first.rule", TierHigh, "first matched"),
		fixedRule("second.rule", TierLow, "should be skipped"),
	)
	cls, err := r.Classify(context.Background(), ActionDescriptor{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if cls.Tier != TierHigh || cls.RuleID != "first.rule" || cls.Source != SourceRule {
		t.Errorf("unexpected verdict: %+v", cls)
	}
	if cls.Reason != "first matched" {
		t.Errorf("Reason should be the per-incident text, got %q", cls.Reason)
	}
}

func TestRuleClassifierAdvancesOnEscalation(t *testing.T) {
	r := NewRuleClassifier(
		silentRule("first"),
		silentRule("second"),
		fixedRule("third.rule", TierMedium, ""),
	)
	cls, err := r.Classify(context.Background(), ActionDescriptor{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if cls.Tier != TierMedium || cls.RuleID != "third.rule" {
		t.Errorf("expected third to fire, got %+v", cls)
	}
	// Empty Reason falls back to RuleID.
	if cls.Reason != "third.rule" {
		t.Errorf("expected Reason fallback to RuleID, got %q", cls.Reason)
	}
}

func TestRuleClassifierAllEscalateReturnsOpinion(t *testing.T) {
	r := NewRuleClassifier(silentRule("a"), silentRule("b"))
	_, err := r.Classify(context.Background(), ActionDescriptor{})
	if !errors.Is(err, ErrNoClassifierOpinion) {
		t.Errorf("expected ErrNoClassifierOpinion, got %v", err)
	}
}

func TestRuleClassifierEmptyEscalates(t *testing.T) {
	_, err := NewRuleClassifier().Classify(context.Background(), ActionDescriptor{})
	if !errors.Is(err, ErrNoClassifierOpinion) {
		t.Errorf("expected ErrNoClassifierOpinion, got %v", err)
	}
}

func TestRuleClassifierPropagatesRealError(t *testing.T) {
	boom := errors.New("rule broke")
	r := NewRuleClassifier(silentRule("a"), boomRule("b", boom))
	_, err := r.Classify(context.Background(), ActionDescriptor{})
	if !errors.Is(err, boom) {
		t.Errorf("expected real error to propagate, got %v", err)
	}
}

// TestDefaultRulesDangerousWinsOverSafeBinary pins the priority
// invariant -- a destructive `rm -rf /` must NOT get fast-pathed by
// the safe-binary rule even though `rm` isn't in safeReadOnlyBinaries
// (it's a defensive double-check; the safe-binary rule wouldn't have
// matched, but we explicitly assert it).
func TestDefaultRulesDangerousWinsOverSafeBinary(t *testing.T) {
	r := NewRuleClassifier(DefaultRules()...)
	cls, err := r.Classify(context.Background(),
		NewExecAction(SurfaceComputerUseHeadless, "rm -rf /", CallerContext{}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if cls.Tier != TierCritical || cls.RuleID != "shell.destructive" {
		t.Errorf("expected destructive verdict, got %+v", cls)
	}
}

// TestDefaultRulesSafeBinaryFastPath pins that `ls` (safe) escapes
// the LLM via the fast-path rule.
func TestDefaultRulesSafeBinaryFastPath(t *testing.T) {
	r := NewRuleClassifier(DefaultRules()...)
	cls, err := r.Classify(context.Background(),
		NewExecAction(SurfaceComputerUseHeadless, "ls -la /tmp", CallerContext{}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if cls.Tier != TierLow || cls.RuleID != "shell.safe_binary" {
		t.Errorf("expected safe_binary verdict, got %+v", cls)
	}
}

// TestDefaultRulesUnknownEscalates pins the "no opinion → escalate"
// path for a command no rule decides on.
func TestDefaultRulesUnknownEscalates(t *testing.T) {
	r := NewRuleClassifier(DefaultRules()...)
	_, err := r.Classify(context.Background(),
		NewExecAction(SurfaceComputerUseHeadless, "kubectl get pods", CallerContext{}))
	if !errors.Is(err, ErrNoClassifierOpinion) {
		t.Errorf("expected escalation, got %v", err)
	}
}

// TestRuleClassifierComposesWithChain pins that the rule classifier
// integrates with the #227 ChainClassifier: rules → (LLM stub).
// Here the LLM is a fixed Noop; what matters is that the chain
// advances past the rule classifier when no rule decides.
func TestRuleClassifierComposesWithChain(t *testing.T) {
	rules := NewRuleClassifier(silentRule("a"))
	chain := NewChainClassifier(rules, NoopClassifier{})
	cls, err := chain.Classify(context.Background(), ActionDescriptor{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if cls.Source != SourceNoop {
		t.Errorf("expected noop verdict via chain fallthrough, got %+v", cls)
	}
}
