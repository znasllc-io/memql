package planner

import "testing"

// --- estimate (acceptance: unit-test the estimate) ------------------------

func TestEstimatePlanCostTokens_Monotone(t *testing.T) {
	short := map[string]any{"goal": "a list of 10 birds"}
	long := map[string]any{"goal": string(make([]byte, 4000))} // ~1000 goal tokens
	if estimatePlanCostTokens(long) <= estimatePlanCostTokens(short) {
		t.Fatalf("a longer goal must estimate higher: short=%d long=%d",
			estimatePlanCostTokens(short), estimatePlanCostTokens(long))
	}

	// More phases => higher estimate.
	fewPhases := map[string]any{"goal": "x", "phases": []any{map[string]any{}, map[string]any{}}}
	manyPhases := map[string]any{"goal": "x", "phases": []any{
		map[string]any{}, map[string]any{}, map[string]any{}, map[string]any{}, map[string]any{}, map[string]any{},
	}}
	if estimatePlanCostTokens(manyPhases) <= estimatePlanCostTokens(fewPhases) {
		t.Fatalf("more phases must estimate higher: few=%d many=%d",
			estimatePlanCostTokens(fewPhases), estimatePlanCostTokens(manyPhases))
	}

	// A trivial goal stays well below the default threshold (so it would
	// auto-run even if it reached this loop).
	if est := estimatePlanCostTokens(short); est > defaultPlanApprovalTokenThreshold {
		t.Fatalf("a trivial goal estimate (%d) must be below the default threshold (%d)",
			est, defaultPlanApprovalTokenThreshold)
	}
}

// --- gate (acceptance: unit-test the gate) --------------------------------

func TestEvaluatePlanApprovalGate(t *testing.T) {
	plan := map[string]any{"goal": "x"}

	// Below threshold -> not blocked (auto-run).
	if r := evaluatePlanApprovalGate(plan, 100, 250_000); r.Blocked {
		t.Fatalf("estimate below threshold must auto-run")
	}
	// Above threshold -> blocked.
	r := evaluatePlanApprovalGate(plan, 300_000, 250_000)
	if !r.Blocked || r.Message == "" {
		t.Fatalf("estimate above threshold must park for approval, got %+v", r)
	}
	// Exactly at threshold -> NOT blocked (strict >).
	if r := evaluatePlanApprovalGate(plan, 250_000, 250_000); r.Blocked {
		t.Fatalf("estimate exactly at threshold must auto-run (strict >)")
	}
	// Threshold <= 0 -> gate disabled, never blocks.
	if r := evaluatePlanApprovalGate(plan, 10_000_000, 0); r.Blocked {
		t.Fatalf("threshold 0 disables the gate")
	}
}

func TestEvaluatePlanApprovalGate_AlreadyApproved(t *testing.T) {
	// metrics.budgetApproved == true -> never blocks even far over threshold.
	approved := map[string]any{"goal": "x", "metrics": map[string]any{"budgetApproved": true}}
	if r := evaluatePlanApprovalGate(approved, 9_000_000, 250_000); r.Blocked {
		t.Fatalf("an approved plan must not re-park")
	}
	// tokenCapDisabled escape hatch -> never blocks.
	capOff := map[string]any{"goal": "x", "tokenCapDisabled": true}
	if r := evaluatePlanApprovalGate(capOff, 9_000_000, 250_000); r.Blocked {
		t.Fatalf("tokenCapDisabled must bypass the approval gate")
	}
}

func TestPlanApprovalTokenThreshold_EnvOverride(t *testing.T) {
	t.Setenv("MEMQL_PLANNER_APPROVAL_TOKEN_THRESHOLD", "1000")
	if got := planApprovalTokenThreshold(); got != 1000 {
		t.Fatalf("env override not honored: got %d", got)
	}
	t.Setenv("MEMQL_PLANNER_APPROVAL_TOKEN_THRESHOLD", "0")
	if got := planApprovalTokenThreshold(); got != 0 {
		t.Fatalf("threshold 0 (disable) must be honored: got %d", got)
	}
	t.Setenv("MEMQL_PLANNER_APPROVAL_TOKEN_THRESHOLD", "garbage")
	if got := planApprovalTokenThreshold(); got != defaultPlanApprovalTokenThreshold {
		t.Fatalf("bad env should fall back to default: got %d", got)
	}
}
