package app

import (
	"context"
	"fmt"
	"strings"

	memoryNodesDatabase "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/observe"
	"github.com/znasllc-io/memql/component/server"
	memqldsl "github.com/znasllc-io/memql/dsl"

	"github.com/uptrace/bun"
)

// databaseAndConcepts creates the database and loads and validates concepts.
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
	// CONCEPT SKIPS REFUSE BOOT, under the same break-glass as every other
	// construct (memql#3622).
	//
	// This used to call LoadUnifiedConcepts, which DISCARDS the skip list --
	// a malformed concept produced a Warn and a nil error, so a product bundle
	// with one mistyped field booted green on every node and every query,
	// mutation and shape bound to that concept failed only at runtime. That is
	// exactly the pack-rot failure engine_bootstrap.go's strict gate exists to
	// prevent, and concepts were the one construct outside it: shapes already
	// fold their drops into the LoadReport and do gate boot.
	//
	// Concepts load in this phase, before the engine builds that report, so
	// the check lives here rather than there. The asymmetry was backwards --
	// a concept is the base of the dependency tree, so everything else
	// resolves against it.
	loaded, skips, err := memql.LoadUnifiedConceptsWithSkips(a.Logger)
	if err != nil {
		a.fatal("failed to load unified concepts", "error", err)
	}
	if len(skips) > 0 {
		detail := make([]string, 0, len(skips))
		for _, s := range skips {
			detail = append(detail, "  - "+s.File+": "+s.String())
		}
		joined := strings.Join(detail, "\n")
		if !memql.DSLAllowSkips() {
			a.fatal("strict DSL boot refused: malformed concept(s)",
				"skipped", len(skips),
				"loaded", loaded,
				"breakGlass", memql.AllowSkipsEnvVar+"=1",
				"detail", "\n"+joined)
		}
		a.Logger.Error(memql.AllowSkipsEnvVar+" set: booting despite dropped concept(s) (operator break-glass)",
			"skipped", len(skips), "loaded", loaded, "detail", "\n"+joined)
	}

	a.registry = memoryNodesDatabase.DefaultRegistry()

	server.RegisterConceptsEndpoint(a.mux, a.registry)

	// The legacy `seed.memql` sidecar seeder used to be constructed and run
	// here. It is gone (memql#3176): it wrote via concept.Create directly
	// against the store, bypassing executeWrite and therefore every write
	// guard hooked there. Authored seeds go through the SeedMaterializer,
	// which takes the guarded path. See the guard in
	// unguarded_concept_create_test.go at the repo root.
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
