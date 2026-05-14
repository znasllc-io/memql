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
	source := `@trigger(event="graph.node.created.*.v1:cognition:space")
automation onSpaceCreated {
  step welcome {
    automation seedWelcomeCurriculum { spaceId: event.payload.id, userId: event.payload.actor }
  }
}`

	rewritten, err := NormaliseAutomationSource(source)
	if err != nil {
		t.Fatalf("NormaliseAutomationSource: %v", err)
	}

	// The rewriter should have emitted automationSeedWelcomeCurriculum
	// as a function-call expression, with args translated and event.X
	// rewritten to ctx.input.X.
	if !strings.Contains(rewritten, "automationSeedWelcomeCurriculum(") {
		t.Errorf("expected rewritten source to contain automationSeedWelcomeCurriculum( call, got:\n%s", rewritten)
	}
	if !strings.Contains(rewritten, "ctx.input.payload.id") {
		t.Errorf("expected event.payload.id rewrite to ctx.input.payload.id, got:\n%s", rewritten)
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
