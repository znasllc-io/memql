//go:build agent

package app

import (
	"context"
	"fmt"
	"github.com/znasllc-io/memql/component/memql"

	memqlgrpc "github.com/znasllc-io/memql/component/grpc"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/integrations/agent"
	"github.com/znasllc-io/memql/integrations/agents"
)

// integrationsAgent registers integration providers for an agent node.
// Core plug-ins (database, auth, identity, embedding, files, storage)
// self-register via memql.RegisterPlugin and are materialized in
// integrationsCore. STT is still wired explicitly because its construction
// depends on provider selection logic outside the PluginContext surface.
// The agent reply pipeline is wired in transportAgent after the gRPC
// server exists -- see setupAgentReplier below.
func (a *App) integrationsAgent() {
	a.integrationsCore()
	a.selectSTTProvider()
	a.wireAskVoice()

	// The agent node is what EXECUTES a compiled work run (memql#5054).
	// Safe here because engineAndBus() has already run -- it is what builds
	// the automation loader, the step registry and the cluster execution
	// guard this needs -- and integrationsCore() above is what materializes
	// the work plug-in it installs itself on.
	a.wireWorkRunDispatcher()

	a.Logger.Info("agent integration providers registered")
}

// setupAgentReplier constructs the agent reply pipeline and installs it on
// the gRPC server so AgentGenerateTurnMsg is dispatched through it.
// Called from transportAgent after transportBase() has constructed the
// server.
//
// CognitionEngineAdapter happens to expose exactly the methods
// agent.MemQLEngine requires, so it serves as the engine backing for the
// replier too. The name is historical; it's a generic integration engine
// adapter despite the cognition prefix.
func (a *App) setupAgentReplier() {
	engineAdapter := &CognitionEngineAdapter{Engine: a.engine}

	if a.router == nil {
		a.fatal("agent replier setup: AI router not initialized (engineAndBus must run first)")
	}

	replier, err := agent.NewReplier(engineAdapter, a.router, a.Logger)
	if err != nil {
		a.fatal("failed to create agent replier", "error", err)
	}

	// Wire computer_use status into the prompt-build path so the
	// agent knows up front whether workerHost / workerComputer
	// will dispatch successfully. setupWorkerService runs BEFORE
	// us in transportAgent so a.workerService is populated.
	replier.ComputerUseStatus = a.computerUseStatusFn()

	if a.grpcServer == nil {
		a.fatal("grpc server not constructed before agent replier setup")
	}
	a.grpcServer.SetAgentTurnHandler(&agentReplierHandlerAdapter{replier: replier})

	// The same Replier, reachable from a DSL builtin (memql#5048). A work
	// step calling runAgentTurn runs on THIS node -- design record section H
	// puts step execution on the agent node -- and there is no AiForwardRouter
	// here to forward with, so it dispatches locally through the entry point
	// the gRPC server already uses.
	//
	// WITHOUT THIS CALL THE BUILTIN IS INERT, and inert in the way that costs
	// an afternoon: the seam is nil, every runAgentTurn refuses, and the
	// refusal reads like a topology problem rather than a missing wiring line.
	a.wireAgentTurnRunner(replier)

	// Tool calls are recorded as v1:work:observation rows against the run
	// they happen inside (memql#5050), replacing the v1:planner:task rows
	// component/memql/taskstamp used to write. Nil here is not a failure --
	// a turn outside any run records nothing by design -- but a node with a
	// work integration and no recorder installed would silently record
	// nothing for runs too, so it is said out loud.
	if work := a.lookupWorkIntegration(); work != nil {
		replier.SetToolInvocationRecorder(work)
	} else {
		a.Logger.Warn("agent: no work integration on this node, so tool calls made inside a run will not be recorded",
			"component", "work.observation")
	}
	// Cooperative preemption signal (epic memql#902 / #906): the planner's
	// cross-node AgentPreemptTurn lands here and flags the in-flight turn to
	// pause at its next checkpoint. component/grpc can't import
	// integrations/agent, so the func is injected.
	a.grpcServer.SetAgentPauseHook(agent.RequestPause)
	a.Logger.Info("agent replier registered on gRPC server")
}

// agentReplierHandlerAdapter bridges agent.Replier (which emits
// agent.DeltaSink and returns *agent.TurnResult) onto grpc.AgentTurnHandler
// (which uses grpc.AgentTurnSink / *grpc.AgentTurnResult). The two are
// shape-compatible; this adapter forwards each call.
type agentReplierHandlerAdapter struct {
	replier *agent.Replier
}

func (a *agentReplierHandlerAdapter) Handle(ctx context.Context, msg *memqlv1.AgentGenerateTurnMsg, sink memqlgrpc.AgentTurnSink) (*memqlgrpc.AgentTurnResult, error) {
	deltaSink := &agentSinkBridge{grpcSink: sink}
	result, err := a.replier.Handle(ctx, msg, deltaSink)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return &memqlgrpc.AgentTurnResult{}, nil
	}
	var citations []*memqlv1.AgentTurnCitation
	if len(result.Citations) > 0 {
		citations = make([]*memqlv1.AgentTurnCitation, 0, len(result.Citations))
		for _, c := range result.Citations {
			citations = append(citations, &memqlv1.AgentTurnCitation{
				DomainId:      c.DomainId,
				MatchedPhrase: c.MatchedPhrase,
			})
		}
	}
	var retrieved []*memqlv1.AgentRetrievedChunk
	if len(result.RetrievedChunks) > 0 {
		retrieved = make([]*memqlv1.AgentRetrievedChunk, 0, len(result.RetrievedChunks))
		for _, c := range result.RetrievedChunks {
			retrieved = append(retrieved, &memqlv1.AgentRetrievedChunk{
				DomainId:    c.DomainId,
				SourceRef:   c.SourceRef,
				Similarity:  c.Similarity,
				TextPreview: c.TextPreview,
				Citation:    c.Citation,
			})
		}
	}
	return &memqlgrpc.AgentTurnResult{
		FinalText:  result.FinalText,
		TextChunks: result.TextChunks,
		ToolCalls:  result.ToolCalls,
		Iterations: result.Iterations,
		Citations:  citations,
		Retrieved:  retrieved,
		Paused:     result.Paused,
	}, nil
}

// agentSinkBridge adapts a grpc.AgentTurnSink to agent.DeltaSink.
type agentSinkBridge struct {
	grpcSink memqlgrpc.AgentTurnSink
}

func (a *agentSinkBridge) TextDelta(text string) {
	a.grpcSink.TextDelta(text)
}

func (a *agentSinkBridge) ToolCall(id, name, argumentsJSON string) {
	a.grpcSink.ToolCall(id, name, argumentsJSON)
}

func (a *agentSinkBridge) ToolResult(id, resultJSON, errMsg string) {
	a.grpcSink.ToolResult(id, resultJSON, errMsg)
}

// wireAgentTurnRunner hands the Replier to the agents plug-in.
//
// The plug-in is CORE -- it loads on identity, edge, mcp and every other node
// type, none of which builds integrations/agent at all. So the seam is an
// interface it declares and this file, which exists only under the agent build
// tag, is the one place that can fill it.
func (a *App) wireAgentTurnRunner(replier *agent.Replier) {
	if a.engine == nil || replier == nil {
		return
	}
	provider := a.engine.IntegrationByName("agents")
	if provider == nil {
		a.Logger.Warn("agent turn runner not wired: the agents plug-in did not materialize on this agent node; runAgentTurn will refuse and a work run that invokes an agent will fail",
			"component", "agents")
		return
	}
	integ, ok := provider.(*agents.Integration)
	if !ok || integ == nil {
		a.Logger.Warn("agent turn runner not wired: the agents provider is not the expected type", "component", "agents")
		return
	}
	integ.SetAgentTurnRunner(&replierTurnRunner{replier: replier, engine: a.engine})
	a.Logger.Info("agent turn runner wired; runAgentTurn dispatches locally on this node", "component", "agents")
}

// replierTurnRunner sends every work turn through the same runtime and records
// stream snapshots in the shared journal for conversational and goal viewers.
type replierTurnRunner struct {
	replier *agent.Replier
	engine  *memql.MemQLEngine
}

func (r *replierTurnRunner) RunTurn(ctx context.Context, msg *memqlv1.AgentGenerateTurnMsg) (string, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	sink := &workTurnDeltas{ctx: ctx, engine: r.engine, id: msg.RequestId, cancel: cancel}
	ctx = r.engine.ObserveWorkCalls(ctx, cancel)
	result, err := r.replier.Handle(ctx, msg, sink)
	if cause := context.Cause(ctx); cause != nil {
		return "", cause
	}
	if err != nil {
		return "", err
	}
	if result == nil {
		return "", fmt.Errorf("agent turn returned no result")
	}
	if err := sink.finish(result.FinalText); err != nil {
		return "", err
	}
	return result.FinalText, nil
}
