package planner

import (
	"context"
	"log/slog"
	"testing"
)

// --- pure gate decision ---------------------------------------------------

func TestEvaluateSpecialistGate_TrainingAlwaysGated(t *testing.T) {
	// spawnTrainingPlan is gated regardless of plan kind.
	for _, kind := range []string{"produceArtifact", "userGoal", "adHocAction", "somethingElse"} {
		plan := map[string]any{"kind": kind}
		if r := evaluateSpecialistGate(plan, "spawnTrainingPlan", true); !r.Blocked {
			t.Fatalf("spawnTrainingPlan must be gated for kind=%s", kind)
		}
	}
}

func TestEvaluateSpecialistGate_CreateGatedForOneOff(t *testing.T) {
	// createSpecialist / extendSpecialist gated for one-off kinds...
	for _, kind := range []string{"produceArtifact", "adHocAction"} {
		plan := map[string]any{"kind": kind}
		for _, action := range []string{"createSpecialist", "extendSpecialist"} {
			if r := evaluateSpecialistGate(plan, action, true); !r.Blocked {
				t.Fatalf("%s must be gated for one-off kind=%s (acceptance: birds list triggers no creation)", action, kind)
			}
		}
	}
	// ...but allowed for genuine multi-step plans.
	plan := map[string]any{"kind": "userGoal"}
	if r := evaluateSpecialistGate(plan, "createSpecialist", true); r.Blocked {
		t.Fatalf("createSpecialist must be allowed for a non-one-off plan kind")
	}
}

func TestEvaluateSpecialistGate_ApprovedBypasses(t *testing.T) {
	// metrics.specialistApproved bypasses the gate.
	approved := map[string]any{"kind": "produceArtifact", "metrics": map[string]any{"specialistApproved": true}}
	if r := evaluateSpecialistGate(approved, "spawnTrainingPlan", true); r.Blocked {
		t.Fatalf("an approved plan must bypass the training gate")
	}
	// tokenCapDisabled escape hatch bypasses too.
	capOff := map[string]any{"kind": "produceArtifact", "tokenCapDisabled": true}
	if r := evaluateSpecialistGate(capOff, "createSpecialist", true); r.Blocked {
		t.Fatalf("tokenCapDisabled must bypass the specialist gate")
	}
}

func TestEvaluateSpecialistGate_DisabledNeverBlocks(t *testing.T) {
	plan := map[string]any{"kind": "produceArtifact"}
	if r := evaluateSpecialistGate(plan, "spawnTrainingPlan", false); r.Blocked {
		t.Fatalf("a disabled gate must never block")
	}
}

// --- loop wiring: a one-off plan parks instead of running the factory -----

// TestGateSpecialistAction_OneOffParks is the acceptance shape: a
// produceArtifact plan that reaches createSpecialist parks for approval
// (mutationUpdatePlanStatus -> awaitingFeedback) and does NOT run the
// ensureAgentForGoal factory.
func TestGateSpecialistAction_OneOffParks(t *testing.T) {
	planRow := map[string]any{
		"output": []any{map[string]any{
			"id": "plan-1", "kind": "produceArtifact", "status": "planning", "goal": "a list of 10 birds",
		}},
	}
	fe := &fakeEngine{
		execResponder: func(query string) (any, error) {
			if containsAll(query, "queryPlanById") {
				return planRow, nil
			}
			return nil, nil
		},
	}
	l := &PlannerAgentLoop{engine: fe, logger: slog.New(slog.NewTextHandler(discardWriter{}, nil))}

	handled, err := l.gateSpecialistAction(context.Background(), "plan-1", "createSpecialist")
	if err != nil || !handled {
		t.Fatalf("one-off createSpecialist must be handled (parked): handled=%v err=%v", handled, err)
	}
	exec, _, _ := fe.snapshot()
	if countContains(exec, "ensureAgentForGoal") != 0 {
		t.Fatalf("must NOT run the factory for a gated one-off plan, got %d ensureAgentForGoal calls", countContains(exec, "ensureAgentForGoal"))
	}
	if countContains(exec, "specialist_approval_required") != 1 {
		t.Fatalf("must park to awaitingFeedback(specialist_approval_required), got %d", countContains(exec, "specialist_approval_required"))
	}
}

func TestGateSpecialistAction_MultiStepProceeds(t *testing.T) {
	planRow := map[string]any{
		"output": []any{map[string]any{"id": "plan-2", "kind": "userGoal", "status": "planning"}},
	}
	fe := &fakeEngine{execResponder: func(query string) (any, error) {
		if containsAll(query, "queryPlanById") {
			return planRow, nil
		}
		return nil, nil
	}}
	l := &PlannerAgentLoop{engine: fe, logger: slog.New(slog.NewTextHandler(discardWriter{}, nil))}
	handled, err := l.gateSpecialistAction(context.Background(), "plan-2", "createSpecialist")
	if err != nil || handled {
		t.Fatalf("a multi-step plan's createSpecialist must NOT be gated: handled=%v err=%v", handled, err)
	}
}
