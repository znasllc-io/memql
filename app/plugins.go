package app

import (
	"context"
	"database/sql"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

// materializePlugins iterates the registrations made at init time via
// memql.RegisterPlugin / RegisterPluginForContract -- packs and
// self-registering core integrations -- and materializes each one with a live
// PluginContext. Each registrant's IntegrationProvider is then registered
// with the engine, making its capabilities callable from the DSL.
//
// Call order: after the engine is constructed and core integrations are
// wired (database/auth/identity self-register now, but cognition/
// agent/stt still go through explicit app wiring because they need
// deps outside the stable PluginContext surface).
//
// A factory returning an error is a fatal startup error: registrants
// that can run in degraded mode should return an instance that no-ops
// instead, so the app still boots.
func (a *App) materializePlugins() {
	plugins := memql.RegisteredPlugins()
	if len(plugins) == 0 {
		return
	}

	pctx := a.pluginContext()
	for _, p := range plugins {
		// A plugin bound to a DISABLED pack is not materialized (module-
		// registry design section 4.2): the pack's DSL half loaded
		// mounted-inert in phase 3, and this is the Go half of the same
		// switch. One honest line per skip; the module inventory reports
		// the state, so the log is a breadcrumb rather than the record.
		if domain, bound := memql.PackDomainForPlugin(p.Name); bound && a.packDisabled(domain) {
			a.Logger.Info("pack disabled by v1:platform:packState; factory not materialized",
				"pack", p.Name, "packDomain", domain)
			continue
		}
		// Reject a pack built against an incompatible Plugin SDK contract
		// version before materializing it -- a stale pack fails loudly here
		// instead of mis-binding against a contract it was not built for.
		if err := p.ValidateContract(); err != nil {
			a.fatal("pack contract incompatible", "pack", p.Name, "error", err)
		}
		prov, err := p.Factory(pctx)
		if err != nil {
			a.fatal("pack factory failed", "pack", p.Name, "error", err)
		}
		if prov == nil {
			// (nil, nil) is the documented "opt out" signal: the pack
			// is compiled in but its dependencies aren't satisfied in this
			// environment (e.g. GCS without a bucket configured). The
			// factory is expected to log its own warning if the opt-out
			// is worth reporting.
			a.Logger.Info("pack opted out", "pack", p.Name)
			continue
		}
		if err := a.engine.RegisterIntegration(prov); err != nil {
			a.fatal("pack registration failed", "pack", p.Name, "error", err)
		}
		a.Logger.Info("pack registered", "pack", p.Name)
	}

	// Wire the semantic (vector) AI-call cache (5.9) now that the engine,
	// provider registry, and database handle are all live. The embedding
	// provider + node_vectors-class store sit behind the primitive's narrow
	// interfaces. This is a no-op for every namespace by default (the
	// enablement registry ships empty), so it changes no AI behaviour until
	// a vetted classification namespace is enabled.
	a.engine.WireSemanticCache("", a.semanticCacheDBGetter())
}

// semanticCacheDBGetter resolves the underlying *sql.DB the semantic cache's
// pgvector store reads/writes. Lazy so it observes the live handle. Returns nil
// until the database component is up, which the store treats as fail-open.
func (a *App) semanticCacheDBGetter() func() *sql.DB {
	return func() *sql.DB {
		if a.db == nil {
			return nil
		}
		bdb := a.db.BunDB()
		if bdb == nil {
			return nil
		}
		return bdb.DB
	}
}

// pluginContext builds the narrow surface every registrant (pack or
// self-registering core integration) receives. All callbacks are lazy so
// packs observe live state even if they stash the context.
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
		// Staged-DATA visibility for the packs that hand-roll SQL
		// (epic memql#3974, task memql#3984). Lazy like every other callback
		// here, and deliberately a call THROUGH the engine rather than a
		// snapshot: the staged set changes when a concept is promoted or
		// trained, and a value captured at wiring time would answer with
		// whatever was true at boot forever after.
		ConceptDataIsStaged: func(conceptId string) bool {
			if a.engine == nil {
				return false
			}
			return a.engine.ConceptDataIsStaged(conceptId)
		},
		// Per-row authorization for the same hand-rolled reads (memql#4029).
		// The sibling of the callback above -- that one asks whether a
		// concept's data is visible to anyone yet, this one whether THIS
		// caller may see this row.
		//
		// Wired straight to the package-level function rather than through
		// the engine, because the gate resolves a tier from the concept
		// registry and holds no per-engine state; there is nothing for a
		// snapshot to go stale against. The caller identity it reads comes
		// from the ctx passed at each call, not from wiring time.
		AdmitSourceRow: memql.AdmitSourceRow,
		Providers:      a.engine.Providers(),
		Policies:       a.engine.Policies(),
		Agents:         a.engine.Agents(),
	}
}
