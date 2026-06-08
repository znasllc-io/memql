package app

import (
	"context"
	"fmt"

	memoryNodesDatabase "github.com/znasllc-io/memql/component/database/memory-nodes"
	conceptSeeder "github.com/znasllc-io/memql/component/database/memory-nodes/seeder"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/observe"
	"github.com/znasllc-io/memql/component/server"

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

	// Load concepts from the unified domain-first tree
	// (dsl/<domain>/concepts.memql) into the global registry. This is
	// the sole concept loader; the legacy version-directory walk
	// (LoadConcepts) was retired once the embedded concept FS emptied.
	if _, err := memql.LoadUnifiedConcepts(a.Logger); err != nil {
		a.fatal("failed to load unified concepts", "error", err)
	}

	a.registry = memoryNodesDatabase.DefaultRegistry()

	server.RegisterConceptsEndpoint(a.mux, a.registry)

	// Pass nil for logger - the seeder creates its own via common.NewLogger
	seedRunner, err := conceptSeeder.NewRunner(a.db, a.registry, nil)
	if err != nil {
		a.fatal("failed to initialize concept seeder", "error", err)
	}
	conceptSeedDep := conceptSeeder.NewDependency(seedRunner, nil)

	a.Dependencies = append(a.Dependencies, a.db)
	if conceptSeedDep != nil {
		a.Dependencies = append(a.Dependencies, conceptSeedDep)
	}

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
