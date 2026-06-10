//go:build !agent && !planner

package app

import (
	memqlgrpc "github.com/znasllc-io/memql/component/grpc"
	"github.com/znasllc-io/memql/integrations/cognition"
)

// attachAgentForwarderToCognition installs the agent-turn forwarder onto
// the cognition integration that was created earlier in the integrations
// phase. Called from cluster.go after NewAiForwardRouter constructs the
// forwarder. On binaries without cognition (agent, planner) this file
// is absent; the default build and cognition build both include it.
func (a *App) attachAgentForwarderToCognition(fwd *memqlgrpc.SIForwardRouter) {
	if a.cognitionIntegration == nil {
		return
	}
	integ, ok := a.cognitionIntegration.(*cognition.CognitionIntegration)
	if !ok || integ == nil {
		a.Logger.Warn("cognition integration missing; cannot install agent forwarder")
		return
	}
	integ.SetAgentForwarder(fwd)
	a.Logger.Info("cognition: agent-turn forwarder installed")

	// Phase 2 (memql#1265): hand cognition the request/response RPC layer so the
	// client-tool relay's reply leg can route a ClientToolResult back to the
	// agent by logical key (surviving this cognition replica restarting
	// mid-call). Built over the shared substrate earlier in the cluster phase;
	// nil-tolerant -- the relay falls back to ForwardContinuation when absent or
	// when the gate (MEMQL_CLIENT_TOOL_RPC_SUBSTRATE) is off.
	if a.substrateRPC != nil {
		integ.SetClusterRPC(a.substrateRPC)
		a.Logger.Info("cognition: client-tool relay RPC layer installed (memql#1265)")
	}
}
