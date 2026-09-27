package app

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"

	"github.com/znasllc-io/memql/component/node"
	"github.com/znasllc-io/memql/component/work"
	procedure "github.com/znasllc-io/memql/integrations/procedure"
)

// integrations_procedure.go joins procedure learning to its runtime
// collaborators (epic memql#5408).
//
// ===========================================================================
// GATE 1 IS INSTALLED HERE OR IT NEVER RUNS (gap G7)
// ===========================================================================
// A learned procedure is recorded as re-runnable -- given the goal signature
// that makes it findable, and let onto the certification ladder above
// candidate -- only when its source passes Gate 1, the isolated compile +
// bind. The plug-in used to look for that compile by type-asserting the
// engine its PluginContext hands it, and *memql.MemQLEngine has no such
// method: the compile is a function of the engine, which CognitionEngineAdapter
// wraps. So Gate 1 never ran on any node, every procedure was recorded as not
// re-runnable, and none was ever findable -- with every test green, because a
// procedure that is merely "not re-runnable" is a well-formed answer.
//
// ===========================================================================
// THE REPLAY'S SEAMS ARE INSTALLED HERE, BY THE NODE THAT HAS THEM
// ===========================================================================
// A replay runs a learned procedure's steps on a target (D4) and needs four
// things the plug-in cannot build itself: a dispatcher per target, a prober,
// and the hand-back to the app. Each is installed where its surface is REAL,
// and nowhere else -- a seam installed where it cannot work turns a replay
// that should have fallen back to the app into one that "ran" somewhere it
// must not:
//
//   - the WORKBENCH dispatcher and its half of the prober, on a node whose
//     workbench plug-in reaches an actual workbench
//     (procedureWorkbenchIsSandboxed);
//   - the MACHINE dispatcher, its half of the prober, and the APP FALLBACK on
//     the agent node only (integrations_procedure_agent.go): the worker
//     streams and the app-session delegate live there.
//
// UNTAGGED, and called from integrationsCore, because the plug-in registers on
// every node type and a lift runs wherever a run succeeds. v1:work:run events
// broadcast, so learnFromSucceededRun -- and the shadow comparison it runs --
// can fire on ANY mesh node, which is exactly why the workbench dispatcher is
// decided per node here rather than assumed everywhere.
//
// The REGISTERED instance, never a second one: capability dispatch goes
// through the instance the registry built, so a seam installed on another
// would be configured and never consulted.
func (a *App) wireProcedureIntegration() {
	integ := a.lookupProcedureIntegration()
	if integ == nil {
		// A node whose engine did not materialize the plug-in has no lift to
		// gate. Debug, not a warning: a test engine or a stripped build.
		a.procedureLog().Debug("procedure integration not wired: the plug-in did not materialize", "component", "procedure")
		return
	}
	integ.SetCompiler(&CognitionEngineAdapter{Engine: a.engine})
	a.procedureLog().Info("procedure integration wired: Gate 1 compiles every learned procedure", "component", "procedure")
	a.wireProcedureReplaySeams(integ)
}

// wireProcedureReplaySeams installs what a replay runs on.
func (a *App) wireProcedureReplaySeams(integ *procedure.Integration) {
	log := a.procedureLog()
	prober := &procedureProber{}

	nodeType := node.CompiledNodeType()
	if sandboxed, why := procedureWorkbenchIsSandboxed(nodeType, workbenchRemoteEnabled(), workbenchLocalFallbackEnabled()); sandboxed {
		integ.SetDispatcher(work.TargetWorkbench, newProcedureWorkbenchDispatcher(a.engine))
		prober.setHost(work.TargetWorkbench, a.procedureWorkbenchHostBuilder())
		log.Info("procedure replay: the workbench dispatcher and prober are installed",
			"component", "procedure", "nodeType", string(nodeType), "reason", why)
	} else {
		log.Info("procedure replay: NO workbench dispatcher on this node; a replay that lands here refuses the workbench target rather than running its steps",
			"component", "procedure", "nodeType", string(nodeType), "reason", why)
	}

	// The agent node adds the machine and the app; every other build says so.
	a.wireProcedureAgentSeams(integ, prober)

	if prober.measures() {
		integ.SetProber(prober)
	}
}

// procedureWorkbenchIsSandboxed answers whether the workbench plug-in's
// dispatchHost, on a node of this type, reaches an ACTUAL WORKBENCH.
//
// The plug-in links into every binary and registers everywhere, so "the
// capability exists" says nothing. What it does when called depends on the
// node:
//
//   - with MEMQL_WORKBENCH_REMOTE it forwards to a workbench node -- or, with
//     no reachable peer, refuses (memql#3506). The base sets it on the agent
//     and the bff.
//   - with MEMQL_WORKBENCH_LOCAL_FALLBACK as well, no reachable peer means it
//     runs the step LOCALLY instead of refusing: the remote flag stops being
//     an assertion that the step lands on a workbench.
//   - on the WORKBENCH node it runs the step locally, which is what that node
//     is for.
//   - on the AGENT without the flag -- or with both, and no peer -- it runs
//     locally on the agent's disk: the workbench's own single-node mode, the
//     one topology that runs it that way on purpose.
//   - anywhere else it would run the step on THAT node's own disk -- the
//     planner, the mcp, the edge, the identity node or a bff, un-flagged or
//     flagged with the fallback -- inside a pod that holds that node's
//     credentials, which is not a sandbox.
//
// The node type is the COMPILED one. Every shipped image is built with its
// node type's tag (BUILD_TAGS=<type>); an untagged binary is a bff, and a dev
// build that names another type in MEMQL_NODE_TYPE is treated as the bff it
// was compiled as -- the conservative direction, since it then gets the
// dispatcher only with the remote flag set.
func procedureWorkbenchIsSandboxed(nodeType node.NodeType, remote, localFallback bool) (bool, string) {
	switch {
	case nodeType == node.NodeTypeWorkbench:
		return true, "this node is the workbench"
	case nodeType == node.NodeTypeAgent && remote:
		return true, "dispatchHost forwards to a workbench node (MEMQL_WORKBENCH_REMOTE); with no peer it refuses or, under MEMQL_WORKBENCH_LOCAL_FALLBACK, runs the workbench's single-node mode on the agent's own disk"
	case nodeType == node.NodeTypeAgent:
		return true, "the agent runs the workbench's single-node mode on its own disk"
	case remote && localFallback:
		return false, "MEMQL_WORKBENCH_LOCAL_FALLBACK lets dispatchHost run a replay's step on this node's own disk when no workbench peer answers, which is not a workbench"
	case remote:
		return true, "dispatchHost forwards to a workbench node (MEMQL_WORKBENCH_REMOTE), or refuses with no_workbench_peer"
	}
	return false, "dispatchHost here would run a replay's steps on this node's own disk, which is not a workbench"
}

// workbenchLocalFallbackEnabled reads MEMQL_WORKBENCH_LOCAL_FALLBACK exactly
// as integrations/workbench reads it (trimmed, case-insensitive 1, true, yes
// or on), so the replay and the plug-in agree about which nodes would run a
// step on their own disk. Only consulted with the remote flag, as there.
func workbenchLocalFallbackEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("MEMQL_WORKBENCH_LOCAL_FALLBACK"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// procedureWorkbenchHostBuilder builds the workbench host a probe runs on.
func (a *App) procedureWorkbenchHostBuilder() procedureHostBuilder {
	engine := a.engine
	return func(_ context.Context, _ string, runId string) (procedureHost, error) {
		handler := procedureCapabilityByName(engine, "workbench", "dispatchHost")
		if handler == nil {
			return nil, errProcedureNoWorkbench
		}
		return &workbenchProcedureHost{handler: handler, runId: runId}, nil
	}
}

// errProcedureNoWorkbench is the refusal for a workbench host with no
// dispatchHost behind it.
var errProcedureNoWorkbench = errors.New("this node has no workbench dispatchHost registered")

// procedureLog is the app's logger, or the default one for an App a test
// built without one.
func (a *App) procedureLog() *slog.Logger {
	if a != nil && a.Logger != nil {
		return a.Logger
	}
	return slog.Default()
}

// lookupProcedureIntegration returns the registered plug-in instance, or nil.
func (a *App) lookupProcedureIntegration() *procedure.Integration {
	if a == nil || a.engine == nil {
		return nil
	}
	provider := a.engine.IntegrationByName("procedure")
	if provider == nil {
		return nil
	}
	integ, _ := provider.(*procedure.Integration)
	return integ
}
