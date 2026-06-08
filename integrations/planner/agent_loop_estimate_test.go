package planner

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/parser"
)

// --- tokenBudget stamp (#1108: malformed object literal) ------------------

// TestBuildStampTokenBudgetQuery_Parses guards the approval gate's
// tokenBudget stamp against the #1108 regression: a bare
// `tokenBudget:%d` field (unquoted key + bare numeric value, no space)
// was mis-lexed by the engine DSL -- the lexer folds the colon into the
// identifier, so `{tokenBudget:300000}` became a single key with no value
// and the parser rejected it with `expected ':' ... got "}"`. The stamp
// must now serialize to a well-formed object literal that the engine DSL
// parser accepts, for every budget value (0, small, large).
func TestBuildStampTokenBudgetQuery_Parses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		planId string
		budget int
	}{
		{"over-threshold", "v1:planner:plan:abcd1234", 300_000},
		{"small", "v1:planner:plan:abcd1234", 30},
		{"zero", "v1:planner:plan:abcd1234", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, err := buildStampTokenBudgetQuery(tc.planId, tc.budget)
			if err != nil {
				t.Fatalf("buildStampTokenBudgetQuery returned error: %v", err)
			}
			// It must parse through the real engine DSL parser -- the
			// pre-fix hand-concatenated form failed here with
			// `expected ':' or '=' after object key, got "}"`.
			if _, perr := parser.ParseExpression(q); perr != nil {
				t.Fatalf("stamp query must parse, got error: %v\nquery: %s", perr, q)
			}
			// And it must actually carry the budget (guards against a
			// "fix" that drops the field to dodge the parse error).
			if !strings.Contains(q, "tokenBudget") {
				t.Fatalf("stamp query lost the tokenBudget field: %s", q)
			}
		})
	}
}

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
