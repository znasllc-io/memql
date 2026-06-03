package planner

import (
	"context"
	"strings"
	"testing"
)

// --- pure gate logic (the cap) ---------------------------------------------

func TestEvaluatePlannerCallGate_UnderCap_Allowed(t *testing.T) {
	plan := map[string]any{
		"metrics": map[string]any{"llmCallCount": float64(3)},
	}
	got := evaluatePlannerCallGate(plan, 5000, 50, 0)
	if got.Blocked {
		t.Fatalf("under the invocation cap should be allowed, got blocked: %s", got.Message)
	}
}

func TestEvaluatePlannerCallGate_AtInvocationCap_Blocked(t *testing.T) {
	plan := map[string]any{
		"metrics": map[string]any{"llmCallCount": float64(50)},
	}
	got := evaluatePlannerCallGate(plan, 5000, 50, 0)
	if !got.Blocked {
		t.Fatalf("at the invocation cap (50/50) must be blocked")
	}
	if got.Reason == "" {
		t.Fatalf("blocked gate must carry a feedback reason")
	}
	if !strings.Contains(got.Message, "ceiling") {
		t.Fatalf("message should explain the ceiling, got %q", got.Message)
	}
}

func TestEvaluatePlannerCallGate_OverInvocationCap_Blocked(t *testing.T) {
	// A re-triggered plan that somehow accumulated more than the cap
	// must still be blocked (>= semantics, never resets).
	plan := map[string]any{
		"metrics": map[string]any{"llmCallCount": float64(99)},
	}
	if got := evaluatePlannerCallGate(plan, 1, 50, 0); !got.Blocked {
		t.Fatalf("over the invocation cap must be blocked")
	}
}

func TestEvaluatePlannerCallGate_TokenBudgetExceeded_Blocked(t *testing.T) {
	plan := map[string]any{
		"metrics":     map[string]any{"llmCallCount": float64(1)},
		"tokenBudget": float64(1000),
		"tokenSpent":  float64(999),
	}
	// 100 estimated > 1 available -> blocked even though invocation
	// count is well under the cap.
	if got := evaluatePlannerCallGate(plan, 100, 50, 0); !got.Blocked {
		t.Fatalf("token budget exceeded must block the call")
	}
}

func TestEvaluatePlannerCallGate_DefaultBudgetEnforced(t *testing.T) {
	// Plan carries no explicit tokenBudget (0) -> the workspace default
	// budget applies.
	plan := map[string]any{
		"tokenSpent": float64(400),
	}
	if got := evaluatePlannerCallGate(plan, 200, 50, 500); !got.Blocked {
		t.Fatalf("default budget (500) with 400 spent + 200 est must block")
	}
	if got := evaluatePlannerCallGate(plan, 50, 50, 500); got.Blocked {
		t.Fatalf("default budget (500) with 400 spent + 50 est must be allowed")
	}
}

func TestEvaluatePlannerCallGate_CapDisabled_BypassesTokenCheck(t *testing.T) {
	plan := map[string]any{
		"tokenBudget":      float64(10),
		"tokenSpent":       float64(10),
		"tokenCapDisabled": true,
	}
	if got := evaluatePlannerCallGate(plan, 1_000_000, 50, 0); got.Blocked {
		t.Fatalf("tokenCapDisabled must bypass the token budget check")
	}
}

func TestPlanLLMCallCount_Shapes(t *testing.T) {
	cases := []struct {
		name string
		plan map[string]any
		want int
	}{
		{"no metrics", map[string]any{}, 0},
		{"empty metrics", map[string]any{"metrics": map[string]any{}}, 0},
		{"float64", map[string]any{"metrics": map[string]any{"llmCallCount": float64(7)}}, 7},
		{"int", map[string]any{"metrics": map[string]any{"llmCallCount": 9}}, 9},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := planLLMCallCount(c.plan); got != c.want {
				t.Fatalf("planLLMCallCount = %d, want %d", got, c.want)
			}
		})
	}
}

// --- end-to-end: cap reached -> park, NEVER another LLM call ---------------

func planRowEnvelope(row map[string]any) any { return rowsEnvelope(row) }

// TestInvokeAndDispatch_AtCap_ParksWithoutLLMCall is the acceptance
// test for memql#819: a plan whose persisted cumulative invocation
// count is at the ceiling must park (awaitingFeedback) and must NOT
// issue a plannerAgent InvokeSI call.
func TestInvokeAndDispatch_AtCap_ParksWithoutLLMCall(t *testing.T) {
	t.Setenv("MEMQL_PLANNER_MAX_INVOCATIONS_PER_PLAN", "2")
	t.Setenv("MEMQL_PLANNER_DEFAULT_TOKEN_BUDGET", "0")

	planRow := map[string]any{
		"id":          "v1:planner:plan:p1",
		"status":      "planning",
		"kind":        "userGoal",
		"goal":        "produce a thing",
		"requestedBy": "v1:identity:user:u1",
		"metrics":     map[string]any{"llmCallCount": float64(2)}, // at the cap of 2
	}
	fe := &fakeEngine{
		execResponder: func(query string) (any, error) {
			switch {
			case strings.Contains(query, "queryPlanById"):
				return planRowEnvelope(planRow), nil
			case strings.Contains(query, "queryTasksForPlan"):
				return rowsEnvelope(), nil
			case strings.Contains(query, "queryActiveAgentsForUser"):
				return rowsEnvelope(), nil
			}
			return nil, nil
		},
	}
	l := NewPlannerAgentLoop(fe, testLogger())
	if err := l.invokeAndDispatchIter(context.Background(), "v1:planner:plan:p1", 0); err != nil {
		t.Fatalf("invokeAndDispatchIter returned error: %v", err)
	}

	exec, si, _ := fe.snapshot()
	if len(si) != 0 {
		t.Fatalf("at the LLM ceiling the loop must NOT call InvokeSI, got %v", si)
	}
	if countContains(exec, "awaitingFeedback") == 0 {
		t.Fatalf("at the LLM ceiling the loop must park to awaitingFeedback; exec=%v", exec)
	}
	if countContains(exec, "mutationRecordPlannerInvocation") != 0 {
		t.Fatalf("a blocked call must NOT record an invocation; exec=%v", exec)
	}
}

// TestInvokeAndDispatch_UnderCap_CallsLLMAndRecords proves the gate
// does not over-block: under the ceiling the loop calls the
// plannerAgent and records the invocation against the cumulative
// counter.
func TestInvokeAndDispatch_UnderCap_CallsLLMAndRecords(t *testing.T) {
	t.Setenv("MEMQL_PLANNER_MAX_INVOCATIONS_PER_PLAN", "50")
	t.Setenv("MEMQL_PLANNER_DEFAULT_TOKEN_BUDGET", "0")

	planRow := map[string]any{
		"id":          "v1:planner:plan:p2",
		"status":      "planning",
		"kind":        "userGoal",
		"goal":        "produce a thing",
		"requestedBy": "v1:identity:user:u1",
		"metrics":     map[string]any{"llmCallCount": float64(0)},
	}
	fe := &fakeEngine{
		execResponder: func(query string) (any, error) {
			switch {
			case strings.Contains(query, "queryPlanById"):
				return planRowEnvelope(planRow), nil
			case strings.Contains(query, "queryTasksForPlan"):
				return rowsEnvelope(), nil
			case strings.Contains(query, "queryActiveAgentsForUser"):
				return rowsEnvelope(), nil
			}
			return nil, nil
		},
		// Return an escalate decision so the loop terminates cleanly
		// after one call instead of recursing.
		siResponder: func(_ string, _ map[string]any) (any, error) {
			return `{"action":"escalate","feedbackReason":"feedback_required","question":"need input"}`, nil
		},
	}
	l := NewPlannerAgentLoop(fe, testLogger())
	if err := l.invokeAndDispatchIter(context.Background(), "v1:planner:plan:p2", 0); err != nil {
		t.Fatalf("invokeAndDispatchIter returned error: %v", err)
	}

	exec, si, _ := fe.snapshot()
	if len(si) != 1 {
		t.Fatalf("under the ceiling the loop must call InvokeSI exactly once, got %v", si)
	}
	if countContains(exec, "mutationRecordPlannerInvocation") != 1 {
		t.Fatalf("a successful call must record exactly one invocation; exec=%v", exec)
	}
}
