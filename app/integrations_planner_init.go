//go:build planner

package app

import (
	"context"

	"github.com/znasllc-io/memql/integrations/planner"
)

// setupPlannerIntegration creates and registers the planner
// integration on a planner-tagged binary. The integration owns
// Plan / Task lifecycle: subscribes to graph.node.updated events
// for v1:planner:plan, dispatches the owning agent when a Plan
// transitions to running, marks the Plan terminal status when the
// agent returns.
//
// CognitionEngineAdapter satisfies the planner.Engine interface
// (single-method `Execute(ctx, query) (any, error)`); reusing the
// adapter avoids duplicating the engine-shape conversion logic
// that every node-type integration needs. The cognition naming on
// the adapter type is historical -- it pre-dated the planner
// integration. Renaming it is a follow-up cleanup; for now both
// integrations are happy with the same shim.
//
// AgentForwarder is installed separately via cluster.go after the
// SIForwardRouter is constructed. Without a forwarder, plan-
// execution dispatches log a warning and skip; that's the
// intended behavior on dev / single-binary builds where the
// forwarder doesn't exist.
func (a *App) setupPlannerIntegration() {
	plannerIntegration, err := planner.NewPlannerIntegration(
		context.Background(),
		planner.WithEngine(&CognitionEngineAdapter{Engine: a.engine}),
		planner.WithEventBus(a.eventBus),
		planner.WithLogger(a.Logger),
		// Per-account task-concurrency admission control (epic memql#902 / #904)
		// needs a *bun.DB for per-account advisory locks + running-Plan counts.
		// a.db.BunDB is the same lazy getter the cron leader uses; nil-safe.
		planner.WithDBGetter(a.db.BunDB),
		// Cross-replica plan-execution claim (memql#1363): the plan-approved
		// event reaches every planner replica; the shared cluster guard's
		// DB-PK claim makes exactly one replica dispatch the agent turn.
		planner.WithClusterClaimer(a.clusterGuard),
	)
	if err != nil {
		a.fatal("failed to create planner integration", "error", err, "component", planner.ComponentName)
	}

	// Stash on the app so cluster.go can wire the agent forwarder
	// onto it after the SIForwardRouter resolves.
	a.plannerIntegration = plannerIntegration
	if a.agentForwarder != nil {
		plannerIntegration.SetAgentForwarder(a.agentForwarder)
		a.Logger.Info("planner: agent-turn forwarder installed early")
	}

	a.Dependencies = append(a.Dependencies, plannerIntegration)
	a.Logger.Info("planner integration registered")
}
