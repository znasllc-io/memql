package planner

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/harness/parambind"
)

func TestSubGoalText(t *testing.T) {
	if got := subGoalText(plannerTask{Input: map[string]any{"goal": "  book a flight  "}}); got != "book a flight" {
		t.Fatalf("goal: got %q", got)
	}
	// Falls through to the next descriptive key.
	if got := subGoalText(plannerTask{Input: map[string]any{"template": "summarize"}}); got != "summarize" {
		t.Fatalf("template fallback: got %q", got)
	}
	if got := subGoalText(plannerTask{Input: map[string]any{"agentId": "a1"}}); got != "" {
		t.Fatalf("no descriptive field: want empty, got %q", got)
	}
}

func TestCanBindAction(t *testing.T) {
	// Literal-only action (no recorded calls) is trivially bindable.
	if !canBindAction(map[string]any{}, map[string]any{}) {
		t.Fatal("empty payload should be bindable")
	}
	// A step-input binding whose root key is present binds.
	payload := map[string]any{
		"calls": []parambind.Call{{
			Capability: "x",
			Bindings:   []parambind.Binding{{Name: "city", Kind: parambind.SourceStepInput, Ref: "city"}},
		}},
	}
	if !canBindAction(payload, map[string]any{"city": "SF"}) {
		t.Fatal("present ref root should bind")
	}
	// Missing root key -> not bindable (downgrades REUSE to ADAPT).
	if canBindAction(payload, map[string]any{"other": 1}) {
		t.Fatal("missing ref root should not bind")
	}
	// Literal bindings impose no input requirement.
	litOnly := map[string]any{
		"calls": []parambind.Call{{
			Capability: "x",
			Bindings:   []parambind.Binding{{Name: "n", Kind: parambind.SourceLiteral, Literal: 1}},
		}},
	}
	if !canBindAction(litOnly, map[string]any{}) {
		t.Fatal("literal-only bindings should bind")
	}
}

func TestBestRef(t *testing.T) {
	if got := bestRef("v1:actions:action:abc", map[string]any{"version": float64(3)}); got != "v1:actions:action:abc@3" {
		t.Fatalf("pinned: got %q", got)
	}
	if got := bestRef("v1:actions:action:abc", nil); got != "v1:actions:action:abc" {
		t.Fatalf("floating: got %q", got)
	}
	if got := bestRef("v1:actions:action:abc", map[string]any{"version": float64(0)}); got != "v1:actions:action:abc" {
		t.Fatalf("zero version floats: got %q", got)
	}
}

// With the feature flag unset, substitution is a no-op (no engine touched).
func TestMaybeSubstituteDisabledNoop(t *testing.T) {
	t.Setenv("MEMQL_ACTION_REPLAY_ENABLED", "")
	l := &PlannerAgentLoop{}
	in := plannerTask{Kind: "semantic", Input: map[string]any{"goal": "x"}}
	out := l.maybeSubstituteActionStep(context.Background(), in)
	if out.Input["type"] == "action" {
		t.Fatal("disabled flag must not rewrite the task")
	}
}

// An already action-typed task is returned unchanged even when enabled.
func TestMaybeSubstituteSkipsActionTyped(t *testing.T) {
	t.Setenv("MEMQL_ACTION_REPLAY_ENABLED", "1")
	l := &PlannerAgentLoop{}
	in := plannerTask{Input: map[string]any{"type": "action", "ref": "v1:actions:action:abc@1"}}
	out := l.maybeSubstituteActionStep(context.Background(), in)
	if out.Input["ref"] != "v1:actions:action:abc@1" {
		t.Fatalf("action-typed task should pass through unchanged, got %+v", out.Input)
	}
}
