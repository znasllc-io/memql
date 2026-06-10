//go:build !agent && !planner

package app

import (
	memqlgrpc "github.com/znasllc-io/memql/component/grpc"
	"github.com/znasllc-io/memql/component/node"
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
}

// attachClientToolResultClientToCognition installs the substrate-RPC
// client-tool result client (memql#1265) onto the cognition integration so the
// client-tool relay's return leg routes the ClientToolResult to the agent turn's
// logical key over the durable substrate instead of ForwardContinuation. Mirrors
// attachAgentForwarderToCognition's build-tag split (real here; no-op stub on
// agent/planner).
func (a *App) attachClientToolResultClientToCognition(client *node.ClientToolResultClient) {
	if a.cognitionIntegration == nil {
		return
	}
	integ, ok := a.cognitionIntegration.(*cognition.CognitionIntegration)
	if !ok || integ == nil {
		a.Logger.Warn("cognition integration missing; cannot install client-tool result client")
		return
	}
	integ.SetClientToolResultClient(client)
	a.Logger.Info("cognition: substrate client-tool result client installed")
}
