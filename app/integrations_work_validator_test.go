package app

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Answer checks are served from integrationsAgent, after integrationsCore has
// materialized the work plug-in and engineAndBus has built the cluster guard
// -- on AGENT nodes, and ONLY there (app/integrations_work_validator.go says
// why). A SOURCE assertion, as TestComposeIntegrationIsWiredFromTheTransport
// is: the wiring is a line a refactor can drop, or copy to every node type,
// with every package test still green.
func TestAnswerChecksAreServedOnAgentNodesOnly(t *testing.T) {
	src := readAppFile(t, "integrations_agent.go")
	agent := src[strings.Index(src, "func (a *App) integrationsAgent()"):]
	if end := strings.Index(agent, "\n}\n"); end > 0 {
		agent = agent[:end]
	}
	core, serve := strings.Index(agent, "a.integrationsCore()"), strings.Index(agent, "a.wireAnswerChecks()")
	if serve < 0 {
		t.Fatal("integrationsAgent does not call wireAnswerChecks(); no node would serve answer checks, and every copy would skip")
	}
	if core < 0 || serve < core {
		t.Error("wireAnswerChecks must run after integrationsCore, which materializes the work plug-in it designates")
	}
	// No other file calls it: not integrationsCore (every node type), not a
	// planner or bff path.
	_, self, _, _ := runtime.Caller(0)
	files, err := filepath.Glob(filepath.Join(filepath.Dir(self), "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		name := filepath.Base(path)
		if strings.HasSuffix(name, "_test.go") || name == "integrations_agent.go" || name == "integrations_work_validator.go" {
			continue
		}
		if strings.Contains(readAppFile(t, name), "a.wireAnswerChecks()") {
			t.Errorf("%s serves answer checks; a node other than the agent would check the same answer again -- on an app-first route, in a second session", name)
		}
	}
	wiring := readAppFile(t, "integrations_work_validator.go")
	if !strings.Contains(wiring, "work.ServeAnswerChecks(a.clusterGuard)") {
		t.Error("wireAnswerChecks does not claim through the cluster guard")
	}
}
