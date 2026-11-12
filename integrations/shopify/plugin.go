package shopify

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"

	"github.com/znasllc-io/memql/component/memql"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
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
		// The privacy jobs run their statements on the engine's maintenance
		// session and on nothing else (compliance.go, privacySession). A node
		// that offers none leaves it unset, and a job then fails saying so.
		if pctx.OpenMaintenanceSession != nil {
			connector.WithMaintenanceSession(pctx.OpenMaintenanceSession)
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
// the environment if there is one and no store exists, bring every store's
// subscriptions in line, then queue the backfills of the domains no backfill
// ever reached.
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
	c.queueUnstartedBackfills(ctx)
}

// queueUnstartedBackfills is how a store connected before backfills were
// queued gets its past: for each store that ingests, every bulk domain of its
// connection that was never backfilled is queued for the continuing pass
// (queueUnstartedBackfillsOf). A store that does not ingest is left out; a
// paused one gets the same pass when it resumes (resumeStore).
func (c *Connector) queueUnstartedBackfills(ctx context.Context) {
	stores, err := c.stores.Stores(ctx)
	if err != nil {
		c.logger.Warn("shopify: could not list the stores at boot; their backfills are queued at the next", "error", err.Error())
		return
	}
	for _, store := range stores {
		if store.Ingests() {
			// Each domain that could not be queued is logged where it failed;
			// the next boot, or the store's next resume, queues it.
			_ = c.queueUnstartedBackfillsOf(ctx, store)
		}
	}
}

// queueUnstartedBackfillsOf queues, for one store, every bulk domain of its
// connection that was never backfilled. The boot pass asks it of every store
// that ingests, and a resume of the store that resumed. A domain whose row
// could not be read or whose backfill could not be queued is logged and does
// not stop the rest; the error says how many were left, so a caller that can
// ask again (the store watch) knows to.
//
// Never backfilled is no row of that store's own, OR a row no backfill ever
// wrote: no status or `none`, no cursor, and no refusal on it. A sweep or a
// pause can write a domain's row before this pass reaches it, and "no row"
// alone would leave that store without its past for good. A row queued,
// running or failed, complete under the rule below, or carrying the origin's
// refusal, is left alone, so a second boot, a second node and a second resume
// queue nothing more.
//
// It also queues, once, a domain READ BEFORE EVERY WRITE CARRIED THE ORIGIN'S
// VERSION (readBeforeOriginVersions): complete, from a backfill an earlier
// build began. Such a backfill stamped each bulk line with the time it was
// streamed, so a snapshot older than a delivery could replace what the
// delivery recorded, and nothing corrects it until the origin changes that
// record again. The queue reads the domain again from the start; the backfill
// it starts completes with a cursor naming the rule, so it is not queued
// again. A backfill RUNNING under the earlier rule is left alone here -- the
// queue never touches a running one -- and begins again by itself, under the
// rule, at its next step (BackfillStore).
//
// When it queues the orders under a grant without read_all_orders, it stamps
// ordersFrom as a connect does, if the store has none.
//
// Each domain's row is read BY ID (memqlsync.StateID), as store health reads
// it, never by walking syncStatesAll: those rows are the append-only history.
func (c *Connector) queueUnstartedBackfillsOf(ctx context.Context, store Store) error {
	left := 0
	for _, concept := range generated.ApplyOrder {
		spec := generated.Types[concept]
		if !bulkDomain(spec) || !domainInConnection(store, spec) {
			continue
		}
		stateID := memqlsync.StateID(generated.ConceptID(concept), ConnectorName, string(memqlsync.DirectionInbound), store.ID)
		res, err := c.engine.Execute(operatorContext(ctx), renderCall("syncStateById", map[string]any{"stateId": stateID}))
		if err != nil {
			c.logger.Warn("shopify: could not read a domain's sync state; its backfill is not queued", "store", store.ID, "concept", concept, "error", err.Error())
			left++
			continue
		}
		if rows := memql.MaterializeRows(res); len(rows) > 0 && mapString(rows[0], "conceptId") != "" &&
			!neverBackfilled(rows[0]) && !readBeforeOriginVersions(rows[0]) {
			continue
		}
		if err := c.queueBackfill(ctx, store.ID, concept, false); err != nil {
			c.logger.Warn("shopify: could not queue a backfill", "store", store.ID, "concept", concept, "error", err.Error())
			left++
			continue
		}
		if concept == "order" {
			c.stampOrdersFrom(ctx, store, false)
		}
	}
	if left > 0 {
		return fmt.Errorf("shopify: %d of the domains of store %q could not be read or queued for their backfill", left, store.ID)
	}
	return nil
}

// readBeforeOriginVersions reports whether a domain's sync-state row records a
// complete backfill that began before originVersionRule: its cursor does not
// carry the rule (backfillReadUnderRule).
func readBeforeOriginVersions(row map[string]any) bool {
	return mapString(row, "backfillStatus") == "complete" && !backfillReadUnderRule(mapString(row, "backfillCursor"))
}

// neverBackfilled reports whether a domain's sync-state row is one no backfill
// ever wrote: no status or `none`, no cursor, and no refusal of the origin's
// recorded on it (a refused domain is queued again by its first clean sweep,
// not at boot).
func neverBackfilled(row map[string]any) bool {
	switch mapString(row, "backfillStatus") {
	case "", "none":
	default:
		return false
	}
	if mapString(row, "backfillCursor") != "" {
		return false
	}
	lastError := mapString(row, "lastError")
	return !strings.HasPrefix(lastError, memqlsync.ErrNotGranted.Error()) &&
		!strings.HasPrefix(lastError, memqlsync.ErrNotRequested.Error())
}
