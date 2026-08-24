package shopify

import (
	"context"
	"database/sql"
	"log/slog"

	"github.com/znasllc-io/memql/component/memql"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
)

// plugin.go -- registration.
//
// The plug-in ALWAYS registers, even with no store configured. The
// inbound dispatcher automation loads unconditionally, so a missing
// executor would fail every inbound row from every source, not just
// Shopify's. With no store row the capabilities no-op; they never
// invent.
//
// The connector NAME is declared in connector.go's init() rather than
// here, because the engine's boot check resolves @origin("shopify")
// before integrations are wired at all. This factory BINDS the
// implementation, which is the later half of the same registration.
func init() {
	// The name is a LITERAL, not ConnectorName, and that is not an
	// oversight. The module-taxonomy gate (module_taxonomy_test.go)
	// finds every registration by scanning the SOURCE rather than the
	// registry -- it has to, because a registry populated by init() only
	// shows the plugins the scanning binary imports -- and its pattern
	// reads a string literal. Passing the constant makes this
	// registration invisible to the gate, which then reports the
	// classification as stale. TestPluginNameMatchesConnectorName keeps
	// the two in step.
	memql.RegisterPlugin("shopify", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		logger := pctx.Logger
		if logger == nil {
			logger = slog.New(slog.DiscardHandler)
		}
		registry := NewStoreRegistry(pctx.Engine, pctx.ResolveSystemSecret)
		connector := NewConnector(pctx.Engine, logger, registry, NewAdminClient())
		if pctx.BunDB != nil {
			connector.WithDatabase(func() *sql.DB {
				db := pctx.BunDB()
				if db == nil {
					return nil
				}
				return db.DB
			})
		}
		// A bind failure is LOGGED, not returned. The only ways it can
		// fail are a name nothing declared (impossible here -- the init
		// above it declares one) and a second bind under one name, which
		// happens when a process materializes plug-ins twice. Neither is
		// a reason to take down a node, and the mirror write guard does
		// not depend on the binding: it matches the ACTOR's name against
		// the concept's declaration, so writes stay gated either way.
		if err := memqlsync.Bind(connector); err != nil {
			logger.Warn("shopify connector was not bound; its capabilities still run",
				"component", "integrations.shopify", "error", err)
		}
		return NewIntegration(connector), nil
	})
}

// Bootstrap runs the connector's start-up work: seed the first store from
// the environment if there is one and no store exists, then bring every
// store's subscriptions in line.
//
// Called from the node's boot sequence rather than from the plug-in
// factory, because both steps talk to the network and to the database,
// and a factory that did either would make plug-in registration fail on a
// store that happens to be unreachable.
func Bootstrap(ctx context.Context, i *Integration, logger *slog.Logger) {
	if i == nil || i.connector == nil {
		return
	}
	c := i.connector
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	if storeID, err := SeedStoreFromEnv(ctx, c.engine, c.stores, SeedConfigFromEnv()); err != nil {
		logger.Warn("shopify: could not seed the first store from the environment", "error", err)
	} else if storeID != "" {
		logger.Info("shopify: seeded the first store from the environment", "store", storeID)
	}
	if err := c.EnsureSubscriptions(ctx); err != nil {
		// Not fatal. A store that is unreachable at boot is reconciled by
		// the runtime's daily pass, and refusing to start would turn a
		// Shopify outage into a MemQL one.
		logger.Warn("shopify: subscription reconcile at boot failed", "error", err)
	}
}
