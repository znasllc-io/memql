package app

import (
	memoryNodesDatabase "github.com/visionarys-io/memql/component/database/memory-nodes"
	conceptSeeder "github.com/visionarys-io/memql/component/database/memory-nodes/seeder"
	"github.com/visionarys-io/memql/component/memql"
	"github.com/visionarys-io/memql/component/node"
	"github.com/visionarys-io/memql/component/server"
)

// databaseAndConcepts creates the database, loads and validates concepts,
// and sets up the concept seeder dependency.
func (a *App) databaseAndConcepts() {
	mnd, err := a.overrides.NewDatabase()
	if err != nil {
		a.fatal("failed to create database", "error", err)
	}
	a.db = mnd

	if err := memoryNodesDatabase.LoadConcepts(a.Logger, string(node.CompiledNodeType())); err != nil {
		a.fatal("failed to load memory node concepts", "error", err)
	}

	// Pass 2 of the DSL restructure migration: load concepts from
	// the unified domain-first tree (dsl/<domain>/*.memql) and merge
	// them into the same registry. During the transitional state
	// both loaders run; their concept IDs assemble identically so
	// the duplicate registration is a no-op. When the legacy tree
	// is retired (Pass 3), the LoadConcepts call above goes away.
	if _, err := memql.LoadUnifiedConcepts(a.Logger, string(node.CompiledNodeType())); err != nil {
		a.Logger.Warn("unified concept loader returned diagnostics; legacy loader covers the gap",
			"component", "app.database",
			"error", err)
	}

	a.registry = memoryNodesDatabase.DefaultRegistry()

	// Note: concept constants validation (ValidateConceptConstants) is
	// skipped. With @visibility filtering, the registry intentionally
	// contains a subset of all concepts — missing concepts are expected,
	// not drift. The concepts are embedded in the binary and can't
	// diverge from constants at runtime.

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
}
