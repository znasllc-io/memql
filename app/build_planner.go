//go:build planner

package app

import "log/slog"

// Build constructs service dependencies for a planner node.
// Planner nodes handle Plan / Task lifecycle: subscribing to Plan
// transitions, dispatching the owning agent for "Allow on a
// permission card" execution, marking Plan terminal status. The
// integration phase is integrationsPlanner (not just
// integrationsCore) so the planner-specific subscription +
// AgentForwarder wiring lands on the planner binary.
func Build(serviceLogger *slog.Logger, version string, overrides Overrides) *App {
	a := newApp(serviceLogger, version, overrides)

	a.configAndAuth()
	a.databaseAndConcepts()
	a.engineAndBus()
	a.integrationsPlanner()
	a.transportMinimal()
	a.cluster()

	a.Dependencies = append(a.Dependencies, a.grpcServer)
	a.Dependencies = append(a.Dependencies, a.httpServer)

	return a
}
