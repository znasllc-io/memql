package memql

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
// This package joined the lane in memql#3030 -- its 3 DB assertions had never run in CI.
//
// When no DB is reachable EnsureSchema is a no-op and the individual tests
// self-skip, so a developer without Postgres sees exactly what they did before.
func TestMain(m *testing.M) {
	if _, err := dbtest.EnsureSchema(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "dbtest.EnsureSchema: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
