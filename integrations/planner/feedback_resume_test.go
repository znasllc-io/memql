package planner

import (
	"context"
	"strings"
	"testing"
)

// feedback_resume_test.go -- coverage for the decompose-loop resume-from-
// feedback re-entry (epic memql#1404 / memql#1405). maybeResumeFromFeedback
// fires when a Plan resumed from awaitingFeedback (carrying the user's
// free-text answer on feedbackResponse) re-enters running; it must be a no-op
// for a normal task-driven running transition, and it must re-enter exactly
// once per resume (planId@startedAt) so a re-delivered resume event doesn't
// double-drive the loop on this replica.

func feedbackResumePlanRow(status, startedAt string, feedbackResp map[string]any) map[string]any {
	row := map[string]any{
		"id":          "v1:planner:plan:p1",
		"status":      status,
		"kind":        "userGoal",
		"goal":        "produce a thing",
		"requestedBy": "v1:identity:user:u1",
		"startedAt":   startedAt,
		"metrics":     map[string]any{"llmCallCount": float64(0)},
	}
	if feedbackResp != nil {
		row["feedbackResponse"] = feedbackResp
	}
	return row
}

func TestMaybeResumeFromFeedback_NoFeedbackResponse_NoOp(t *testing.T) {
	// A normal task-driven running transition carries no feedbackResponse;
	// maybeResumeFromFeedback must return false (NOT a resume) and never enter
	// invokeAndDispatch (no plannerAgent SI call).
	fe := &fakeEngine{
		execResponder: func(query string) (any, error) {
			if strings.Contains(query, "queryPlanById") {
				return rowsEnvelope(feedbackResumePlanRow("running", "2026-06-14T00:00:00Z", nil)), nil
			}
			return nil, nil
		},
	}
	l := NewPlannerAgentLoop(fe, testLogger())
	if l.maybeResumeFromFeedback(context.Background(), "v1:planner:plan:p1") {
		t.Fatalf("a running transition with no feedbackResponse must NOT be treated as a feedback resume")
	}
	_, si, _ := fe.snapshot()
	if len(si) != 0 {
		t.Fatalf("no-op resume must make no plannerAgent SI call, got %v", si)
	}
}

func TestMaybeResumeFromFeedback_Resumes_AndDedups(t *testing.T) {
	t.Setenv("MEMQL_PLANNER_MAX_INVOCATIONS_PER_PLAN", "50")
	t.Setenv("MEMQL_PLANNER_DEFAULT_TOKEN_BUDGET", "0")
	t.Setenv("MEMQL_PLANNER_MAX_IDENTICAL_DECISIONS", "2")

	fb := map[string]any{
		"response":    "use metric units and keep it under 200 words",
		"respondedBy": "v1:identity:user:u1",
		"respondedAt": "2026-06-14T00:00:00Z",
	}
	siCalls := 0
	fe := &fakeEngine{
		execResponder: func(query string) (any, error) {
			switch {
			case strings.Contains(query, "queryPlanById"):
				return rowsEnvelope(feedbackResumePlanRow("running", "2026-06-14T00:00:01Z", fb)), nil
			case strings.Contains(query, "queryTasksForPlan"),
				strings.Contains(query, "queryActiveAgentsForUser"):
				return rowsEnvelope(), nil
			}
			return nil, nil
		},
		siResponder: func(_ string, _ map[string]any) (any, error) {
			siCalls++
			// A terminal decision so invokeAndDispatch settles in one pass.
			return `{"action":"markPlanSucceeded","reply":"done"}`, nil
		},
	}
	l := NewPlannerAgentLoop(fe, testLogger())

	// First resume: must be claimed + handled (returns true) and must drive
	// the loop (at least one plannerAgent SI call once the gate/triage pass).
	if !l.maybeResumeFromFeedback(context.Background(), "v1:planner:plan:p1") {
		t.Fatalf("a running transition carrying a feedbackResponse must be handled as a resume")
	}

	// Second delivery of the SAME resume (same planId@startedAt): deduped on
	// this replica -- still returns true (handled) but does NOT re-drive the
	// loop a second time.
	siAfterFirst := siCalls
	if !l.maybeResumeFromFeedback(context.Background(), "v1:planner:plan:p1") {
		t.Fatalf("a re-delivered resume must still report handled=true")
	}
	if siCalls != siAfterFirst {
		t.Fatalf("a re-delivered resume must NOT re-drive the loop; SI calls went %d -> %d", siAfterFirst, siCalls)
	}
}
