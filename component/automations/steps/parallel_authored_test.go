package steps

import (
	"context"
	"log/slog"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
)

type wrappedBranchOutput struct{}

func (wrappedBranchOutput) Execute(_ context.Context, step *automations.Step, _ *Context) (*automations.StepResult, error) {
	return &automations.StepResult{StepId: step.ID, Status: "success", Result: memql.NewResultWithOutput(map[string]any{"reply": "section content"})}, nil
}

func TestParallelExecutor_ReturnsBranchContentToFollowingSteps(t *testing.T) {
	for _, wait := range []string{"all", "any"} {
		t.Run(wait, func(t *testing.T) {
			reg := NewRegistry()
			reg.Register(automations.StepTypeFunction, wrappedBranchOutput{})
			step := &automations.Step{ID: "sections", Type: automations.StepTypeParallel, Parallel: &automations.ParallelStepConfig{Wait: wait, Branches: []*automations.Step{{ID: "one", Type: automations.StepTypeFunction}}}}
			result, err := (&ParallelExecutor{Registry: reg}).Execute(context.Background(), step, &Context{Evaluator: automations.NewEvaluator()})
			if err != nil {
				t.Fatal(err)
			}
			want := []any{map[string]any{"reply": "section content"}}
			if !reflect.DeepEqual(result.Result, want) {
				t.Fatalf("following step receives opaque engine envelopes instead of section content: %#v", result.Result)
			}
		})
	}
}

// memql#1368 -- execution-level coverage for the authored `parallel`
// statement: a statement body with a parallel is compiled through the REAL
// automation loader (parser -> compiler -> IR) and fired through the real
// executor, whose ParallelExecutor runs the branches. Both branches must run,
// and the wait for all of them holds: the statement after the parallel does
// not start until the slow branch has finished.

// recordingExecutor records each executed step id; an optional per-step
// delay simulates a slow branch.
type recordingExecutor struct {
	mu     sync.Mutex
	ids    []string
	delays map[string]time.Duration
}

func (e *recordingExecutor) Execute(ctx context.Context, step *automations.Step, stepCtx *Context) (*automations.StepResult, error) {
	if d, ok := e.delays[step.ID]; ok {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	e.mu.Lock()
	e.ids = append(e.ids, step.ID)
	e.mu.Unlock()
	return &automations.StepResult{
		StepId:      step.ID,
		Status:      "success",
		StartedAt:   time.Now(),
		CompletedAt: time.Now(),
		Result:      step.ID + ":ok",
	}, nil
}

func (e *recordingExecutor) executed() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.ids))
	copy(out, e.ids)
	return out
}

func TestParallelExecutor_AuthoredDSL_WaitAllRunsBothBranches(t *testing.T) {
	const src = `@description("Gather two reports concurrently.")
@trigger(event="system.startup")
automation gather {
  parallel {
    branch sales {
      automation fetchSales()
    }
    branch support {
      automation fetchSupport()
    }
  }
  automation afterBoth()
}`
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	loader := automations.NewLoader(automations.LoaderOptions{Logger: logger})
	auto, err := loader.CompileSource(src, "test:parallel-exec")
	if err != nil {
		t.Fatalf("authored parallel automation must compile: %v", err)
	}
	if len(auto.Steps) != 2 || auto.Steps[0].Type != automations.StepTypeParallel {
		t.Fatalf("expected the parallel step and the one after it, got %+v", auto.Steps)
	}

	// The sub-automation calls go to a recorder; the `support` branch is
	// slowed down so the wait for all branches is observable.
	rec := &recordingExecutor{delays: map[string]time.Duration{
		"fetchSupport": 50 * time.Millisecond,
	}}
	reg := NewRegistry()
	reg.Register(automations.StepTypeAutomation, rec)
	ev := events.NewEvent("system.startup", events.KindMessage, nil)
	exec, err := automations.NewExecutor(automations.ExecutorOptions{Logger: logger, StepRegistry: reg}).ExecuteWithEvent(context.Background(), auto, "test", &ev)
	if err != nil {
		t.Fatalf("parallel execution failed: %v", err)
	}
	if exec.Status != "completed" {
		t.Fatalf("want status completed, got %q", exec.Status)
	}

	// Both branches ran, and the statement after the parallel ran after both,
	// the slow one included.
	got := rec.executed()
	if len(got) != 3 || got[2] != "afterBoth" {
		t.Fatalf("want both branches, then afterBoth, got %v", got)
	}
	seen := map[string]bool{got[0]: true, got[1]: true}
	if !seen["fetchSales"] || !seen["fetchSupport"] {
		t.Errorf("both branches must run before the statement after them, got %v", got)
	}
}

// A gated branch inside an authored parallel is skip-gated by the executor:
// the branch whose condition is false records status "skipped" and its
// sub-automation never runs, while the sibling still executes.
func TestParallelExecutor_AuthoredDSL_BranchConditionSkips(t *testing.T) {
	const src = `@description("Gated branch probe.")
@trigger(event="system.startup")
automation gather {
  step layer0 {
    parallel {
      branches: [
        step a { if steps.prep.status == "success" { automation fetchA { } } },
        step b { automation fetchB { } }
      ]
    }
  }
}`
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	loader := automations.NewLoader(automations.LoaderOptions{Logger: logger})
	auto, err := loader.CompileSource(src, "test:parallel-branch-gate-exec")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	rec := &recordingExecutor{}
	reg := NewRegistry()
	reg.Register(automations.StepTypeAutomation, rec)
	exec := &ParallelExecutor{Registry: reg}

	// prep recorded as FAILED -> branch a's gate is false -> skipped.
	eval := automations.NewEvaluator()
	eval.SetStepResult("prep", &automations.StepResult{StepId: "prep", Status: "failed"})

	res, err := exec.Execute(context.Background(), auto.Steps[0], &Context{Evaluator: eval})
	if err != nil {
		t.Fatalf("parallel execution failed: %v", err)
	}
	got := rec.executed()
	if len(got) != 1 || got[0] != "layer0.b" {
		t.Fatalf("only the ungated branch must run, got %v", got)
	}
	if len(res.Children) != 2 {
		t.Fatalf("want 2 child results, got %d", len(res.Children))
	}
	statuses := map[string]string{}
	for _, child := range res.Children {
		statuses[child.StepId] = child.Status
	}
	if statuses["layer0.a"] != "skipped" {
		t.Errorf("gated branch must be skipped, got %q", statuses["layer0.a"])
	}
	if statuses["layer0.b"] != "success" {
		t.Errorf("ungated branch must succeed, got %q", statuses["layer0.b"])
	}
}
