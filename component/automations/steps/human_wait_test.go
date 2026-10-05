package steps

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/work"
)

type humanWaitProbe struct{ calls atomic.Int32 }

func (p *humanWaitProbe) Execute(_ context.Context, step *automations.Step, _ *Context) (*automations.StepResult, error) {
	p.calls.Add(1)
	return &automations.StepResult{StepId: step.ID, Status: "failed", CompletedAt: time.Now()}, fmt.Errorf("wrapped: %w", &work.HumanWait{ApprovalID: "q"})
}

func TestHumanQuestionStopsAContinueOnErrorLoop(t *testing.T) {
	a, err := automations.NewLoader(automations.LoaderOptions{}).CompileSource(`automation questionLoop {
  for item in [1, 2, 3] {
    builtin askQuestion()
  } on error continue
  builtin laterEffect()
}`, "test.memql")
	if err != nil {
		t.Fatal(err)
	}
	probe := &humanWaitProbe{}
	reg := NewRegistry()
	reg.Register(automations.StepTypeFunction, probe)
	executor := automations.NewExecutor(automations.ExecutorOptions{StepRegistry: reg})
	defer executor.Close()
	run, err := executor.Execute(context.Background(), a, "test")
	if !isHumanWait(err) || run.Status != "waiting" || probe.calls.Load() != 1 {
		t.Fatalf("loop swallowed suspension: %+v %v, calls=%d", run, err, probe.calls.Load())
	}
}

func TestHumanQuestionSurvivesParallelWaitStrategiesAndErrorPolicies(t *testing.T) {
	for _, wait := range []string{"all", "any"} {
		for _, failFast := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%v", wait, failFast), func(t *testing.T) {
				reg := NewRegistry()
				reg.Register(automations.StepTypeFunction, &humanWaitProbe{})
				step := &automations.Step{ID: "parallel", Type: automations.StepTypeParallel, Parallel: &automations.ParallelStepConfig{Wait: wait, FailFast: failFast, Branches: []*automations.Step{{ID: "question", Type: automations.StepTypeFunction}}}}
				result, err := (&ParallelExecutor{Registry: reg}).Execute(context.Background(), step, &Context{Evaluator: automations.NewEvaluator()})
				if !isHumanWait(err) || result.Status != "waiting" {
					t.Fatalf("parallel swallowed suspension: %+v %v", result, err)
				}
			})
		}
	}
}

func TestParallelSuccessCannotHideAConcurrentlyPersistedQuestion(t *testing.T) {
	questionStarted := make(chan struct{})
	executor := &ParallelExecutor{Dispatch: func(ctx context.Context, step *automations.Step, _ *Context) (*automations.StepResult, error) {
		if step.ID == "parallel.question" {
			close(questionStarted)
			// Model a question saved just before another branch finishes. Its
			// receipt must reach the parent even after cancellation arrives.
			<-ctx.Done()
			return nil, &work.HumanWait{ApprovalID: "persisted"}
		}
		<-questionStarted
		return &automations.StepResult{StepId: step.ID, Status: "success", Result: "done"}, nil
	}}
	step := &automations.Step{ID: "parallel", Type: automations.StepTypeParallel, Parallel: &automations.ParallelStepConfig{Wait: "any", Branches: []*automations.Step{{ID: "question", Type: automations.StepTypeFunction}, {ID: "success", Type: automations.StepTypeFunction}}}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := executor.Execute(ctx, step, &Context{Evaluator: automations.NewEvaluator()})
	if !isHumanWait(err) || result.Status != "waiting" {
		t.Fatalf("success hid an already persisted question: %+v %v", result, err)
	}
}
