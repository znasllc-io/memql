package automations

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/core/common"
)

func TestCheckpointAuthorityCannotLeakIntoAnotherStep(t *testing.T) {
	e := NewExecutor(ExecutorOptions{})
	defer e.Close()
	ctx := common.ContextWithRun(context.Background(), common.RunContext{RunId: "run", Continuation: &common.StepContinuation{StepKey: "question", ObservationID: "checkpoint", ApprovalID: "answered"}})
	sc := &StepContext{Execution: &AutomationExecution{ID: "run"}}
	current, _ := common.RunFromContext(e.withRunContext(ctx, sc, &Step{ID: "question"}))
	if current.Continuation == nil {
		t.Fatal("the suspended step lost its checkpoint requirement")
	}
	for _, step := range []string{"publish", "question/child"} {
		next, _ := common.RunFromContext(e.withRunContext(ctx, sc, &Step{ID: step}))
		if next.Continuation != nil {
			t.Fatalf("%s inherited another step's continuation", step)
		}
	}
}
