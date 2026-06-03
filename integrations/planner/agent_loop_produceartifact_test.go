package planner

import (
	"context"
	"strings"
	"testing"
)

func TestMarkPlanningComplete_ProduceArtifact_AutoRuns(t *testing.T) {
	fe := &fakeEngine{}
	l := NewPlannerAgentLoop(fe, testLogger())
	if err := l.markPlanningComplete(context.Background(), "v1:planner:plan:p1", produceArtifactPlanKind); err != nil {
		t.Fatalf("markPlanningComplete: %v", err)
	}
	exec, _, _ := fe.snapshot()
	if countContains(exec, `status:"queued"`) == 0 {
		t.Fatalf("planning-complete must transition to queued; exec=%v", exec)
	}
	if countContains(exec, "mutationStartPlan") == 0 {
		t.Fatalf("produceArtifact must AUTO-RUN (mutationStartPlan) after planning; exec=%v", exec)
	}
}

func TestMarkPlanningComplete_UserGoal_DoesNotAutoRun(t *testing.T) {
	fe := &fakeEngine{}
	l := NewPlannerAgentLoop(fe, testLogger())
	if err := l.markPlanningComplete(context.Background(), "v1:planner:plan:p1", "userGoal"); err != nil {
		t.Fatalf("markPlanningComplete: %v", err)
	}
	exec, _, _ := fe.snapshot()
	if countContains(exec, `status:"queued"`) == 0 {
		t.Fatalf("planning-complete must transition to queued; exec=%v", exec)
	}
	if countContains(exec, "mutationStartPlan") != 0 {
		t.Fatalf("non-produceArtifact plans must NOT auto-run; exec=%v", exec)
	}
}

// TestInvokeAndDispatch_ProduceArtifact_FlowsThroughLoopAndAutoRuns is the
// acceptance test for memql#823: a produceArtifact plan now goes through
// the hardened decompose loop (it calls the plannerAgent, budgeted) and
// auto-runs once planning completes (no manual Run gate, no #816 bypass).
func TestInvokeAndDispatch_ProduceArtifact_FlowsThroughLoopAndAutoRuns(t *testing.T) {
	t.Setenv("MEMQL_PLANNER_MAX_INVOCATIONS_PER_PLAN", "50")
	t.Setenv("MEMQL_PLANNER_DEFAULT_TOKEN_BUDGET", "0")

	planRow := map[string]any{
		"id":          "v1:planner:plan:pa1",
		"status":      "planning",
		"kind":        produceArtifactPlanKind,
		"goal":        "Create a list of the top 10 most beautiful birds",
		"requestedBy": "v1:identity:user:u1",
		"ownerAgentId": "v1:agents:agent:sofia",
		"metrics":     map[string]any{"llmCallCount": float64(0)},
	}
	taskRow := map[string]any{
		"id":     "v1:planner:task:t1",
		"kind":   "produce",
		"status": "queued",
	}
	siCalls := 0
	fe := &fakeEngine{
		execResponder: func(query string) (any, error) {
			switch {
			case strings.Contains(query, "queryPlanById"):
				return rowsEnvelope(planRow), nil
			case strings.Contains(query, "queryTasksForPlan"):
				return rowsEnvelope(taskRow), nil
			case strings.Contains(query, "queryActiveAgentsForUser"):
				return rowsEnvelope(), nil
			}
			return nil, nil
		},
		siResponder: func(_ string, _ map[string]any) (any, error) {
			siCalls++
			// The planner finished emitting tasks -> "planning complete".
			return `{"action":"markPlanSucceeded","output":{"done":true}}`, nil
		},
	}
	l := NewPlannerAgentLoop(fe, testLogger())
	if err := l.invokeAndDispatchIter(context.Background(), "v1:planner:plan:pa1", 0, newConvTracker()); err != nil {
		t.Fatalf("invokeAndDispatchIter: %v", err)
	}

	if siCalls != 1 {
		t.Fatalf("produceArtifact must go through the plannerAgent loop (1 call), got %d", siCalls)
	}
	exec, _, _ := fe.snapshot()
	if countContains(exec, "mutationStartPlan") == 0 {
		t.Fatalf("produceArtifact must auto-run after planning completes; exec=%v", exec)
	}
}
