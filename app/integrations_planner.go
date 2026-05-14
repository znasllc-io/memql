//go:build planner

package app

// integrationsPlanner registers integration providers for a planner
// node. Planner nodes own Plan / Task lifecycle: subscribing to
// Plan transitions, dispatching the owning agent when a Plan goes
// from awaitingFeedback -> running, marking Plan terminal status
// (succeeded with output / failed with errorMessage), enforcing
// token budgets, dispatching container-executor backed Tasks.
//
// Cognition has its own integration for routing + turn-taking; the
// two services share only the wire (the AgentForwarder protocol
// over NodeService.Stream) and the event bus.
func (a *App) integrationsPlanner() {
	a.integrationsCore()
	a.setupPlannerIntegration()
	a.Logger.Info("planner integration providers registered")
}
