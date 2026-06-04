package planner

import (
	"context"
	"log/slog"
	"testing"
)

// Acceptance for memql#835: with produceArtifact routed through the
// unified loop (the hardcoded HandlePlanCreated bypass removed), a
// "create a simple file" plan resolves in a BOUNDED, small number of
// planner calls -- specifically ZERO plannerAgent decompose calls and
// ZERO classifier calls, landing on a single direct production turn
// (mutationStartPlan). This is the deterministic stand-in for the
// "validate locally that produceArtifact makes a small bounded number of
// planner calls" gate the issue requires.
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
			if containsAll(query, "queryPlanById") {
				return planRow, nil
			}
			return nil, nil
		},
		siResponder: func(_ string, _ map[string]any) (any, error) { return nil, nil },
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
	// ZERO classifier calls either -- produceArtifact is known-trivial.
	if n := countContains(si, "goalComplexityTriage"); n != 0 {
		t.Fatalf("produceArtifact must skip the complexity classifier, got %d goalComplexityTriage calls", n)
	}
	// Exactly one direct production turn kicked off.
	if n := countContains(exec, "mutationStartPlan"); n != 1 {
		t.Fatalf("produceArtifact must resolve to one direct production turn (mutationStartPlan), got %d", n)
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
		if containsAll(query, "queryPlanById") {
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
	if countContains(exec, "mutationStartPlan") != 0 {
		t.Fatalf("no-owner produceArtifact must NOT start a direct turn, got %d mutationStartPlan", countContains(exec, "mutationStartPlan"))
	}
}
