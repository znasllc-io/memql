//go:build planner

package app

import (
	memqlgrpc "github.com/visionarys-io/memql/component/grpc"
	"github.com/visionarys-io/memql/integrations/planner"
)

// attachAgentForwarderToPlanner installs the agent-turn forwarder
// onto the planner integration that was created earlier in the
// integrations phase. Called from cluster.go after
// NewAiForwardRouter constructs the forwarder for a planner-typed
// node. The default build and the planner build are the only
// callsites; other binaries have the stub from
// planner_forwarder_wiring_stub.go.
func (a *App) attachAgentForwarderToPlanner(fwd *memqlgrpc.AiForwardRouter) {
	if a.plannerIntegration == nil {
		return
	}
	integ, ok := a.plannerIntegration.(*planner.PlannerIntegration)
	if !ok || integ == nil {
		a.Logger.Warn("planner integration missing; cannot install agent forwarder")
		return
	}
	integ.SetAgentForwarder(fwd)
	a.Logger.Info("planner: agent-turn forwarder installed")
}
