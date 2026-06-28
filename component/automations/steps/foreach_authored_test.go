package steps

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"github.com/znasllc-io/memql/component/automations"
)

// memql#2246 -- execution-level coverage for the authored `forEach` step:
// a struct-form DSL source with a forEach loop is compiled through the REAL
// automation loader (rewriter -> parser -> compiler -> IR) and then executed
// by the real ForEachExecutor. The inner call must run once per item, with
// the per-item value bound to the canonical `item` variable, proving the
// author surface lowers onto the runtime StepTypeForEach contract.

func TestForEachExecutor_AuthoredDSL_RunsInnerCallPerItem(t *testing.T) {
	const src = `@description("Retire each stale node.")
automation pruneStaleNodes {
  step prune {
    forEach node in decide.result {
      automation retireNode { id: node.id }
    }
  }
}`
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	loader := automations.NewLoader(automations.LoaderOptions{Logger: logger})
	auto, err := loader.CompileSource(src, "test:foreach-exec")
	if err != nil {
		t.Fatalf("authored forEach automation must compile: %v", err)
	}
	if len(auto.Steps) != 1 || auto.Steps[0].Type != automations.StepTypeForEach {
		t.Fatalf("expected one forEach step, got %+v", auto.Steps)
	}
	loop := auto.Steps[0]
	if loop.ForEach == nil || loop.ForEach.As != "item" {
		t.Fatalf("forEach config must bind the canonical `item` var, got %+v", loop.ForEach)
	}

	// The inner call dispatches as a sub-automation step; replace that
	// executor with a recorder.
	rec := &recordingExecutor{}
	reg := NewRegistry()
	reg.Register(automations.StepTypeAutomation, rec)
	exec := &ForEachExecutor{Registry: reg}

	// Seed the `decide` step result with a three-item collection that the
	// loop's `decide.result` source resolves against.
	eval := automations.NewEvaluator()
	eval.SetStepResult("decide", &automations.StepResult{
		StepId: "decide",
		Status: "success",
		Result: []any{
			map[string]any{"id": "n1"},
			map[string]any{"id": "n2"},
			map[string]any{"id": "n3"},
		},
	})

	res, err := exec.Execute(context.Background(), loop, &Context{Evaluator: eval})
	if err != nil {
		t.Fatalf("forEach execution failed: %v", err)
	}
	if res.Status != "success" {
		t.Fatalf("want status success, got %q (error: %s)", res.Status, res.Error)
	}

	// The inner sub-automation must have run exactly once per item (3 times).
	got := rec.executed()
	if len(got) != 3 {
		t.Fatalf("inner call must run once per item (3), got %d: %v", len(got), got)
	}
}
