package planner

import "testing"

// TestSelectPlannerProvider_DefaultThreshold pins the core tiering
// contract: the cheap tier (no override) is used until the escalation
// iteration, then the reasoning tier kicks in. A trivial/moderate plan
// converges before the threshold and so NEVER escalates.
func TestSelectPlannerProvider_DefaultThreshold(t *testing.T) {
	// Default threshold is 3. Ensure env doesn't perturb the test.
	t.Setenv("MEMQL_PLANNER_REASONING_ESCALATION_ITER", "")
	t.Setenv("MEMQL_PLANNER_REASONING_PROVIDER", "")

	for iter := 0; iter < 3; iter++ {
		provider, reasoning := selectPlannerProvider(iter)
		if reasoning || provider != "" {
			t.Fatalf("iter %d must stay on the cheap tier (no override), got provider=%q reasoning=%v", iter, provider, reasoning)
		}
	}
	for iter := 3; iter <= 5; iter++ {
		provider, reasoning := selectPlannerProvider(iter)
		if !reasoning {
			t.Fatalf("iter %d (>= threshold 3) must escalate to reasoning", iter)
		}
		if provider != defaultPlannerReasoningProvider {
			t.Fatalf("iter %d escalation provider = %q, want %q", iter, provider, defaultPlannerReasoningProvider)
		}
	}
}

// TestSelectPlannerProvider_EscalationDisabled: threshold <= 0 means the
// planner NEVER escalates -- it stays cheap for every iteration.
func TestSelectPlannerProvider_EscalationDisabled(t *testing.T) {
	t.Setenv("MEMQL_PLANNER_REASONING_ESCALATION_ITER", "0")
	for _, iter := range []int{0, 1, 5, 50} {
		if provider, reasoning := selectPlannerProvider(iter); reasoning || provider != "" {
			t.Fatalf("escalation disabled: iter %d must stay cheap, got provider=%q reasoning=%v", iter, provider, reasoning)
		}
	}
}

// TestSelectPlannerProvider_EnvOverrides: a custom threshold + custom
// reasoning provider are both honored.
func TestSelectPlannerProvider_EnvOverrides(t *testing.T) {
	t.Setenv("MEMQL_PLANNER_REASONING_ESCALATION_ITER", "1")
	t.Setenv("MEMQL_PLANNER_REASONING_PROVIDER", "myReasoner")

	if provider, reasoning := selectPlannerProvider(0); reasoning || provider != "" {
		t.Fatalf("iter 0 must be cheap under threshold 1, got %q/%v", provider, reasoning)
	}
	provider, reasoning := selectPlannerProvider(1)
	if !reasoning || provider != "myReasoner" {
		t.Fatalf("iter 1 must escalate to the custom provider, got %q/%v", provider, reasoning)
	}
}

// TestReasoningEscalationIter_BadEnvFallsBack: a non-numeric override
// falls back to the default threshold rather than disabling/erroring.
func TestReasoningEscalationIter_BadEnvFallsBack(t *testing.T) {
	t.Setenv("MEMQL_PLANNER_REASONING_ESCALATION_ITER", "not-a-number")
	if got := reasoningEscalationIter(); got != defaultReasoningEscalationIter {
		t.Fatalf("bad env should fall back to %d, got %d", defaultReasoningEscalationIter, got)
	}
}
