package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/znasllc-io/memql/app"
	"github.com/znasllc-io/memql/component/database"
	memoryNodesDatabase "github.com/znasllc-io/memql/component/database/memory-nodes"
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

	// Mirror the server bootstrap: applySubcommandEnv overlays the repo-root
	// .env, bridges legacy env-var names via envregistry.ApplyLegacyEnvAliases,
	// and applies domain derivations -- so a migrate run sees the same DSN as
	// the cluster (#751 -- subcommands run before main()'s autoload).
	if err := applySubcommandEnv("migrate"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
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

	var dbDep common.Dependency
	for _, d := range application.Dependencies {
		d.Start(context.Background())
		started = append(started, d)
		if d.ComponentName() == memoryNodesDatabase.ComponentName {
			dbDep = d
			break
		}
	}
	if dbDep == nil {
		fmt.Fprintln(os.Stderr, "migrate: database dependency not present in this build -- nothing to migrate")
		return 1
	}

	// The database logs migrate failures but does not abort startup, so a
	// failed migration would otherwise leave this Job exiting 0 and the deploy
	// proceeding onto a broken schema behind a green gate (#671). Consult the
	// recorded error and exit non-zero so make deploy's gate aborts (and
	// auto-rolls-back) instead -- after giving a migration that deferred the
	// rest of its work the attempts it needs (awaitMigrations).
	if db, ok := dbDep.(restartableDatabase); ok {
		if err := awaitMigrations(context.Background(), logger, db, migrateDeferredAttempts, migrateDeferredPause); err != nil {
			fmt.Fprintf(os.Stderr, "migrate: migration failed: %v\n", err)
			return 1
		}
	}

	logger.Info("migrate: database migrations applied (or already current)")
	return 0
}

// migrateDeferredAttempts bounds the attempts the subcommand runs while a
// migration defers the rest of its work. Each one makes progress, so this is a
// backstop against a migration that never finishes rather than a budget one is
// expected to approach: at the default MEMORY_NODES_DATABASE_MIGRATION_TIMEOUT_MS
// it is ten minutes of attempts.
const migrateDeferredAttempts = 20

// migrateDeferredPause separates those attempts, so a migration that defers at
// once -- an attempt too short for even one of its steps -- cannot spin.
const migrateDeferredPause = time.Second

// restartableDatabase is the database dependency as awaitMigrations drives it:
// every dependency starts and stops, and the database also reports how its
// last migration attempt went.
type restartableDatabase interface {
	Start(ctx context.Context)
	Stop(ctx context.Context)
	MigrationError() error
}

// awaitMigrations returns the migration outcome, stopping and starting the
// database for another attempt while a migration has deferred the rest of its
// work to the next one (database.ErrMigrationDeferred). That error means the
// migration stopped with its steps committed and nothing left running -- the
// readiness history collapse does it when its walk outlasts one attempt
// (memql#5604) -- and a node answers it on its monitor tick. This subcommand
// has no tick, and exiting non-zero would leave the migrate Job's backoffLimit
// to decide whether a long walk finishes, with `make up` failing at its
// migrate step while the walk was making progress. Any other outcome returns
// at once.
func awaitMigrations(ctx context.Context, logger *slog.Logger, db restartableDatabase, attempts int, pause time.Duration) error {
	for attempt := 1; ; attempt++ {
		err := db.MigrationError()
		if err == nil || !errors.Is(err, database.ErrMigrationDeferred) {
			return err
		}
		if attempt >= attempts {
			return fmt.Errorf("a migration was still deferring its work after %d attempts: %w", attempts, err)
		}
		logger.Info("migrate: a migration deferred the rest of its work; starting the next attempt",
			"attempt", attempt+1, "of", attempts, "deferral", err.Error())
		select {
		case <-ctx.Done():
			return err
		case <-time.After(pause):
		}
		db.Stop(ctx)
		db.Start(ctx)
	}
}

const migrateUsage = `usage: memql migrate

Apply pending MemQL DB migrations and exit (gated pre-deploy step, memql#553).

Requires the database environment (MEMQL_DATABASE_DSN and
MEMORY_NODES_DATABASE_MIGRATE_ON_START=true). Idempotent: a no-op when the
schema is already current. Run as a k8s Job BEFORE rolling the node mesh.`
