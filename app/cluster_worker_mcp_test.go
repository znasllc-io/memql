//go:build mcp

package app

import (
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/node"
	"log/slog"
	"testing"
)

func TestMCPHostToolsReuseOneOutboundDialerAndOwnNoMachineStreams(t *testing.T) {
	engine, err := memql.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	if err := engine.Init(memorynodes.DefaultRegistry()); err != nil {
		t.Fatal(err)
	}
	identity := &node.Identity{ID: "mcp-host-test", Type: node.NodeTypeMCP}
	peers := node.NewPeerManager(identity, slog.Default())
	a := &App{engine: engine, Logger: slog.Default()}
	a.wireWorkerForwarding(identity, peers, nil, nil)
	if engine.IntegrationByName("agentworker") == nil {
		t.Fatal("MCP tools have no worker dispatcher")
	}
	if a.workerService != nil {
		t.Fatal("MCP must not host machine streams")
	}
	if !engine.Providers().FleetInferenceInstalled() || !engine.Providers().AppInferenceInstalled() {
		t.Fatal("tool-side inference lost the Apps/local source")
	}
	if countDialers(a) != 1 {
		t.Fatal("expected one agent dialer")
	}
	t.Setenv("MEMQL_WORKBENCH_REMOTE", "")
	a.wireWorkbenchForwarding(identity, peers, nil, nil)
	if countDialers(a) != 1 {
		t.Fatal("cloud workbench forwarding must reuse the agent dialer")
	}
}
