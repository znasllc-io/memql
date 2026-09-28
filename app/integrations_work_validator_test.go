package app

import (
	"strings"
	"testing"
)

// The validator claim is wired from integrationsAgent, after
// integrationsCore has materialized the work plug-in and engineAndBus has
// built the cluster guard -- on AGENT nodes, the ones that can serve the
// check (app/integrations_work_validator.go says why not everywhere). A
// SOURCE assertion, as TestComposeIntegrationIsWiredFromTheTransport is: the
// wiring is a line a refactor can drop with every package test still green.
func TestTheAnswerValidatorIsClaimedAmongAgentReplicas(t *testing.T) {
	src := readAppFile(t, "integrations_agent.go")
	agent := src[strings.Index(src, "func (a *App) integrationsAgent()"):]
	if end := strings.Index(agent, "\n}\n"); end > 0 {
		agent = agent[:end]
	}
	core, claim := strings.Index(agent, "a.integrationsCore()"), strings.Index(agent, "a.wireWorkValidatorClaim()")
	if claim < 0 {
		t.Fatal("integrationsAgent does not call wireWorkValidatorClaim(); two copies of one run transition on an agent would each check it, and each could open an app session")
	}
	if core < 0 || claim < core {
		t.Error("wireWorkValidatorClaim must run after integrationsCore, which materializes the work plug-in it installs on")
	}
	if strings.Contains(readAppFile(t, "integrations.go"), "a.wireWorkValidatorClaim()") {
		t.Error("the claim is wired on every node type; a replica that cannot serve the check would win it and leave the agent's copy skipped")
	}
	if !strings.Contains(readAppFile(t, "integrations_work_validator.go"), "work.SetValidatorClaimer(a.clusterGuard)") {
		t.Error("wireWorkValidatorClaim does not install the cluster guard as the validator's claimer")
	}
}
