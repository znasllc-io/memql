package steps

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/automations"
)

// memql#1368 -- execution-level coverage for the authored `parallel` step:
// a struct-form DSL source with a parallel layer is compiled through the
// REAL automation loader (rewriter -> parser -> compiler -> IR) and then
// executed by the real ParallelExecutor. Both branches must run, branch ids
// surface as <parent>.<branch>, and `wait: "all"` semantics hold (the step
// does not complete until the slow branch has).

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
automation gather {
  step layer0 {
    parallel {
      wait: "all"
      branches: [
        step sales   { automation fetchSales { } },
        step support { automation fetchSupport { } }
      ]
    }
  }
}`
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	loader := automations.NewLoader(automations.LoaderOptions{Logger: logger})
	auto, err := loader.CompileSource(src, "test:parallel-exec")
	if err != nil {
		t.Fatalf("authored parallel automation must compile: %v", err)
	}
	if len(auto.Steps) != 1 || auto.Steps[0].Type != automations.StepTypeParallel {
		t.Fatalf("expected one parallel step, got %+v", auto.Steps)
	}

	// Replace the sub-automation executor with a recorder; the `support`
	// branch is slowed down so wait:"all" is observable (the parallel step
	// must not return before the slow branch lands).
	rec := &recordingExecutor{delays: map[string]time.Duration{
		"layer0.support": 50 * time.Millisecond,
	}}
	reg := NewRegistry()
	reg.Register(automations.StepTypeAutomation, rec)
	exec := &ParallelExecutor{Registry: reg}

	res, err := exec.Execute(context.Background(), auto.Steps[0], &Context{
		Evaluator: automations.NewEvaluator(),
	})
	if err != nil {
		t.Fatalf("parallel execution failed: %v", err)
	}
	if res.Status != "success" {
		t.Fatalf("want status success, got %q (error: %s)", res.Status, res.Error)
	}

	// wait:"all" -- BOTH branches must have completed by the time the step
	// result is returned, including the slow one.
	got := rec.executed()
	if len(got) != 2 {
		t.Fatalf("want both branches executed before the step returns, got %v", got)
	}
	seen := map[string]bool{}
	for _, id := range got {
		seen[id] = true
	}
	// Branch ids surface as <parent>.<branch> (the executor's contract).
	if !seen["layer0.sales"] || !seen["layer0.support"] {
		t.Errorf("branch ids must surface as <parent>.<branch>, got %v", got)
	}

	if len(res.Children) != 2 {
		t.Fatalf("want 2 child results, got %d", len(res.Children))
	}
	// Children are reassembled in branch order regardless of completion order.
	if res.Children[0].StepId != "layer0.sales" || res.Children[1].StepId != "layer0.support" {
		t.Errorf("children must be in branch order, got %s / %s",
			res.Children[0].StepId, res.Children[1].StepId)
	}
	for _, child := range res.Children {
		if child.Status != "success" {
			t.Errorf("branch %s must succeed, got %q", child.StepId, child.Status)
		}
	}
}

// A gated branch inside an authored parallel is skip-gated by the executor:
// the branch whose condition is false records status "skipped" and its
// sub-automation never runs, while the sibling still executes.
func TestParallelExecutor_AuthoredDSL_BranchConditionSkips(t *testing.T) {
	const src = `@description("Gated branch probe.")
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
