package automations

import (
	"strings"
	"testing"
)

// memql#2246 -- the `forEach` step in the struct-form automation grammar.
//
// These tests lock the full chain: authored struct form -> rewriter ->
// procedural parser -> compiler -> Automation.Steps[].ForEach IR
// (ForEachStepConfig{Source, As, Do}), which the ForEachExecutor already
// runs once per item. This is the author-surface that unblocks moving the
// per-row sweep logics (#2235) into automation steps.

const forEachAuthoredSrc = `@description("Prune stale nodes one row at a time.")
automation pruneStaleNodes {
  step decide {
    automation findStaleNodes { }
  }
  step prune {
    forEach node in decide.result {
      automation retireNode { id: node.id }
    }
  }
}`

func TestCompileSource_ForEachStep(t *testing.T) {
	auto, err := newTestLoader().CompileSource(forEachAuthoredSrc, "test:foreach-step")
	if err != nil {
		t.Fatalf("forEach automation must compile: %v", err)
	}
	if len(auto.Steps) != 2 {
		t.Fatalf("want 2 steps, got %d", len(auto.Steps))
	}

	loop := auto.Steps[1]
	if loop.Type != StepTypeForEach {
		t.Fatalf("step 1 must be the forEach loop, got %s/%s", loop.ID, loop.Type)
	}
	if loop.ForEach == nil {
		t.Fatal("forEach step must carry the ForEach config")
	}
	// The collection expression survives, the iteration variable is the
	// canonical `item`, and the loop carries exactly one Do child.
	if loop.ForEach.Source != "decide.result" {
		t.Errorf("forEach source mismatch, got %q", loop.ForEach.Source)
	}
	if loop.ForEach.As != "item" {
		t.Errorf("forEach iteration var must be canonicalised to `item`, got %q", loop.ForEach.As)
	}
	if len(loop.ForEach.Do) != 1 {
		t.Fatalf("want 1 Do child, got %d", len(loop.ForEach.Do))
	}
	do := loop.ForEach.Do[0]
	if do.ID != "prune_do1" {
		t.Errorf("Do child must keep its synthesized name, got %q", do.ID)
	}
	if do.Type != StepTypeAutomation || do.Automation == nil || do.Automation.Name != "retireNode" {
		t.Errorf("Do child must dispatch the inner call, got type=%s automation=%+v", do.Type, do.Automation)
	}
}

func TestCompileSource_ForEachStep_Diagnostics(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			name:    "missing in keyword",
			body:    `forEach node decide.result { automation y { } }`,
			wantErr: "expected `in`",
		},
		{
			name:    "empty body",
			body:    `forEach node in decide.result { }`,
			wantErr: "the loop body is empty",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := "@description(\"bad\")\nautomation bad {\n  step s {\n    " + tc.body + "\n  }\n}"
			_, err := newTestLoader().CompileSource(src, "test:foreach-bad")
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}
