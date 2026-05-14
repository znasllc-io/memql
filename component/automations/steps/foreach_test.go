package steps

import (
	"context"
	"testing"
	"time"

	"github.com/visionarys-io/memql/component/automations"
)

type fakeExecutor struct {
	calls int
}

func (e *fakeExecutor) Execute(ctx context.Context, step *automations.Step, stepCtx *Context) (*automations.StepResult, error) {
	e.calls++
	return &automations.StepResult{
		StepId:      step.ID,
		Status:      "success",
		StartedAt:   time.Now(),
		CompletedAt: time.Now(),
		Result:      "ok",
	}, nil
}

func TestForEachExecutor_EvaluatesNestedStepCondition(t *testing.T) {
	reg := NewRegistry()
	fake := &fakeExecutor{}
	reg.Register(automations.StepType("fake"), fake)

	exec := &ForEachExecutor{Registry: reg}

	eval := automations.NewEvaluator()
	eval.SetInput([]any{"only-item"})

	step := &automations.Step{
		ID:   "forEach_test",
		Type: automations.StepTypeForEach,
		ForEach: &automations.ForEachStepConfig{
			Source: "$input",
			As:     "item",
			Do: []*automations.Step{
				{
					ID:        "doIt",
					Type:      automations.StepType("fake"),
					Condition: "$index == 1", // index will be 0 for the only item -> should skip
				},
			},
		},
	}

	res, err := exec.Execute(context.Background(), step, &Context{
		Evaluator: eval,
	})
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if fake.calls != 0 {
		t.Fatalf("expected fake executor not to be called, got calls=%d", fake.calls)
	}

	// Verify nested step result is marked skipped.
	if res == nil || len(res.Children) != 1 {
		t.Fatalf("expected 1 iteration child result, got: %#v", res)
	}
	iter := res.Children[0]
	if iter == nil || len(iter.Children) != 1 {
		t.Fatalf("expected 1 nested child result, got: %#v", iter)
	}
	nested := iter.Children[0]
	if nested.Status != "skipped" {
		t.Fatalf("expected nested step to be skipped, got status=%q", nested.Status)
	}
}
