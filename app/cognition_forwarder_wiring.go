//go:build !agent && !planner

package app

import (
	memqlgrpc "github.com/visionarys-io/memql/component/grpc"
	"github.com/visionarys-io/memql/integrations/cognition"
)

// attachAgentForwarderToCognition installs the agent-turn forwarder onto
// the cognition integration that was created earlier in the integrations
// phase. Called from cluster.go after NewAiForwardRouter constructs the
// forwarder. On binaries without cognition (agent, planner) this file
// is absent; the default build and cognition build both include it.
func (a *App) attachAgentForwarderToCognition(fwd *memqlgrpc.AiForwardRouter) {
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
