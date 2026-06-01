package steps

import (
	"testing"

	"github.com/znasllc-io/memql/component/automations"
)

// These tests pin the memql#574 fix: a BARE step identifier passed to a
// selector builtin (e.g. `coalesce(getActiveGA, getFallbackGA)`) must resolve
// to the step's result, not render as its own name. The autoJoinSI logic
// builds the GA participant id via
//
//	getGA := coalesce(getActiveGA, getFallbackGA)
//	... agentId: coalesce(getGA.First().id, "")
//
// Before the fix, `getActiveGA` fell through to EvaluateString and resolved to
// the literal string "getActiveGA", so coalesce returned that string, getGA had
// no navigable nodes, and the unevaluated `coalesce(getGA.First().id, "")`
// literal flowed into the mutation as the agentId -- a ghost SI on every fresh
// daily space.

func bundleResult(nodes ...any) any {
	return map[string]any{"Bundle": map[string]any{"nodes": nodes}}
}

func agentRow(id, name string) any {
	return map[string]any{"id": id, "payload": map[string]any{"name": name}}
}

// resolveAutoJoinGAId reproduces the autoJoinSI two-step id derivation against a
// seeded evaluator and returns whatever agentId the mutation would receive.
func resolveAutoJoinGAId(t *testing.T, active, fallback *automations.StepResult) any {
	t.Helper()
	eval := automations.NewEvaluator()
	eval.SetStepResult("getActiveGA", active)
	eval.SetStepResult("getFallbackGA", fallback)
	exec := &MutationExecutor{}

	ga, err := exec.evaluateValue(eval, "coalesce(getActiveGA, getFallbackGA)")
	if err != nil {
		t.Fatalf("evaluating getGA: %v", err)
	}
	eval.SetStepResult("getGA", &automations.StepResult{StepId: "getGA", Status: "success", Result: ga})

	id, err := exec.evaluateValue(eval, `coalesce(getGA.First().id, "")`)
	if err != nil {
		t.Fatalf("evaluating agentId: %v", err)
	}
	return id
}

func TestBareStepIdentifier_AutoJoinSI_ActiveAgent(t *testing.T) {
	active := &automations.StepResult{StepId: "getActiveGA", Status: "success", Result: bundleResult(agentRow("v1:agents:agent:A", "Sofia"))}
	fallback := &automations.StepResult{StepId: "getFallbackGA", Status: "skipped"} // not fired, Result nil

	got := resolveAutoJoinGAId(t, active, fallback)
	if got != "v1:agents:agent:A" {
		t.Fatalf("agentId = %#v, want the active GA id (not a coalesce literal)", got)
	}
}

func TestBareStepIdentifier_AutoJoinSI_FallbackAgent(t *testing.T) {
	// activeAssistantId empty -> getActiveGA skipped, getFallbackGA fires.
	active := &automations.StepResult{StepId: "getActiveGA", Status: "skipped"} // Result nil
	fallback := &automations.StepResult{StepId: "getFallbackGA", Status: "success", Result: bundleResult(agentRow("v1:agents:agent:B", "Sofia"))}

	got := resolveAutoJoinGAId(t, active, fallback)
	if got != "v1:agents:agent:B" {
		t.Fatalf("agentId = %#v, want the fallback GA id", got)
	}
}

func TestBareStepIdentifier_AutoJoinSI_NoAgent(t *testing.T) {
	// The fired query returned zero rows and the other branch was skipped: there
	// is genuinely no GA. The id must resolve to nil/"" so the `!getGA.Empty()`
	// guard suppresses the join -- NOT to a literal expression.
	active := &automations.StepResult{StepId: "getActiveGA", Status: "success", Result: bundleResult()} // empty rowset
	fallback := &automations.StepResult{StepId: "getFallbackGA", Status: "skipped"}

	got := resolveAutoJoinGAId(t, active, fallback)
	if got != nil && got != "" {
		t.Fatalf("agentId = %#v, want nil/empty when no GA exists", got)
	}
}

func TestBareStepIdentifier_ResolvesToStepResult(t *testing.T) {
	eval := automations.NewEvaluator()
	eval.SetStepResult("rows", &automations.StepResult{StepId: "rows", Status: "success", Result: bundleResult(agentRow("v1:agents:agent:C", "Sofia"))})
	exec := &MutationExecutor{}

	got, err := exec.evaluateValue(eval, "rows")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m, ok := got.(map[string]any)
	if !ok || m["Bundle"] == nil {
		t.Fatalf("bare step id should resolve to the step result bundle, got %#v", got)
	}
}

func TestBareIdentifier_NonStep_StaysLiteral(t *testing.T) {
	// Regression guard: a bare identifier that is NOT a known step must keep its
	// prior behaviour (render as a literal string) so existing
	// coalesce(field, default) / cond(...) usages are unaffected.
	eval := automations.NewEvaluator()
	exec := &MutationExecutor{}

	got, err := exec.evaluateValue(eval, "writer")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "writer" {
		t.Fatalf("non-step bare identifier = %#v, want literal %q", got, "writer")
	}
}
