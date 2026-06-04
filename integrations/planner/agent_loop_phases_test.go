package planner

import (
	"context"
	"log/slog"
	"testing"
)

func ph(status string, expected, completed int) phaseView {
	return phaseView{Status: status, ExpectedTaskCount: expected, CompletedTaskCount: completed}
}

// --- pure phase-checkpoint state machine ----------------------------------

func TestEvaluatePhaseProgress_Done(t *testing.T) {
	phases := []phaseView{ph("done", 2, 2), ph("done", 1, 1)}
	if a, _ := evaluatePhaseProgress(phases, 2); a != phaseActionDone {
		t.Fatalf("all-complete must be Done, got %s", a)
	}
}

func TestEvaluatePhaseProgress_CheckpointAtNonTrivialBoundary(t *testing.T) {
	// Phase 0 done, phase 1 not started, phase 1 expects 3 (>= minTasks 2)
	// -> checkpoint before phase 1.
	phases := []phaseView{ph("done", 2, 2), ph("pending", 3, 0)}
	a, idx := evaluatePhaseProgress(phases, 2)
	if a != phaseActionCheckpoint || idx != 1 {
		t.Fatalf("expected checkpoint at phase 1, got %s idx=%d", a, idx)
	}
}

func TestEvaluatePhaseProgress_TrivialNextAutoContinues(t *testing.T) {
	// Phase 0 done, next phase expects only 1 (< minTasks 2) -> continue.
	phases := []phaseView{ph("done", 2, 2), ph("pending", 1, 0)}
	if a, _ := evaluatePhaseProgress(phases, 2); a != phaseActionContinue {
		t.Fatalf("a trivial next phase must auto-continue, got %s", a)
	}
}

func TestEvaluatePhaseProgress_MidPhaseNoCheckpoint(t *testing.T) {
	// Phase 0 done, phase 1 already in progress (completed>0) -> not a fresh
	// boundary, continue.
	phases := []phaseView{ph("done", 2, 2), ph("active", 3, 1)}
	if a, _ := evaluatePhaseProgress(phases, 2); a != phaseActionContinue {
		t.Fatalf("mid-phase must not checkpoint, got %s", a)
	}
}

func TestEvaluatePhaseProgress_FirstPhaseNoCheckpoint(t *testing.T) {
	// Nothing complete yet -> the first phase isn't a boundary; continue.
	phases := []phaseView{ph("pending", 5, 0), ph("pending", 5, 0)}
	if a, idx := evaluatePhaseProgress(phases, 2); a != phaseActionContinue || idx != 0 {
		t.Fatalf("first phase must continue (no boundary), got %s idx=%d", a, idx)
	}
}

func TestEvaluatePhaseProgress_MinTasksZeroCheckpointsEveryBoundary(t *testing.T) {
	phases := []phaseView{ph("done", 1, 1), ph("pending", 1, 0)}
	if a, _ := evaluatePhaseProgress(phases, 0); a != phaseActionCheckpoint {
		t.Fatalf("minTasks=0 must checkpoint at every boundary, got %s", a)
	}
}

func TestPhaseComplete(t *testing.T) {
	if !phaseComplete(ph("done", 0, 0)) {
		t.Fatalf("status=done is complete")
	}
	if !phaseComplete(ph("active", 3, 3)) {
		t.Fatalf("completed==expected is complete")
	}
	if phaseComplete(ph("active", 3, 2)) {
		t.Fatalf("completed<expected is NOT complete")
	}
	if phaseComplete(ph("pending", 0, 0)) {
		t.Fatalf("a pending phase with no expected tasks is not complete")
	}
}

// --- loop wiring ----------------------------------------------------------

// A running multi-phase plan at a fresh non-trivial boundary parks to
// awaitingFeedback(phase_checkpoint) and stamps the idempotency marker.
func TestMaybeCheckpoint_ParksAtBoundary(t *testing.T) {
	planRow := map[string]any{
		"output": []any{map[string]any{
			"id": "p1", "status": "running",
			"phases": []any{
				map[string]any{"status": "done", "expectedTaskCount": 2, "completedTaskCount": 2},
				map[string]any{"status": "pending", "expectedTaskCount": 3, "completedTaskCount": 0},
			},
		}},
	}
	fe := &fakeEngine{execResponder: func(query string) (any, error) {
		if containsAll(query, "queryPlanById") {
			return planRow, nil
		}
		return nil, nil
	}}
	l := &PlannerAgentLoop{engine: fe, logger: slog.New(slog.NewTextHandler(discardWriter{}, nil))}
	handled, err := l.maybeCheckpointAtPhaseBoundary(context.Background(), "p1")
	if err != nil || !handled {
		t.Fatalf("a fresh non-trivial boundary must park: handled=%v err=%v", handled, err)
	}
	exec, _, _ := fe.snapshot()
	if countContains(exec, "phase_checkpoint") != 1 {
		t.Fatalf("must park to awaitingFeedback(phase_checkpoint), got %d", countContains(exec, "phase_checkpoint"))
	}
	if countContains(exec, "lastPhaseCheckpoint") != 1 {
		t.Fatalf("must stamp the idempotency marker lastPhaseCheckpoint, got %d", countContains(exec, "lastPhaseCheckpoint"))
	}
}

// Already parked at this boundary (metrics.lastPhaseCheckpoint == idx) ->
// no re-park.
func TestMaybeCheckpoint_Idempotent(t *testing.T) {
	planRow := map[string]any{
		"output": []any{map[string]any{
			"id": "p1", "status": "running",
			"metrics": map[string]any{"lastPhaseCheckpoint": 1},
			"phases": []any{
				map[string]any{"status": "done", "expectedTaskCount": 2, "completedTaskCount": 2},
				map[string]any{"status": "pending", "expectedTaskCount": 3, "completedTaskCount": 0},
			},
		}},
	}
	fe := &fakeEngine{execResponder: func(query string) (any, error) {
		if containsAll(query, "queryPlanById") {
			return planRow, nil
		}
		return nil, nil
	}}
	l := &PlannerAgentLoop{engine: fe, logger: slog.New(slog.NewTextHandler(discardWriter{}, nil))}
	handled, err := l.maybeCheckpointAtPhaseBoundary(context.Background(), "p1")
	if err != nil || handled {
		t.Fatalf("already-parked boundary must not re-park: handled=%v err=%v", handled, err)
	}
}

// A non-running plan (e.g. already awaitingFeedback) is a no-op.
func TestMaybeCheckpoint_NonRunningNoOp(t *testing.T) {
	planRow := map[string]any{
		"output": []any{map[string]any{
			"id": "p1", "status": "queued",
			"phases": []any{
				map[string]any{"status": "done", "expectedTaskCount": 2, "completedTaskCount": 2},
				map[string]any{"status": "pending", "expectedTaskCount": 3, "completedTaskCount": 0},
			},
		}},
	}
	fe := &fakeEngine{execResponder: func(query string) (any, error) {
		if containsAll(query, "queryPlanById") {
			return planRow, nil
		}
		return nil, nil
	}}
	l := &PlannerAgentLoop{engine: fe, logger: slog.New(slog.NewTextHandler(discardWriter{}, nil))}
	if handled, err := l.maybeCheckpointAtPhaseBoundary(context.Background(), "p1"); err != nil || handled {
		t.Fatalf("a non-running plan must be a no-op: handled=%v err=%v", handled, err)
	}
}
