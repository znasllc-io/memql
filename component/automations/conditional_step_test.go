package automations

import (
	"log/slog"
	"os"
	"strings"
	"testing"
)

// memql#1366 -- conditional steps in the struct-form automation grammar.
//
// The phased-authoring headline gates each layer on the prior layer's
// success: `step merge { if steps.fetchA.status == "success" { automation
// merge { } } }`. These tests lock the full chain: struct form -> rewriter
// -> procedural parser -> Automation.Steps[].Condition, and the runtime
// evaluator actually resolving the `steps.<id>.<field>` reference inside a
// condition (it used to fall through to a LITERAL string, making the
// comparison constant).

func TestCompileSource_ConditionalStep(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	loader := NewLoader(LoaderOptions{Logger: logger})

	const src = `@description("Gather then merge.")
automation gather {
  step fetchA {
    automation fetchA { }
  }
  step fetchB {
    automation fetchB { }
  }
  step merge {
    if steps.fetchA.status == "success" && steps.fetchB.status == "success" {
      automation merge { }
    }
  }
}`
	auto, err := loader.CompileSource(src, "test:conditional-step")
	if err != nil {
		t.Fatalf("conditional-step automation must compile: %v", err)
	}
	if len(auto.Steps) != 3 {
		t.Fatalf("want 3 steps, got %d", len(auto.Steps))
	}
	if auto.Steps[0].Condition != "" || auto.Steps[1].Condition != "" {
		t.Errorf("layer-0 steps must be ungated, got %q / %q",
			auto.Steps[0].Condition, auto.Steps[1].Condition)
	}
	cond := auto.Steps[2].Condition
	if cond == "" {
		t.Fatalf("gated step must carry its condition; steps: %+v", auto.Steps)
	}
	// The condition rides through token-for-token (modulo spacing).
	for _, want := range []string{`steps.fetchA.status`, `steps.fetchB.status`, `"success"`, "&&"} {
		if !strings.Contains(cond, want) {
			t.Errorf("condition %q missing %q", cond, want)
		}
	}
}

// The runtime half: a `steps.<id>.<field>` reference inside a condition must
// resolve against the recorded step results. Before memql#1366 the filter
// resolver only tried the `event.` prefix and fell back to the literal path
// string, so `steps.x.status == "success"` was constant-false and a gated
// layer either never ran or (truthy form) always ran.
func TestEvaluateCondition_StepStatusReference(t *testing.T) {
	e := NewEvaluator()
	e.SetStepResult("fetchA", &StepResult{StepId: "fetchA", Status: "success"})
	e.SetStepResult("fetchB", &StepResult{StepId: "fetchB", Status: "failed"})
	e.SetStepResult("fetchC", &StepResult{StepId: "fetchC", Status: "skipped"})

	cases := []struct {
		cond string
		want bool
	}{
		{`steps.fetchA.status == "success"`, true},
		{`steps.fetchB.status == "success"`, false},
		{`steps.fetchC.status == "success"`, false}, // skipped cascades the skip
		{`steps.fetchA.status == "success" && steps.fetchB.status == "success"`, false},
		{`steps.fetchA.status == "success" && steps.fetchA.status != "skipped"`, true},
	}
	for _, tc := range cases {
		got, err := e.EvaluateCondition(tc.cond)
		if err != nil {
			t.Errorf("EvaluateCondition(%q) errored: %v", tc.cond, err)
			continue
		}
		if got != tc.want {
			t.Errorf("EvaluateCondition(%q) = %v, want %v", tc.cond, got, tc.want)
		}
	}
}

// A path that merely starts with "steps." but names no recorded step keeps
// the prior literal fall-through (no behavior change for non-step literals).
func TestEvaluateFilterValue_UnknownStepKeepsLiteralFallthrough(t *testing.T) {
	e := NewEvaluator()
	val, err := e.EvaluateFilterValue("steps.nosuch.status")
	if err != nil {
		t.Fatalf("EvaluateFilterValue: %v", err)
	}
	if val != "steps.nosuch.status" {
		t.Fatalf("unknown step reference must keep the literal fall-through, got %v", val)
	}
}
