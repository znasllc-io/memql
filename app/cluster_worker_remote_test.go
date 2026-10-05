//go:build !agent && !identity && !workbench && !mcp && !edge

package app

import (
	"log/slog"
	"strings"
	"testing"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/node"
)

func TestRemoteInferenceCannotCompileTheWorkerForwardingNoop(t *testing.T) {
	if !strings.Contains(readAppFile(t, "cluster_worker_noagent.go"), "//go:build !agent && !planner && (identity || workbench || mcp || edge)") {
		t.Fatal("planner work compilation sees the fleet catalog but installs no callable fleet inference; it compiles the worker-forwarding no-op")
	}
}

func TestBFFAndPlannerInstallInferenceOnTheirExistingAgentDialer(t *testing.T) {
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
	identity := &node.Identity{ID: "remote", Type: node.CompiledNodeType()}
	peers := node.NewPeerManager(identity, slog.Default())
	dialer := node.NewWorkerDialer(identity, peers, engine, nil, nil, slog.Default())
	if dialer == nil {
		t.Fatal("planner agent dialer was not constructed")
	}
	dialer.SetDialTypes(node.NodeTypeAgent)
	a := &App{engine: engine, Logger: slog.Default()}
	a.Dependencies = append(a.Dependencies, dialer)
	a.wireFleetCatalog()
	if engine.Providers().FleetInferenceInstalled() {
		t.Fatal("catalog alone claims inference")
	}
	if engine.Providers().AppInferenceInstalled() {
		t.Fatal("the planner claims an app door before its forward is wired")
	}
	a.wireWorkerForwarding(identity, peers, nil, nil)
	if !engine.Providers().FleetInferenceInstalled() {
		t.Fatal("planner still cannot dispatch to its visible fleet models")
	}
	// THE APP DOOR RIDES THE SAME FORWARD (the planner/app-source design,
	// section 3a). Without it every `app:` source in a planner route --
	// triage, compile, compose -- is passed over on every call.
	if !engine.Providers().AppInferenceInstalled() {
		t.Fatal("planner routes naming an app source still cannot reach the agent holding the machine")
	}
	if len(a.Dependencies) != 1 || a.existingWorkerDialer() != dialer || a.workerService != nil {
		t.Fatal("planner inference created a second dialer or a local WorkerService")
	}
}
