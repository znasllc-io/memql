package app

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/harness"
)

// TestDispatchActionStepMissingRef: an action-typed step with no ref is a
// hard error before the engine is ever touched (so it needs no DB).
func TestDispatchActionStepMissingRef(t *testing.T) {
	a := &App{}
	_, calls, err := a.dispatchActionStep(context.Background(), harness.StepView{
		ID:    "step-1",
		Input: map[string]any{"type": "action"},
	})
	if err == nil || !strings.Contains(err.Error(), "missing ref") {
		t.Fatalf("want missing-ref error, got %v", err)
	}
	if calls != 0 {
		t.Fatalf("replay must report 0 tool calls, got %d", calls)
	}
}

// TestDispatchActionStepNoEngine: a well-formed ref with no engine wired
// surfaces the executor's "engine not configured" failure, wrapped with the
// step id + ref for operability. Confirms the action step routes into the
// merged ActionExecutor rather than the LLM path.
func TestDispatchActionStepNoEngine(t *testing.T) {
	a := &App{}
	_, calls, err := a.dispatchActionStep(context.Background(), harness.StepView{
		ID: "step-2",
		Input: map[string]any{
			"type": "action",
			"ref":  "v1:actions:action:abc@3",
			"args": map[string]any{"city": "SF"},
		},
	})
	if err == nil {
		t.Fatal("want engine-not-configured error, got nil")
	}
	if !strings.Contains(err.Error(), "step-2") || !strings.Contains(err.Error(), "abc@3") {
		t.Fatalf("error should name the step + ref, got %v", err)
	}
	if calls != 0 {
		t.Fatalf("replay must report 0 tool calls, got %d", calls)
	}
}
