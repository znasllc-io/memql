package cognition

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/znasllc-io/memql/component/database/dbtest"
)

// TestMain guarantees the shared test schema is fully migrated before any
// Postgres-gated test in this package runs, serialized against the other
// db-gated packages CI runs in the same lane.
//
// Required by the "db-tests" lane, not optional bookkeeping: the lane runs
// per-package test binaries as PARALLEL PROCESSES against ONE shared database,
// so without this the suites race each other's migration and fail
// intermittently with `relation "MemoryNodes" does not exist` (memql#2551).
// This package joined the lane in memql#3030 -- its 9 advisory-lock exactly-once gates had never run in CI.
//
// When no DB is reachable EnsureSchema is a no-op and the individual tests
// self-skip, so a developer without Postgres sees exactly what they did before.
//
// A developer WITH a reachable database does see one new thing, and it is worth
// stating rather than implying otherwise: `go test ./...` now MIGRATES that
// database from this package, which it never did before memql#3030. The tests
// themselves already wrote and deleted rows when a DB was reachable -- lane
// membership only ever governed CI -- so the migration is the change, not the
// writes. Point MEMQL_DATABASE_DSN at a scratch database if that matters to
// you; the lane sets MEMQL_REQUIRE_DB=1 and CI provisions its own.
func TestMain(m *testing.M) {
	if _, err := dbtest.EnsureSchema(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "dbtest.EnsureSchema: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
