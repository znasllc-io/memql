package training

import (
	"testing"

	"github.com/znasllc-io/memql/core/id"
)

// TestTrainingPlanIdsAreValid is the training-side guard for issue
// #1712: every plan/task id minted by the training pipeline's system
// automations must pass the engine's shortId validator. Each case
// mirrors how a live call site builds its id, so reintroducing a
// colon-concatenated id fails this test.
//
// Audited system minting sites routed through the central generator:
//   - plan_lifecycle.go  beginPlan   ("train", agentId) + 3 derived task ids
//   - retry.go            scheduleRetry ("trainretry", agentId, step)
func TestTrainingPlanIdsAreValid(t *testing.T) {
	const (
		planConcept = "v1:planner:plan"
		taskConcept = "v1:planner:task"
	)
	agentId := "v1:agents:agent:d6ac657f-1234-5678-9abc-def012345678"

	planId := id.NewSystemNodeShortId("train", agentId)
	if err := id.ValidateShortId(planConcept, planId); err != nil {
		t.Fatalf("training plan id %q invalid: %v", planId, err)
	}

	for _, suffix := range []string{"t0-capabilities", "t1-identityVector", "t2-distillPrompt"} {
		taskId := id.SystemNodeShortId(planId, suffix)
		if err := id.ValidateShortId(taskConcept, taskId); err != nil {
			t.Fatalf("training task id %q invalid: %v", taskId, err)
		}
	}

	retryId := id.NewSystemNodeShortId("trainretry", agentId, "capabilities")
	if err := id.ValidateShortId(planConcept, retryId); err != nil {
		t.Fatalf("training retry plan id %q invalid: %v", retryId, err)
	}
}
