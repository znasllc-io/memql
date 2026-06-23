// agent_loop_budget.go
//
// Hard per-plan LLM ceilings for the Planner Agent decompose/dispatch
// loop (memql#819). The existing maxPlannerIterations cap only bounds
// re-invocations WITHIN a single event cycle -- it resets to 0 every
// time HandlePlanCreated / a future re-trigger re-enters the loop, so
// it cannot stop a Plan from spending without bound across cycles or
// retries.
//
// This file adds two CUMULATIVE ceilings that are persisted on the Plan
// row (so they survive across cycles, retries, and re-triggers -- a
// re-triggered Plan can NOT reset its allowance):
//
//  1. invocation count  -- Plan.metrics.llmCallCount vs a hard ceiling.
//  2. token budget      -- component/planner.budget.go's CheckCall,
//     keyed on Plan.tokenBudget / tokenSpent (or a workspace default).
//
// Before every plannerAgent InvokeAI call the loop evaluates the gate;
// on exceed it parks the Plan (awaitingFeedback) and NEVER makes
// another LLM call. After every successful call it records the
// invocation (count++ and tokenSpent += estimate) so the ceilings
// keep advancing toward their limit.
package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strconv"

	compplanner "github.com/znasllc-io/memql/component/planner"
)

const (
	// defaultMaxPlannerInvocationsPerPlan is the hard cumulative cap on
	// total plannerAgent LLM calls for a single Plan, across ALL event
	// cycles / retries / re-triggers. Dropped 50 -> 8 (epic #836 / #843):
	// under the goal-resolution restructure a trivial deliverable never
	// enters this loop (the #837 triage shortcuts it) and a moderate plan
	// converges in a handful of calls (decompose + a couple of
	// dispatchTask + completion), so 8 is plenty for a real plan while
	// making a misbehaving one park within a handful of calls instead of
	// spending up to 50x. Genuinely huge work decomposes via child
	// sub-plans (requestSubPlan), each independently bounded by this same
	// ceiling -- that is how the loop scales simple -> complex without any
	// single Plan running away. Override with
	// MEMQL_PLANNER_MAX_INVOCATIONS_PER_PLAN.
	defaultMaxPlannerInvocationsPerPlan = 8

	// defaultPlannerTokenBudget is the fallback per-plan token ceiling
	// budget.go enforces when Plan.tokenBudget is unset (0). A runaway
	// backstop: with the trimmed prompt (#820) a normal decompose
	// spends well under this, so it only bites pathological loops.
	// Override with MEMQL_PLANNER_DEFAULT_TOKEN_BUDGET (0 disables the
	// fallback -- then only Plans carrying their own tokenBudget are
	// token-gated).
	defaultPlannerTokenBudget = 2_000_000

	// plannerSystemPromptTokenEstimate approximates the fixed system
	// prompt + structured-output schema overhead of one plannerAgent
	// call, added to the per-call input estimate.
	plannerSystemPromptTokenEstimate = 1500

	// plannerCharsPerTokenEstimate is the rough chars-per-token ratio
	// used to estimate a call's input size from the marshaled inputs.
	plannerCharsPerTokenEstimate = 4
)

// maxPlannerInvocationsPerPlan returns the cumulative invocation cap,
// honoring the env override.
func maxPlannerInvocationsPerPlan() int {
	if v := os.Getenv("MEMQL_PLANNER_MAX_INVOCATIONS_PER_PLAN"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultMaxPlannerInvocationsPerPlan
}

// plannerDefaultTokenBudget returns the fallback token budget, honoring
// the env override (0 = no fallback enforcement).
func plannerDefaultTokenBudget() int {
	if v := os.Getenv("MEMQL_PLANNER_DEFAULT_TOKEN_BUDGET"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return defaultPlannerTokenBudget
}

// planLLMCallCount reads the persisted cumulative plannerAgent call
// count off Plan.metrics.llmCallCount. Zero when the metrics object or
// the field is absent (a brand-new Plan).
func planLLMCallCount(plan map[string]any) int {
	metrics, _ := plan["metrics"].(map[string]any)
	if metrics == nil {
		return 0
	}
	return asInt(metrics["llmCallCount"])
}

// staticPlanLookup satisfies compplanner.PlanLookup from an in-memory
// TokenState snapshot, so the budget check reuses the Plan row the loop
// already loaded instead of issuing another query.
type staticPlanLookup struct{ state compplanner.TokenState }

func (s staticPlanLookup) GetPlanTokenState(_ context.Context, _ string) (compplanner.TokenState, error) {
	return s.state, nil
}

// tokenStateFromPlan projects the budget-relevant fields off a loaded
// (shape-flattened) Plan row.
func tokenStateFromPlan(plan map[string]any) compplanner.TokenState {
	return compplanner.TokenState{
		Budget:        asInt(plan["tokenBudget"]),
		Spent:         asInt(plan["tokenSpent"]),
		AllocatedToCh: asInt(plan["tokenAllocatedToChildren"]),
		CapDisabled:   asBool(plan["tokenCapDisabled"]),
	}
}

// estimatePlannerCallTokens approximates the input-token cost of one
// plannerAgent call from the marshaled inputs plus the fixed system
// prompt overhead. Deliberately conservative -- the goal is a budget
// guardrail, not exact accounting (InvokeAI returns no usage stats).
func estimatePlannerCallTokens(data map[string]any) int {
	b, err := json.Marshal(data)
	if err != nil {
		return plannerSystemPromptTokenEstimate
	}
	return plannerSystemPromptTokenEstimate + len(b)/plannerCharsPerTokenEstimate
}

// plannerGateResult is the verdict from evaluatePlannerCallGate.
type plannerGateResult struct {
	Blocked bool
	Reason  string
	Message string
}

// evaluatePlannerCallGate decides whether the NEXT plannerAgent call is
// allowed for this Plan, enforcing the two cumulative ceilings. Pure +
// deterministic (no engine, no I/O) so the cap logic is unit-testable.
// When Blocked, the caller parks the Plan and makes NO LLM call.
func evaluatePlannerCallGate(plan map[string]any, estTokens, maxInvocations, defaultBudget int) plannerGateResult {
	if count := planLLMCallCount(plan); count >= maxInvocations {
		return plannerGateResult{
			Blocked: true,
			Reason:  "feedback_required",
			Message: fmt.Sprintf(
				"Planner reached its hard per-plan LLM ceiling (%d total invocations across all cycles) without converging. Resume to grant a fresh allowance.",
				maxInvocations),
		}
	}
	budget := compplanner.NewEngineTokenBudget(staticPlanLookup{tokenStateFromPlan(plan)}, defaultBudget)
	if err := budget.CheckCall(context.Background(), "", estTokens); err != nil {
		return plannerGateResult{
			Blocked: true,
			Reason:  "feedback_required",
			Message: fmt.Sprintf(
				"Planner hit its per-plan token budget before this call (%v). Resume to grant more budget.", err),
		}
	}
	return plannerGateResult{}
}

// recordPlannerInvocation persists the cumulative counter advance after
// a plannerAgent call: metrics.llmCallCount++ and tokenSpent += the
// estimated cost. Best-effort -- a failed record is logged but does not
// abort the loop (the next loadPlan re-reads whatever did persist).
func (l *PlannerAgentLoop) recordPlannerInvocation(ctx context.Context, plan map[string]any, planId string, estTokens int) {
	newCount := planLLMCallCount(plan) + 1
	newSpent := asInt(plan["tokenSpent"]) + estTokens

	// Preserve any other metrics fields already on the Plan -- the
	// update replaces the whole metrics object, so merge rather than
	// clobber.
	merged := map[string]any{}
	if metrics, ok := plan["metrics"].(map[string]any); ok {
		for k, v := range metrics {
			merged[k] = v
		}
	}
	merged["llmCallCount"] = newCount
	metricsJSON, err := json.Marshal(merged)
	if err != nil {
		metricsJSON = []byte(`{}`)
	}
	q := fmt.Sprintf(
		`recordPlannerInvocation({planId:%q, tokenSpent:%d, metrics:%s})`,
		planId, newSpent, string(metricsJSON),
	)
	if _, err := l.engine.Execute(systemActorContext(ctx), q); err != nil {
		l.logger.Warn("planner agent loop: recordPlannerInvocation failed",
			"planId", planId, "error", err)
	}
}

// asInt coerces a JSON-materialized numeric (float64 / int / int64 /
// json.Number / numeric string) to int. Returns 0 for nil or
// unparseable values.
func asInt(v any) int {
	switch n := v.(type) {
	case nil:
		return 0
	case int:
		return n
	case int64:
		return clampInt64ToInt(n, 0)
	case float64:
		// Bound the float before narrowing so the int64() conversion
		// can't overflow, then funnel through the guarded sink.
		if math.IsNaN(n) || n > math.MaxInt32 || n < math.MinInt32 {
			return 0
		}
		return clampInt64ToInt(int64(n), 0)
	case json.Number:
		i, _ := n.Int64()
		return clampInt64ToInt(i, 0)
	case string:
		i, _ := strconv.Atoi(n)
		return i
	default:
		return 0
	}
}

// asBool coerces a JSON-materialized bool (or "true"/"false" string) to
// bool. Returns false for nil or unrecognized values.
func asBool(v any) bool {
	switch b := v.(type) {
	case bool:
		return b
	case string:
		return b == "true"
	default:
		return false
	}
}
