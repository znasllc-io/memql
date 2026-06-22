//go:build !agent && !planner

package app

import (
	"context"

	"github.com/znasllc-io/memql/component/polyphon"
	"github.com/znasllc-io/memql/integrations/cognition"
	embeddingIntegration "github.com/znasllc-io/memql/integrations/embedding"
)

// setupCognitionIntegration creates and registers the cognition integration
// for AI response generation and conversation management.
// Compiled for cognition and standalone builds only.
func (a *App) setupCognitionIntegration() {
	var polyphonScoreEng *polyphon.ScoreEngine
	if a.polyphonScoreEngine != nil {
		polyphonScoreEng = a.polyphonScoreEngine.(*polyphon.ScoreEngine)
	}

	cognitionIntegration, err := cognition.NewCognitionIntegration(
		nil,
		cognition.WithEngine(&CognitionEngineAdapter{Engine: a.engine}),
		cognition.WithProviderRegistry(&CognitionProviderAdapter{Engine: a.engine}),
		cognition.WithEventBus(a.eventBus),
		cognition.WithScoreEngine(polyphonScoreEng),
		// Lazy DIRECT (non-pooled) *bun.DB getter for the dispatch gate's
		// Postgres advisory lock (znasllc-io/memql#1217). The gate holds a
		// session-scoped lock across a whole turn, so it MUST bypass the
		// transaction pooler -- DirectBunDB() returns the direct pool when
		// DIRECT_DSN is set, else falls back to the main pool, so behavior is
		// unchanged when unset (epic memql#1925). nil-safe (the gate fails safe
		// / no-ops when the DB isn't ready).
		cognition.WithDirectDBGetter(a.db.DirectBunDB),
		cognition.WithVariableResolver(func(ctx context.Context, name string) (string, error) {
			return a.engine.ResolveVariable(ctx, name)
		}),
	)
	if err != nil {
		a.fatal("failed to create cognition integration", "error", err, "component", cognition.ComponentName)
	}
	a.Logger.Info("cognition integration initialized for conversation management")

	// Wire embedding function for vector-based domain scoring. The embedding
	// integration is a plug-in -- look it up from the engine's integration
	// registry by name.
	if embProv := a.engine.Integrations().Provider("embedding"); embProv != nil {
		if embInteg, ok := embProv.(*embeddingIntegration.Integration); ok && embInteg != nil {
			cognitionIntegration.SetEmbedFunc(embInteg.Embed)
			a.Logger.Info("cognition: vector domain scoring enabled via embedding integration")
		}
	}

	// Inject the agent-turn forwarder so cognition can ship
	// AgentGenerateTurnMsg to an agent peer instead of generating the
	// reply locally. On cognition cluster binaries the forwarder is
	// created later in the cluster phase -- we stash the integration
	// on the app so cluster.go can wire the forwarder onto it then.
	// Single-binary (no tags) builds leave the forwarder nil and
	// cognition falls back to the local generate path.
	a.cognitionIntegration = cognitionIntegration
	if a.agentForwarder != nil {
		cognitionIntegration.SetAgentForwarder(a.agentForwarder)
		a.Logger.Info("cognition: agent-turn forwarder installed early")
	}

	if err := a.engine.RegisterIntegration(cognitionIntegration); err != nil {
		a.fatal("failed to register cognition integration provider", "error", err)
	}
	a.Logger.Info("cognition integration provider registered with engine")

	a.Dependencies = append(a.Dependencies, cognitionIntegration)
}
