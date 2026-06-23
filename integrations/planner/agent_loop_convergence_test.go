package planner

import (
	"context"
	"strings"
	"testing"
)

func TestConvTracker_IdenticalNonTerminal_ParksPastThreshold(t *testing.T) {
	t.Setenv("MEMQL_PLANNER_MAX_IDENTICAL_DECISIONS", "2")
	c := newConvTracker()
	d := plannerDecision{Action: "decompose", PlanOutline: []phaseOutline{{Kind: "produce", Label: "Produce"}}}
	// Allowed twice, park on the 3rd identical emission.
	if park, _ := c.recordAndCheck(d); park {
		t.Fatalf("1st identical decision must not park")
	}
	if park, _ := c.recordAndCheck(d); park {
		t.Fatalf("2nd identical decision must not park")
	}
	if park, count := c.recordAndCheck(d); !park {
		t.Fatalf("3rd identical decision must park (count=%d)", count)
	}
}

func TestConvTracker_DistinctDecisions_NeverPark(t *testing.T) {
	t.Setenv("MEMQL_PLANNER_MAX_IDENTICAL_DECISIONS", "2")
	c := newConvTracker()
	// dispatchTask for 10 DIFFERENT tasks -> distinct fingerprints -> no park.
	for i := 0; i < 10; i++ {
		d := plannerDecision{Action: "dispatchTask", Task: plannerTask{
			Kind: "produce", LogicalStepId: "step-" + string(rune('a'+i)),
		}}
		if park, _ := c.recordAndCheck(d); park {
			t.Fatalf("distinct dispatchTask %d must never park", i)
		}
	}
}

func TestConvTracker_TerminalActions_NeverTracked(t *testing.T) {
	c := newConvTracker()
	for _, action := range []string{"markPlanSucceeded", "markPlanFailed", "escalate"} {
		for i := 0; i < 20; i++ {
			if park, _ := c.recordAndCheck(plannerDecision{Action: action}); park {
				t.Fatalf("terminal action %q must never be tracked/parked", action)
			}
		}
	}
}

// TestInvokeAndDispatch_OscillatingPlanner_Parks is the acceptance test
// for memql#822: a planner that keeps emitting the SAME non-terminal
// decision parks (awaitingFeedback) instead of spinning -- and does so
// before exhausting the raw per-cycle iteration cap.
func TestInvokeAndDispatch_OscillatingPlanner_Parks(t *testing.T) {
	t.Setenv("MEMQL_PLANNER_MAX_INVOCATIONS_PER_PLAN", "50")
	t.Setenv("MEMQL_PLANNER_DEFAULT_TOKEN_BUDGET", "0")
	t.Setenv("MEMQL_PLANNER_MAX_IDENTICAL_DECISIONS", "2")

	planRow := map[string]any{
		"id":          "v1:planner:plan:p1",
		"status":      "planning",
		"kind":        "userGoal",
		"goal":        "produce a thing",
		"requestedBy": "v1:identity:user:u1",
		"metrics":     map[string]any{"llmCallCount": float64(0)},
	}
	aiCalls := 0
	fe := &fakeEngine{
		execResponder: func(query string) (any, error) {
			switch {
			case strings.Contains(query, "planById"):
				return rowsEnvelope(planRow), nil
			case strings.Contains(query, "tasksForPlan"):
				return rowsEnvelope(), nil
			case strings.Contains(query, "activeAgentsForUser"):
				return rowsEnvelope(), nil
			}
			return nil, nil
		},
		// Always emit the IDENTICAL decompose decision -> oscillation.
		aiResponder: func(_ string, _ map[string]any) (any, error) {
			aiCalls++
			return `{"action":"decompose","plan_outline":[{"kind":"produce","label":"Produce"}]}`, nil
		},
	}
	l := NewPlannerAgentLoop(fe, testLogger())
	if err := l.invokeAndDispatchIter(context.Background(), "v1:planner:plan:p1", 0, newConvTracker()); err != nil {
		t.Fatalf("invokeAndDispatchIter returned error: %v", err)
	}

	// maxIdentical=2 -> parks on the 3rd identical decision => 3 LLM
	// calls, which is BELOW the per-cycle iteration cap (5). The
	// convergence guard, not the raw cap, is what stopped it.
	if aiCalls != 3 {
		t.Fatalf("oscillating planner should park after 3 identical decisions, got %d InvokeAI calls", aiCalls)
	}
	exec, _, _ := fe.snapshot()
	if countContains(exec, "awaitingFeedback") == 0 {
		t.Fatalf("oscillating planner must park to awaitingFeedback; exec=%v", exec)
	}
}
