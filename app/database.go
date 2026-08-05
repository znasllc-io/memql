package app

import (
	"context"
	"fmt"

	memoryNodesDatabase "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/observe"
	"github.com/znasllc-io/memql/component/server"
	memqldsl "github.com/znasllc-io/memql/dsl"

	"github.com/uptrace/bun"
)

// databaseAndConcepts creates the database, loads and validates concepts,
// and sets up the concept seeder dependency.
func (a *App) databaseAndConcepts() {
	mnd, err := a.overrides.NewDatabase()
	if err != nil {
		a.fatal("failed to create database", "error", err)
	}
	a.db = mnd

	// Mount any product DSL delivered at runtime via MEMQL_DSL_PATH (a bundle
	// image's init-container populates that volume) BEFORE the first Tree()
	// walk below. This is what lets a product-agnostic engine image run a
	// product's DSL with no compiled-in product code (platform-consolidation,
	// #2472). No-op when MEMQL_DSL_PATH is unset (every carrier/engine node
	// that ships its DSL at compile time).
	memqldsl.MountRuntimeDomainsFromEnv(a.Logger)

	// Load concepts from the unified domain-first tree
	// (dsl/<domain>/concepts.memql) into the global registry. This is
	// the sole concept loader; the legacy version-directory walk
	// (LoadConcepts) was retired once the embedded concept FS emptied.
	if _, err := memql.LoadUnifiedConcepts(a.Logger); err != nil {
		a.fatal("failed to load unified concepts", "error", err)
	}

	a.registry = memoryNodesDatabase.DefaultRegistry()

	server.RegisterConceptsEndpoint(a.mux, a.registry)

	// The legacy `seed.memql` concept seeder used to be constructed here and
	// added as a Dependency. It is DELETED (memql#3067): it wrote through
	// concept.Create directly against the store, so it was the one write route
	// that skipped executeWrite and therefore every guard hooked there -- the
	// agentRole predefined lock, the RBAC base-role and rank guards, the
	// healing-override guard, the agent kind/actor-scope checks, and the
	// identity credential actor-scope guard.
	//
	// It was already dead: it ingested only files named `seed.memql`, from
	// database.Concepts -- a typed-EMPTY fstest.MapFS whose own comment said it
	// existed solely to feed this walk -- and the tree contains no such file.
	// The loop body never executed. The hazard was what happened when someone
	// added the first one: catalog rows written behind every lock, nothing
	// failing and nothing logged, with the guards still looking present.
	//
	// SeedMaterializer (component/memql) is the live seeding path and goes
	// through the guarded route. This seeder predates it -- it is original code
	// from the initial commit -- and was left behind rather than superseded on
	// purpose.
	a.Dependencies = append(a.Dependencies, a.db)

	// Readiness probe (#657): GET /readyz asserts critical schema presence so a
	// deploy gate can prove migrations actually applied WITHOUT DB credentials
	// (the staging DB is firewalled to in-cluster egress). The closure resolves
	// a.db lazily; a not-yet-connected DB reports not-ready instead of panicking.
	// Runtime counterpart to the #671 migrate gate.
	server.RegisterReadinessCheck("critical-schema", func(ctx context.Context) error {
		if a.db == nil {
			return fmt.Errorf("database not initialized")
		}
		return a.db.AssertCriticalSchema(ctx)
	})

	// Observe runtime: wire the TimescaleDB sink so instrumentation
	// calls via component/observe land in the code_invocation
	// hypertable. Closure captures a.db; resolution is lazy at Start
	// time so the sink doesn't depend on a particular db init order.
	provider := func() *bun.DB {
		if a.db == nil {
			return nil
		}
		return a.db.BunDB()
	}
	observeSink := observe.NewSinkComponent(a.Logger, provider, observe.TimescaleSinkOptions{Logger: a.Logger}, 0)
	a.Dependencies = append(a.Dependencies, observeSink)
}
