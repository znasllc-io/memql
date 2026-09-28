package app

import (
	"strings"
	"testing"
)

// The validator claim is wired from integrationsCore, which EVERY node type
// runs after the engine phase that builds the cluster guard -- the
// validateGoalAnswer automation fires wherever the work automations load, so
// a claim wired on one node type would leave the others racing it. A SOURCE
// assertion, as TestComposeIntegrationIsWiredFromTheTransport is: the wiring
// is a line that a refactor can drop with every package test still green.
func TestTheAnswerValidatorIsClaimedOnEveryNodeType(t *testing.T) {
	src := readAppFile(t, "integrations.go")
	core := src[strings.Index(src, "func (a *App) integrationsCore()"):]
	if end := strings.Index(core, "\n}\n"); end > 0 {
		core = core[:end]
	}
	if !strings.Contains(core, "a.wireWorkValidatorClaim()") {
		t.Fatal("integrationsCore does not call wireWorkValidatorClaim(); the answer validator would run once per replica an event reaches")
	}
	if !strings.Contains(readAppFile(t, "integrations_work_validator.go"), "work.SetValidatorClaimer(a.clusterGuard)") {
		t.Error("wireWorkValidatorClaim does not install the cluster guard as the validator's claimer")
	}
}
