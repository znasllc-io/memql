package app

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/node"
	proc "github.com/znasllc-io/memql/component/procedure"
	"github.com/znasllc-io/memql/component/work"
	procedure "github.com/znasllc-io/memql/integrations/procedure"
)

// TestTheProcedureIntegrationShipsWithItsCompileGate is gaps G1 and G7 of epic
// memql#5408, and both are the same shape: a feature that is complete, tested
// and INERT in the shipped binary.
//
//   - G1: no binary imported integrations/procedure, so its builtins reached
//     no executor on any node. The registration is asserted in THIS test
//     binary, which links app/ exactly as every node binary does. (It holds
//     twice over: plugins_core.go blank-imports the package, and this
//     wiring's own file imports it by name -- so removing the blank import
//     alone does not unregister it, and removing both does not compile.)
//   - G7: the plug-in looked for Gate 1 on its engine, which has no such
//     method, so no learned procedure was ever compiled or recorded as
//     re-runnable. The gate is asserted INSTALLED on the registered instance
//     by the wiring integrationsCore calls on every node type.
//
// A capability that refuses, or a procedure that is "not re-runnable", is a
// well-formed answer -- which is why neither gap failed a single test until
// the wiring itself was asserted.
func TestTheProcedureIntegrationShipsWithItsCompileGate(t *testing.T) {
	var reg *memql.PluginRegistration
	for _, p := range memql.RegisteredPlugins() {
		if p.Name == "procedure" {
			p := p
			reg = &p
		}
	}
	if reg == nil {
		t.Fatal("the procedure plug-in is not registered in the app binary: plugins_core.go must blank-import integrations/procedure (gap G1)")
	}

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
	app := &App{engine: engine, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	// Materialized the way materializePlugins does it: the registered
	// factory, the app's own plugin context, the engine's registry.
	if err := reg.ValidateContract(); err != nil {
		t.Fatal(err)
	}
	provider, err := reg.Factory(app.pluginContext())
	if err != nil || provider == nil {
		t.Fatalf("the procedure factory did not build an integration: %v", err)
	}
	if err := engine.RegisterIntegration(provider); err != nil {
		t.Fatalf("RegisterIntegration: %v", err)
	}

	integ := app.lookupProcedureIntegration()
	if integ == nil {
		t.Fatal("the registered procedure integration is not the expected type")
	}
	if integ.CompileGateInstalled() {
		t.Fatal("the gate was installed before the wiring ran, so this test would pass having wired nothing")
	}
	app.wireProcedureIntegration()
	if !integ.CompileGateInstalled() {
		t.Fatal("wireProcedureIntegration did not install Gate 1 on the registered instance (gap G7)")
	}

	// And the wiring is on the boot path every node type takes.
	src, err := os.ReadFile("integrations.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "a.wireProcedureIntegration()") {
		t.Fatal("integrationsCore does not call wireProcedureIntegration, so no node installs Gate 1")
	}
}

// ---------------------------------------------------------------------------
// The replay's seams (epic memql#5408, task memql#5411)
// ---------------------------------------------------------------------------

// procedureTestEngine is a real engine over the shipped DSL tree, with no
// database: registration and the tool registry need none.
func procedureTestEngine(t *testing.T) *memql.MemQLEngine {
	t.Helper()
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
	return engine
}

// procedureWiredApp builds an App over a real engine with the procedure
// plug-in materialized the way materializePlugins does it, and returns the
// REGISTERED instance, before any wiring ran.
func procedureWiredApp(t *testing.T) (*App, *procedure.Integration) {
	t.Helper()
	var reg *memql.PluginRegistration
	for _, p := range memql.RegisteredPlugins() {
		if p.Name == "procedure" {
			p := p
			reg = &p
		}
	}
	if reg == nil {
		t.Fatal("the procedure plug-in is not registered in the app binary")
	}
	engine := procedureTestEngine(t)
	app := &App{engine: engine, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	provider, err := reg.Factory(app.pluginContext())
	if err != nil || provider == nil {
		t.Fatalf("the procedure factory did not build an integration: %v", err)
	}
	if err := engine.RegisterIntegration(provider); err != nil {
		t.Fatalf("RegisterIntegration: %v", err)
	}
	integ := app.lookupProcedureIntegration()
	if integ == nil {
		t.Fatal("the registered procedure integration is not the expected type")
	}
	if wb, machine, prober, fallback := integ.ReplaySeamsInstalled(); wb || machine || prober || fallback {
		t.Fatal("a seam was installed before the wiring ran, so a wiring test would pass having wired nothing")
	}
	return app, integ
}

// TestTheProcedureIntegrationShipsWithItsWorkbenchDispatcher is the replay's
// workbench half, held to the wiring integrationsCore calls on every node
// type. A plug-in whose setters nothing calls is green in every test and
// inert in the cluster; the refusal a replay would then give --
// no_workbench_dispatcher -- is a well-formed answer, which is exactly why
// nothing else would notice.
//
// With MEMQL_WORKBENCH_REMOTE the node's workbench plug-in forwards to a
// workbench, so every node type installs it -- whatever this test binary was
// compiled as.
func TestTheProcedureIntegrationShipsWithItsWorkbenchDispatcher(t *testing.T) {
	t.Setenv("MEMQL_WORKBENCH_REMOTE", "1")
	t.Setenv("MEMQL_WORKBENCH_LOCAL_FALLBACK", "")
	app, integ := procedureWiredApp(t)
	app.wireProcedureIntegration()

	wb, _, prober, _ := integ.ReplaySeamsInstalled()
	if !wb {
		t.Fatal("wireProcedureIntegration did not install the workbench dispatcher on a node whose workbench forwards to a workbench")
	}
	if !prober {
		t.Fatal("wireProcedureIntegration installed the workbench dispatcher with no prober: every procedure that learned a precondition would refuse to start")
	}
}

// The other direction, and the reason the decision exists: v1:work:run
// events broadcast, so a shadow comparison can land on ANY mesh node, and on
// a node whose workbench plug-in would run the step on that pod's own disk
// the dispatcher must be ABSENT -- a refusal the runner names, rather than a
// learned procedure's commands running beside the planner's credentials.
func TestAWorkbenchThatWouldRunOnThisNodesOwnDiskIsNotInstalled(t *testing.T) {
	t.Setenv("MEMQL_WORKBENCH_REMOTE", "")
	t.Setenv("MEMQL_WORKBENCH_LOCAL_FALLBACK", "")
	app, integ := procedureWiredApp(t)
	app.wireProcedureIntegration()

	want, why := procedureWorkbenchIsSandboxed(node.CompiledNodeType(), false, false)
	wb, _, _, _ := integ.ReplaySeamsInstalled()
	if wb != want {
		t.Fatalf("workbench dispatcher installed = %v on a %s node without the remote flag, want %v (%s)",
			wb, node.CompiledNodeType(), want, why)
	}
	if node.CompiledNodeType() == node.NodeTypeBFF && wb {
		t.Fatal("an un-flagged bff got the workbench dispatcher, whose steps would run on the bff's own disk")
	}
}

func TestTheWorkbenchDispatcherIsInstalledOnlyWhereItReachesAWorkbench(t *testing.T) {
	for _, tc := range []struct {
		nodeType      node.NodeType
		remote        bool
		localFallback bool
		want          bool
	}{
		{node.NodeTypeAgent, true, false, true},
		{node.NodeTypeAgent, false, false, true}, // the workbench's own single-node mode
		{node.NodeTypeAgent, true, true, true},   // forwards, or that same single-node mode
		{node.NodeTypeBFF, true, false, true},
		{node.NodeTypeBFF, false, false, false},
		{node.NodeTypeWorkbench, false, false, true},
		{node.NodeTypeWorkbench, true, true, true},
		{node.NodeTypePlanner, false, false, false},
		{node.NodeTypePlanner, true, false, true}, // forwards, or refuses no_workbench_peer
		{node.NodeTypeMCP, false, false, false},
		{node.NodeTypeEdge, false, false, false},
		{node.NodeTypeIdentity, false, false, false},
		// WITH THE LOCAL FALLBACK, no peer means THIS node's own disk: the
		// remote flag no longer asserts that the step runs on a workbench.
		{node.NodeTypeBFF, true, true, false},
		{node.NodeTypePlanner, true, true, false},
		{node.NodeTypeMCP, true, true, false},
		{node.NodeTypeEdge, true, true, false},
		{node.NodeTypeIdentity, true, true, false},
		// The fallback without the remote flag changes nothing: it is only
		// consulted in remote mode.
		{node.NodeTypeBFF, false, true, false},
	} {
		got, why := procedureWorkbenchIsSandboxed(tc.nodeType, tc.remote, tc.localFallback)
		if got != tc.want || why == "" {
			t.Errorf("%s remote=%v localFallback=%v -> %v (%q), want %v", tc.nodeType, tc.remote, tc.localFallback, got, why, tc.want)
		}
	}
}

// The wiring reads the fallback from the environment the workbench plug-in
// reads it from, so a node where the plug-in would run a replay's step on its
// own disk gets no dispatcher -- whatever this test binary was compiled as.
func TestAWorkbenchWithTheLocalFallbackIsInstalledOnlyWhereItsOwnDiskIsAWorkbench(t *testing.T) {
	t.Setenv("MEMQL_WORKBENCH_REMOTE", "1")
	t.Setenv("MEMQL_WORKBENCH_LOCAL_FALLBACK", "1")
	app, integ := procedureWiredApp(t)
	app.wireProcedureIntegration()

	want, why := procedureWorkbenchIsSandboxed(node.CompiledNodeType(), true, true)
	wb, _, _, _ := integ.ReplaySeamsInstalled()
	if wb != want {
		t.Fatalf("workbench dispatcher installed = %v on a %s node with the remote flag and the local fallback, want %v (%s)",
			wb, node.CompiledNodeType(), want, why)
	}
	if node.CompiledNodeType() == node.NodeTypeBFF && wb {
		t.Fatal("a bff with the local fallback got the workbench dispatcher, whose steps would run on the bff's own disk when no peer answers")
	}
}

// The fallback is parsed the way integrations/workbench parses it, so the two
// agree about which nodes run steps on their own disk.
func TestTheLocalFallbackIsReadAsTheWorkbenchReadsIt(t *testing.T) {
	for value, want := range map[string]bool{
		"1": true, "true": true, "TRUE": true, " yes ": true, "On": true,
		"": false, "0": false, "no": false, "false": false, "enabled": false,
	} {
		t.Setenv("MEMQL_WORKBENCH_LOCAL_FALLBACK", value)
		if got := workbenchLocalFallbackEnabled(); got != want {
			t.Errorf("MEMQL_WORKBENCH_LOCAL_FALLBACK=%q -> %v, want %v", value, got, want)
		}
	}
}

// The installed dispatcher reaches the REGISTERED workbench plug-in by name --
// the only way a surface behind another integration can be reached without
// importing it -- and a node without one refuses by name.
func TestTheWorkbenchDispatcherReachesTheRegisteredWorkbench(t *testing.T) {
	engine := procedureTestEngine(t)
	d := newProcedureWorkbenchDispatcher(engine)
	if _, err := d.Dispatch(context.Background(), workbenchStep("exec", map[string]any{"command": "ls"})); err == nil ||
		!strings.Contains(err.Error(), "no workbench dispatchHost") {
		t.Fatalf("an engine with no workbench = %v", err)
	}

	fake := newFakeWorkbench()
	if err := engine.RegisterIntegration(fakeDispatchHostIntegration{name: "workbench", handler: fake.handler()}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Dispatch(context.Background(), workbenchStep("exec", map[string]any{"command": "ls"})); err != nil {
		t.Fatal(err)
	}
	if len(fake.calls) != 1 || fake.calls[0]["action"] != "exec" {
		t.Fatalf("the registered workbench saw %v", fake.actions())
	}
}

// fakeDispatchHostIntegration registers a dispatchHost under a real
// integration's name.
type fakeDispatchHostIntegration struct {
	name    string
	handler procedureCapability
}

func (f fakeDispatchHostIntegration) IntegrationName() string { return f.name }

func (f fakeDispatchHostIntegration) Capabilities() []memql.IntegrationCapability {
	// A func LITERAL, because a named func type is not assignable to the
	// engine's unexported handler type and a literal is.
	return []memql.IntegrationCapability{{Name: "dispatchHost", Handler: func(ctx context.Context, args map[string]any, target int) ([]memorynodes.MemoryNode, error) {
		return f.handler(ctx, args, target)
	}}}
}

// component/procedure spells the targets as plain strings because it cannot
// import component/work; the replay compares them across that line, so the
// two spellings are pinned together here.
func TestTheReplayTargetsAreSpelledAsComponentProcedureSpellsThem(t *testing.T) {
	if string(work.TargetWorkbench) != proc.TargetWorkbench || string(work.TargetMachine) != proc.TargetMachine {
		t.Fatalf("component/work spells %q/%q and component/procedure %q/%q",
			work.TargetWorkbench, work.TargetMachine, proc.TargetWorkbench, proc.TargetMachine)
	}
}
