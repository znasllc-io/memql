package automations

import (
	"strings"
	"testing"
)

// memql#2246 -- the `forEach` step in the struct-form automation grammar.
//
// These tests lock the full chain: an authored `for` statement -> the
// statement parser -> the statement compiler -> Automation.Steps[].ForEach IR
// (ForEachStepConfig{Source, As, Do}), which the ForEachExecutor runs once
// per item. This is the author-surface that moved the per-row sweep logics
// (#2235) into automation steps.

const forEachAuthoredSrc = `@description("Prune stale nodes one row at a time.")
@trigger(event="system.startup")
automation pruneStaleNodes {
  decide := automation findStaleNodes()
  for node in decide {
    automation retireNode(id: node.id)
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
	// The collection expression survives, the iteration variable is its
	// author's, and the loop carries exactly one Do child.
	if loop.ForEach.Source != "decide" {
		t.Errorf("forEach source mismatch, got %q", loop.ForEach.Source)
	}
	if loop.ForEach.As != "node" {
		t.Errorf("the loop variable must keep its author's name, `node`, got %q", loop.ForEach.As)
	}
	if len(loop.ForEach.Do) != 1 {
		t.Fatalf("want 1 Do child, got %d", len(loop.ForEach.Do))
	}
	do := loop.ForEach.Do[0]
	if do.ID != "retireNode" {
		t.Errorf("an unnamed call's id is its callee, got %q", do.ID)
	}
	if do.Type != StepTypeAutomation || do.Automation == nil || do.Automation.Name != "retireNode" {
		t.Errorf("Do child must dispatch the inner call, got type=%s automation=%+v", do.Type, do.Automation)
	}
}

func TestCompileSource_ForEachStep_Diagnostics(t *testing.T) {
	src := "@description(\"bad\")\n@trigger(event=\"system.startup\")\nautomation bad {\n  decide := automation findStaleNodes()\n  for node decide {\n    automation retireNode(id: node.id)\n  }\n}"
	_, err := newTestLoader().CompileSource(src, "test:foreach-bad")
	if err == nil || !strings.Contains(err.Error(), "expected `in` after the loop variable") {
		t.Fatalf("a `for` without `in` must be refused naming it, got %v", err)
	}
}
