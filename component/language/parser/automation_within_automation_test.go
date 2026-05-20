package parser

import (
	"strings"
	"testing"
)

// TestAutomationWithinAutomation_StepKind verifies the parser
// recognises `automation NAME { args }` as a step body kind and
// rewrites it to `automationNAME({...})` so the compiler can
// then convert it to a StepTypeAutomation.
func TestAutomationWithinAutomation_StepKind(t *testing.T) {
	source := `@trigger(event="graph.node.created.v1:cognition:space")
automation onSpaceCreated {
  step welcome {
    automation seedWelcomeCurriculum { spaceId: event.payload.id, userId: event.payload.actor }
  }
}`

	rewritten, err := NormaliseAutomationSource(source)
	if err != nil {
		t.Fatalf("NormaliseAutomationSource: %v", err)
	}

	// The rewriter should emit automationSeedWelcomeCurriculum as a
	// function-call expression. event.X references pass through
	// verbatim -- the runtime function-step args resolver resolves
	// them against the evaluator's `event` binding. (Earlier versions
	// translated to ctx.input.X, but ctx.input was never a recognised
	// runtime reference; see memql-cockpit#49.)
	if !strings.Contains(rewritten, "automationSeedWelcomeCurriculum(") {
		t.Errorf("expected rewritten source to contain automationSeedWelcomeCurriculum( call, got:\n%s", rewritten)
	}
	if !strings.Contains(rewritten, "event.payload.id") {
		t.Errorf("expected event.payload.id to ride through verbatim, got:\n%s", rewritten)
	}
	if strings.Contains(rewritten, "ctx.input") {
		t.Errorf("rewriter must not emit ctx.input references, got:\n%s", rewritten)
	}
}

// TestAutomationWithinAutomation_BareCall verifies a no-args call
// works too -- `automation seedWelcomeCurriculum {}` -> bare
// `automationSeedWelcomeCurriculum({})`.
func TestAutomationWithinAutomation_BareCall(t *testing.T) {
	source := `@trigger(event="x.y.z")
automation parent {
  step run {
    automation child { }
  }
}`
	rewritten, err := NormaliseAutomationSource(source)
	if err != nil {
		t.Fatalf("NormaliseAutomationSource: %v", err)
	}
	if !strings.Contains(rewritten, "automationChild(") {
		t.Errorf("expected automationChild( call, got:\n%s", rewritten)
	}
}
