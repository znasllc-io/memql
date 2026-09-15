package steps

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/events"
)

// memql#2246 -- execution-level coverage for the authored `for` loop: a
// statement body with a loop is compiled through the REAL automation loader
// (parser -> compiler -> IR) and fired through the real executor, whose
// ForEachExecutor runs the loop. The inner call must run once per item, with
// the per-item value bound to the loop's variable, proving the author surface
// lowers onto the runtime StepTypeForEach contract.

func TestForEachExecutor_AuthoredDSL_RunsInnerCallPerItem(t *testing.T) {
	const src = `@description("Retire each stale node.")
@trigger(event="system.startup")
automation pruneStaleNodes {
  decide := logic staleNodes()
  for node in decide.nodes() {
    automation retireNode(id: node.id)
  }
}`
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	loader := automations.NewLoader(automations.LoaderOptions{Logger: logger})
	auto, err := loader.CompileSource(src, "test:foreach-exec")
	if err != nil {
		t.Fatalf("authored for loop must compile: %v", err)
	}
	if len(auto.Steps) != 2 || auto.Steps[1].Type != automations.StepTypeForEach {
		t.Fatalf("expected the decide step and one forEach step, got %+v", auto.Steps)
	}
	if loop := auto.Steps[1]; loop.ForEach == nil || loop.ForEach.As != "node" {
		t.Fatalf("the loop must bind its author's variable, `node`, got %+v", loop.ForEach)
	}

	// `staleNodes` answers three rows; the inner call dispatches as a
	// sub-automation step, which a recorder stands in for.
	rows := []any{
		map[string]any{"id": "n1", "payload": map[string]any{}},
		map[string]any{"id": "n2", "payload": map[string]any{}},
		map[string]any{"id": "n3", "payload": map[string]any{}},
	}
	rec := &recordingExecutor{}
	reg := NewRegistry()
	reg.Register(automations.StepTypeFunction, &argRecorder{answers: map[string]any{"staleNodes": stepResultFor("query", rows)}})
	reg.Register(automations.StepTypeAutomation, rec)
	ev := events.NewEvent("system.startup", events.KindMessage, nil)
	exec, err := automations.NewExecutor(automations.ExecutorOptions{Logger: logger, StepRegistry: reg}).ExecuteWithEvent(context.Background(), auto, "test", &ev)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if exec.Status != "completed" {
		t.Fatalf("want status completed, got %q", exec.Status)
	}

	// The inner sub-automation must have run exactly once per item (3 times).
	got := rec.executed()
	if len(got) != 3 {
		t.Fatalf("inner call must run once per item (3), got %d: %v", len(got), got)
	}
}
