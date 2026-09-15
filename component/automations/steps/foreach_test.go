package steps

import (
	"context"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/events"
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

// TestForEachExecutor_EvaluatesNestedStepCondition: a gated statement inside
// a `for` decides per item, over the loop's variable -- a false gate runs
// nothing, a true one runs the call.
func TestForEachExecutor_EvaluatesNestedStepCondition(t *testing.T) {
	a, err := automations.NewLoader(automations.LoaderOptions{}).CompileSource(`@trigger(event="probe.fired")
automation loops {
  args {
    items any
  }
  for item in args.items {
    if item == "wanted" {
      builtin doIt()
    }
  }
}`, "test.memql")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	for _, c := range []struct {
		items []any
		calls int
	}{{[]any{"only-item"}, 0}, {[]any{"only-item", "wanted"}, 1}} {
		fake := &fakeExecutor{}
		reg := NewRegistry()
		reg.Register(automations.StepTypeFunction, fake)
		ev := events.NewEvent("probe.fired", events.KindMessage, map[string]any{"items": c.items})
		exec, err := automations.NewExecutor(automations.ExecutorOptions{StepRegistry: reg}).ExecuteWithEvent(context.Background(), a, "test", &ev)
		if err != nil {
			t.Fatalf("run: %v (%s)", err, exec.Error)
		}
		if fake.calls != c.calls {
			t.Fatalf("items %v: the gated call ran %d time(s), want %d", c.items, fake.calls, c.calls)
		}
	}
}
