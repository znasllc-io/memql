package main

import (
	"context"
	"fmt"
	"os"

	"github.com/znasllc-io/memql/app"
	memoryNodesDatabase "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/genesis"
	"github.com/znasllc-io/memql/core/common"
)

// runMigrateSubcommand applies pending DB migrations and exits 0 (#553,
// epic #549). It is the GATED PRE-DEPLOY migration step: run as a one-shot
// k8s Job BEFORE rolling the node mesh, instead of inline on identity boot,
// so a schema change is applied exactly once, up front, and never races
// old-version worker pods mid-rollout.
//
// Idempotent + concurrency-safe: the bun migrator takes a Postgres advisory
// lock and marks-applied-on-success, so a re-run (or two Jobs) is a no-op
// when the schema is already current.
//
// It starts the dependency chain in registration order up to AND INCLUDING
// the database (config -> memoryNodesDB) and stops there -- the database's
// Start() is what applies migrations (when MEMORY_NODES_DATABASE_MIGRATE_ON_START
// is true, which the Job sets). No engine, no transport, no mesh: the Job is
// not a node and must not bind ports or dial peers that aren't up yet.
func runMigrateSubcommand(args []string) int {
	for _, a := range args {
		switch a {
		case "-h", "--help", "help":
			fmt.Fprintln(os.Stderr, migrateUsage)
			return 0
		default:
			fmt.Fprintf(os.Stderr, "migrate: unexpected argument %q\n%s\n", a, migrateUsage)
			return 2
		}
	}

	// Mirror the server bootstrap's local /.env overlay so a dev run sees
	// the same DSN as the cluster (a no-op in the container, where /.env
	// is absent and the envelope/Job env already cover everything).
	if _, err := genesis.ApplyLocalOverride("."); err != nil {
		fmt.Fprintf(os.Stderr, "migrate: local .env override failed: %v\n", err)
	}

	logger := mustCreateCLILogger()
	application := app.Build(logger, resolveVersionFn(), app.Overrides{})

	var started []common.Dependency
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), app.DefaultRunShutdownTimeout)
		defer cancel()
		for i := len(started) - 1; i >= 0; i-- {
			started[i].Stop(stopCtx)
		}
	}()

	foundDB := false
	for _, d := range application.Dependencies {
		d.Start(context.Background())
		started = append(started, d)
		if d.ComponentName() == memoryNodesDatabase.ComponentName {
			foundDB = true
			break
		}
	}
	if !foundDB {
		fmt.Fprintln(os.Stderr, "migrate: database dependency not present in this build -- nothing to migrate")
		return 1
	}

	logger.Info("migrate: database migrations applied (or already current)")
	return 0
}

const migrateUsage = `usage: memql migrate

Apply pending memQL DB migrations and exit (gated pre-deploy step, memql#553).

Requires the database environment (MEMORY_NODES_DATABASE_DSN and
MEMORY_NODES_DATABASE_MIGRATE_ON_START=true). Idempotent: a no-op when the
schema is already current. Run as a k8s Job BEFORE rolling the node mesh.`
