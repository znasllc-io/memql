package planner

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestIsRateLimitError(t *testing.T) {
	rateLimited := []error{
		errors.New("anthropic: 429 Too Many Requests"),
		errors.New(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`),
		errors.New("provider overloaded (529)"),
		errors.New("OpenAI rate limit exceeded"),
		errors.New("plannerAgent invocation failed: 429"),
	}
	for _, e := range rateLimited {
		if !isRateLimitError(e) {
			t.Fatalf("expected rate-limit classification for %q", e)
		}
	}
	hard := []error{
		nil,
		errors.New("connection refused"),
		errors.New("invalid JSON schema"),
		errors.New("context deadline exceeded"),
	}
	for _, e := range hard {
		if isRateLimitError(e) {
			t.Fatalf("did NOT expect rate-limit classification for %v", e)
		}
	}
}

// TestInvokeAndDispatch_RateLimited_ParksNotFails is the acceptance test
// for memql#821: a 429 from the provider parks the plan (awaitingFeedback)
// and is NOT re-attempted (no retry storm) and NOT marked failed.
func TestInvokeAndDispatch_RateLimited_ParksNotFails(t *testing.T) {
	t.Setenv("MEMQL_PLANNER_MAX_INVOCATIONS_PER_PLAN", "50")
	t.Setenv("MEMQL_PLANNER_DEFAULT_TOKEN_BUDGET", "0")

	planRow := map[string]any{
		"id":          "v1:planner:plan:p1",
		"status":      "planning",
		"kind":        "userGoal",
		"goal":        "produce a thing",
		"requestedBy": "v1:identity:user:u1",
		"metrics":     map[string]any{"llmCallCount": float64(0)},
	}
	siCallCount := 0
	fe := &fakeEngine{
		execResponder: func(query string) (any, error) {
			switch {
			case strings.Contains(query, "queryPlanById"):
				return rowsEnvelope(planRow), nil
			case strings.Contains(query, "queryTasksForPlan"):
				return rowsEnvelope(), nil
			case strings.Contains(query, "queryActiveAgentsForUser"):
				return rowsEnvelope(), nil
			}
			return nil, nil
		},
		siResponder: func(_ string, _ map[string]any) (any, error) {
			siCallCount++
			return nil, errors.New("anthropic: 429 Too Many Requests (rate_limit_error)")
		},
	}
	l := NewPlannerAgentLoop(fe, testLogger())
	if err := l.invokeAndDispatchIter(context.Background(), "v1:planner:plan:p1", 0, newConvTracker()); err != nil {
		t.Fatalf("invokeAndDispatchIter returned error: %v", err)
	}

	if siCallCount != 1 {
		t.Fatalf("a 429 must NOT trigger an immediate re-attempt; InvokeSI called %d times", siCallCount)
	}
	exec, _, _ := fe.snapshot()
	if countContains(exec, "awaitingFeedback") == 0 {
		t.Fatalf("a 429 must park the plan to awaitingFeedback; exec=%v", exec)
	}
	if countContains(exec, `status:"failed"`) != 0 {
		t.Fatalf("a 429 must NOT mark the plan failed; exec=%v", exec)
	}
}
