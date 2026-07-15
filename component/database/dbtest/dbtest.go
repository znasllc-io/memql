// Package dbtest provides shared setup for the Postgres-gated
// ("*_db_test.go") suites that CI runs together in the db-tests lane
// (.github/workflows/ci.yml) against ONE shared database.
//
// Those suites compile into separate per-package test binaries that `go
// test` runs as parallel processes, all pointed at the same
// MEMQL_DATABASE_DSN. Only one of them (examples/referencepack) ever migrated
// the blank service DB; the others (component/memql, component/automations/
// steps) opened a connection and read/wrote the MemoryNodes table assuming
// the schema already existed. With no coordination the two classes raced:
// when a reader reached its first DB op before referencepack's migration
// finished, it failed with `relation "MemoryNodes" does not exist`, or -- if
// the initial CREATE had landed but a later migration had not -- a
// half-applied `column ... does not exist`. That intermittently red the
// required db-tests lane and poisoned the merge queue (memql#2551).
//
// EnsureSchema closes the gap: every db-gated package calls it from TestMain,
// so the schema is fully migrated before any test in any package touches it.
// A blocking, cross-process advisory lock serializes the migration so the
// first process migrates while the rest wait, then observe the applied
// migrations and return -- the non-idempotent (column-rename) migrations can
// never run concurrently.
package dbtest

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// defaultDSN mirrors the fallback each *_db_test.go uses when
// MEMQL_DATABASE_DSN is unset, so the ensure step and the tests it guards
// target the same database.
const defaultDSN = "postgres://memql:memql_local_dev@localhost:5432/memql?sslmode=disable"

// schemaLockKey is a dedicated, fixed 64-bit advisory-lock id that serializes
// schema migration across the parallel db-gated test processes. Distinct from
// the cron-leader / reconciler lock ids so the leases are independent
// (memql#2551).
const schemaLockKey int64 = 7756010113207025510

// EnsureSchema migrates the shared test database to the current schema,
// serialized across processes so the parallel db-gated package test binaries
// never race on a half-created schema (memql#2551). It is idempotent: the
// first process to acquire the advisory lock migrates; every other process
// blocks on the lock, then observes the applied migrations and returns.
//
// It reports reachable=false (err=nil) when no Postgres is reachable, so the
// caller can proceed to m.Run() and let the individual tests self-skip --
// preserving the green-by-skip behaviour on a DB-less CI lane.
func EnsureSchema(ctx context.Context) (reachable bool, err error) {
	dsn := strings.TrimSpace(os.Getenv("MEMQL_DATABASE_DSN"))
	if dsn == "" {
		dsn = defaultDSN
		// Align NewMemoryNodesDatabase (which reads the env) with the DSN the
		// tests fall back to, so both target the same database.
		_ = os.Setenv("MEMQL_DATABASE_DSN", dsn)
	}

	lockDB := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	defer func() { _ = lockDB.Close() }()

	pingCtx, cancelPing := context.WithTimeout(ctx, 5*time.Second)
	defer cancelPing()
	if perr := lockDB.PingContext(pingCtx); perr != nil {
		// No Postgres reachable: leave migration to nobody and let the
		// individual tests skip (they each ping + Skipf).
		return false, nil
	}

	// pg_advisory_lock is session-scoped, so the lock and its release must run
	// on the same pinned backend. Take a dedicated connection, matching the
	// cron-leader / reconciler advisory-lock idiom.
	conn, err := lockDB.DB.Conn(ctx)
	if err != nil {
		return true, fmt.Errorf("dbtest: acquire lock connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	// BLOCKING lock: a sibling process that reaches here while another is
	// migrating waits rather than proceeding on an incomplete schema.
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", schemaLockKey); err != nil {
		return true, fmt.Errorf("dbtest: pg_advisory_lock: %w", err)
	}
	defer func() {
		uctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, _ = conn.ExecContext(uctx, "SELECT pg_advisory_unlock($1)", schemaLockKey)
		cancel()
	}()

	// Migrate via the production lifecycle. Under the advisory lock this is the
	// ONLY migration touching the DB, so the non-idempotent (rename)
	// migrations never run concurrently; a sibling that already applied them
	// leaves bun_migrations recording the fact, and this call is a no-op.
	mnd, err := memoryNodes.NewMemoryNodesDatabase()
	if err != nil {
		return true, fmt.Errorf("dbtest: NewMemoryNodesDatabase: %w", err)
	}
	mnd.Start(ctx)
	select {
	case <-mnd.Ready():
	case <-time.After(90 * time.Second):
		mnd.Stop(context.Background())
		return true, fmt.Errorf("dbtest: shared test database did not become ready within 90s")
	}
	migErr := mnd.MigrationError()
	mnd.Stop(context.Background())
	if migErr != nil {
		return true, fmt.Errorf("dbtest: migrate shared test schema: %w", migErr)
	}
	return true, nil
}
