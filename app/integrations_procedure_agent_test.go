//go:build agent

package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	procedure "github.com/znasllc-io/memql/integrations/procedure"
)

// TestTheAgentNodeInstallsTheMachineDispatcherAndTheAppFallback holds the
// agent node to the replay's agent-only seams (epic memql#5408, D4, D16):
// the machine dispatcher, the prober that measures the machine, and the
// hand-back to the app. Each is a setter on the plug-in, and each missing
// one is a well-formed refusal -- no_machine_dispatcher,
// procedure_fallback_unavailable -- so nothing but this test would notice a
// wiring line that went away. The workbench half rides along: an agent
// reaches a workbench with or without the remote flag.
func TestTheAgentNodeInstallsTheMachineDispatcherAndTheAppFallback(t *testing.T) {
	t.Setenv("MEMQL_WORKBENCH_REMOTE", "")
	app, integ := procedureWiredApp(t)
	app.wireProcedureIntegration()

	wb, machine, prober, fallback := integ.ReplaySeamsInstalled()
	if !machine {
		t.Error("the agent node did not install the machine dispatcher: every machine-local procedure would refuse to replay")
	}
	if !fallback {
		t.Error("the agent node did not install the app fallback: every divergence would fail its goal instead of handing it back")
	}
	if !prober {
		t.Error("the agent node installed no prober: every procedure that learned a precondition would refuse to start")
	}
	if !wb {
		t.Error("the agent node did not install the workbench dispatcher, which its own single-node workbench serves")
	}
}

// The machine dispatcher reaches the REGISTERED worker by name, at dispatch
// time -- the worker registers in the transport phase, after these seams are
// installed, which is exactly why nothing is resolved when they are.
func TestTheMachineDispatcherReachesTheRegisteredWorker(t *testing.T) {
	engine := procedureTestEngine(t)
	d := newProcedureMachineDispatcher(engine)
	d.agents = testAgents
	req := machineStep("fs_read", map[string]any{"file_path": "/etc/hosts"})
	if _, err := d.Dispatch(context.Background(), req); err == nil || !strings.Contains(err.Error(), "no worker dispatchHost") {
		t.Fatalf("an engine with no worker = %v", err)
	}

	m := newFakeMachine()
	m.files["/etc/hosts"] = "127.0.0.1 localhost\n"
	if err := engine.RegisterIntegration(fakeDispatchHostIntegration{name: "agentworker", handler: m.handler()}); err != nil {
		t.Fatal(err)
	}
	res, err := d.Dispatch(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.calls) != 1 || len(res.Observation.Contents) != 1 || res.Observation.Contents[0].Digest != procedureDigest("127.0.0.1 localhost\n") {
		t.Fatalf("the registered worker saw %v and answered %+v", m.actions(), res.Observation)
	}
}

// The production fallback resolves through the ENGINE'S router seam, never
// around it; an engine with no router installed refuses by name, which is
// the router seam's own sentinel coming back.
func TestTheAppFallbackResolvesThroughTheEnginesRouterSeam(t *testing.T) {
	engine := procedureTestEngine(t)
	f := newProcedureAppFallback(engine)
	f.agents = testAgents
	_, err := f.Handover(context.Background(), fallbackRequest())
	if err == nil || !errors.Is(err, memql.ErrAIResolverUnwired) {
		t.Fatalf("an engine with no router = %v, want the seam's own ErrAIResolverUnwired", err)
	}
}

// The prober the agent installs measures the machine; the workbench half is
// the untagged wiring's.
func TestTheAgentsProberMeasuresTheMachine(t *testing.T) {
	app := &App{engine: procedureTestEngine(t)}
	p := &procedureProber{}
	app.wireProcedureAgentSeams(procedure.New(nil, nil), p)
	if _, ok := p.hosts[work.TargetMachine]; !ok {
		t.Fatal("the agent's prober cannot measure the machine")
	}
}
