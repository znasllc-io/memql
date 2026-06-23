package planner

import (
	"context"
	"log/slog"
	"testing"
)

// Acceptance for memql#835 as amended by memql#1393: a small
// produceArtifact plan still resolves in a BOUNDED number of planner
// calls through the unified loop -- ZERO plannerAgent decompose calls
// and a single direct production turn (startPlan) -- but the
// known-trivial shortcut is gone: exactly ONE cheap classifier call
// decides the route (so a LARGE deliverable can decompose instead of
// timing out the 3m turn wallclock).
func TestProduceArtifact_ThroughLoop_NoPlannerCalls(t *testing.T) {
	planRow := map[string]any{
		"output": []any{map[string]any{
			"id":           "pa-1",
			"kind":         produceArtifactPlanKind,
			"status":       "planning",
			"goal":         "create a markdown file listing 10 birds",
			"ownerAgentId": "agent-1",
			"requestedBy":  "user-1",
		}},
	}
	fe := &fakeEngine{
		execResponder: func(query string) (any, error) {
			if containsAll(query, "planById") {
				return planRow, nil
			}
			return nil, nil
		},
		aiResponder: func(templateId string, _ map[string]any) (any, error) {
			if templateId == "goalComplexityTriage" {
				return map[string]any{"complexity": "trivial", "reasoning": "one list"}, nil
			}
			return nil, nil
		},
	}
	l := &PlannerAgentLoop{engine: fe, logger: slog.New(slog.NewTextHandler(discardWriter{}, nil))}

	if err := l.invokeAndDispatch(context.Background(), "pa-1"); err != nil {
		t.Fatalf("invokeAndDispatch returned error: %v", err)
	}

	exec, si, _ := fe.snapshot()
	// ZERO plannerAgent decompose calls -- the spam path that cost ~$250.
	if n := countContains(si, "plannerAgent"); n != 0 {
		t.Fatalf("produceArtifact must make ZERO plannerAgent decompose calls, got %d", n)
	}
	// Exactly ONE cheap classifier call routes it (#1393).
	if n := countContains(si, "goalComplexityTriage"); n != 1 {
		t.Fatalf("produceArtifact must make exactly 1 goalComplexityTriage call, got %d", n)
	}
	// Exactly one direct production turn kicked off.
	if n := countContains(exec, "startPlan"); n != 1 {
		t.Fatalf("produceArtifact must resolve to one direct production turn (startPlan), got %d", n)
	}
}

// Without an owning agent, produceArtifact can't run a single turn, so it
// falls through to the (now hard-capped) decompose loop rather than
// silently dropping -- it does NOT shortcut.
func TestProduceArtifact_NoOwner_DoesNotShortcut(t *testing.T) {
	planRow := map[string]any{
		"output": []any{map[string]any{
			"id": "pa-2", "kind": produceArtifactPlanKind, "status": "planning",
			"goal": "make a file", "requestedBy": "user-1",
		}},
	}
	fe := &fakeEngine{execResponder: func(query string) (any, error) {
		if containsAll(query, "planById") {
			return planRow, nil
		}
		return nil, nil
	}}
	l := &PlannerAgentLoop{engine: fe, logger: slog.New(slog.NewTextHandler(discardWriter{}, nil))}
	handled, err := l.triageAndMaybeShortcut(context.Background(), "pa-2", "")
	if err != nil || handled {
		t.Fatalf("produceArtifact with no ownerAgentId must not shortcut: handled=%v err=%v", handled, err)
	}
	exec, _, _ := fe.snapshot()
	if countContains(exec, "startPlan") != 0 {
		t.Fatalf("no-owner produceArtifact must NOT start a direct turn, got %d startPlan", countContains(exec, "startPlan"))
	}
}
