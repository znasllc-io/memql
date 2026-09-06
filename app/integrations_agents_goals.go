package app

// integrations_agents_goals.go -- joins the agents plug-in to the work spine's
// goal opener (memql#5048).
//
// UNTAGGED, and that is the whole point. The `agents` plug-in is CORE: it
// registers on every node type, so `agent()` and the `produceArtifact` tool
// are reachable wherever a DSL call or a model's tool loop runs -- and
// produceArtifact is called from an Assistant's tool loop, which is a bff.
// Wiring this under the agent tag would leave both surfaces refusing on every
// node that actually serves them.
//
// WITHOUT THIS CALL BOTH SURFACES REFUSE. That refusal is deliberate (see
// goals.go): the failure these two spent their whole lives having was
// returning an id for work nothing executed, and an ack for a goal that was
// never opened is that same failure with a new name. So the seam refuses
// loudly rather than acking quietly, and this is the line that keeps it from
// having to.

import (
	"context"

	"github.com/znasllc-io/memql/integrations/agents"
	workspine "github.com/znasllc-io/memql/integrations/work"
)

func (a *App) wireAgentWorkGoals() {
	if a.engine == nil {
		return
	}
	provider := a.engine.IntegrationByName("agents")
	if provider == nil {
		// Not every node materializes it (a test engine, a stripped build).
		// Debug rather than warn: on a node with no agents plug-in there is
		// no surface to be broken.
		a.Logger.Debug("agent work goals not wired: the agents plug-in did not materialize", "component", "agents")
		return
	}
	integ, ok := provider.(*agents.Integration)
	if !ok || integ == nil {
		a.Logger.Warn("agent work goals not wired: the agents provider is not the expected type", "component", "agents")
		return
	}
	work := a.lookupWorkIntegration()
	if work == nil {
		a.Logger.Warn("agent work goals not wired: the work integration did not materialize, so agent() and produceArtifact will refuse on this node",
			"component", "agents")
		return
	}
	integ.SetWorkGoals(&agentWorkGoals{work: work})
	a.Logger.Info("agent work goals wired: agent() and produceArtifact open a work goal", "component", "agents")
}

// agentWorkGoals adapts integrations/work onto the seam integrations/agents
// declares.
//
// The two DirectGoal types are deliberately separate declarations rather than
// one shared import: integrations/agents must not depend on integrations/work.
// That is not fastidiousness about the import graph -- integrations/work is
// kept small and holds the ONE call-origin stamping site in the tree, and the
// allowlist that permits it is per PACKAGE. Widening what may reach it starts
// with widening what imports it.
//
// So the conversion lives here, in app/, which already knows about both. It is
// six fields, and it is the whole cost of that separation.
type agentWorkGoals struct {
	work *workspine.Integration
}

func (a *agentWorkGoals) OpenDirectGoal(ctx context.Context, g agents.DirectGoal) (string, string, error) {
	return a.work.OpenDirectGoal(ctx, workspine.DirectGoal{
		OwnerUserId:    g.OwnerUserId,
		Statement:      g.Statement,
		AutomationName: g.AutomationName,
		Input:          g.Input,
		RequestedVia:   g.RequestedVia,
		TriggeredBy:    g.TriggeredBy,
	})
}
