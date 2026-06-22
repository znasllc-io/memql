package app

import (
	"context"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

// materializePlugins iterates plug-ins registered at init time via
// memql.RegisterPlugin and materializes each one with a live PluginContext.
// Each plug-in's IntegrationProvider is then registered with the engine,
// making its capabilities callable from the DSL.
//
// Call order: after the engine is constructed and core integrations are
// wired (database/auth/identity are themselves plug-ins now, but cognition/
// agent/stt still go through explicit app wiring because they need
// deps outside the stable PluginContext surface).
//
// A plug-in factory returning an error is a fatal startup error: plug-ins
// that can run in degraded mode should return an instance that no-ops
// instead, so the app still boots.
func (a *App) materializePlugins() {
	plugins := memql.RegisteredPlugins()
	if len(plugins) == 0 {
		return
	}

	pctx := a.pluginContext()
	for _, p := range plugins {
		// Reject a pack built against an incompatible Plugin SDK contract
		// version before materializing it -- a stale pack fails loudly here
		// instead of mis-binding against a contract it was not built for.
		if err := p.ValidateContract(); err != nil {
			a.fatal("plug-in contract incompatible", "plugin", p.Name, "error", err)
		}
		prov, err := p.Factory(pctx)
		if err != nil {
			a.fatal("plug-in factory failed", "plugin", p.Name, "error", err)
		}
		if prov == nil {
			// (nil, nil) is the documented "opt out" signal: the plug-in
			// is compiled in but its dependencies aren't satisfied in this
			// environment (e.g. GCS without a bucket configured). The
			// factory is expected to log its own warning if the opt-out
			// is worth reporting.
			a.Logger.Info("plug-in opted out", "name", p.Name)
			continue
		}
		if err := a.engine.RegisterIntegration(prov); err != nil {
			a.fatal("plug-in registration failed", "plugin", p.Name, "error", err)
		}
		a.Logger.Info("plug-in registered", "name", p.Name)
	}
}

// pluginContext builds the narrow surface plug-ins receive. All callbacks
// are lazy so plug-ins observe live state even if they stash the context.
func (a *App) pluginContext() memql.PluginContext {
	return memql.PluginContext{
		Logger: a.Logger,
		Engine: a.engine,
		BunDB: func() *bun.DB {
			if a.db == nil {
				return nil
			}
			return a.db.BunDB()
		},
		DirectBunDB: func() *bun.DB {
			if a.db == nil {
				return nil
			}
			return a.db.DirectBunDB()
		},
		VisionProvider: func() common.VisionAIProvider {
			if a.engine == nil {
				return nil
			}
			return a.engine.VisionProvider()
		},
		EmbeddingProviderByName: func(name string) (memql.EmbeddingAIProvider, error) {
			return a.engine.Providers().EmbeddingProvider(name)
		},
		ResolvePartitionFromContext: func(ctx context.Context) string {
			return a.engine.ResolvePartitionFromContext(ctx)
		},
		ResolveVariable: func(ctx context.Context, name string) (string, error) {
			return a.engine.ResolveVariable(ctx, name)
		},
		ResolveSystemVariable: func(ctx context.Context, name string) (string, error) {
			return a.engine.ResolveSystemVariable(ctx, name)
		},
		ResolveSecret: func(ctx context.Context, name string) (string, error) {
			return a.engine.ResolveSecret(ctx, name)
		},
		ResolveSystemSecret: func(ctx context.Context, name string) (string, error) {
			return a.engine.ResolveSystemSecret(ctx, name)
		},
		Providers: a.engine.Providers(),
		Policies:  a.engine.Policies(),
		Agents:    a.engine.Agents(),
	}
}
