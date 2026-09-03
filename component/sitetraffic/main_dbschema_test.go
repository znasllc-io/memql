package sitetraffic

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
// This package joined the lane in memql#4906, which is also the release that
// adds the `edge_request` hypertable and its two aggregates -- so on a
// developer's existing database the FIRST run here is the one that creates
// them, and a suite that read the aggregate before the migration landed would
// fail on a missing relation rather than on anything it was testing.
//
// When no DB is reachable EnsureSchema is a no-op and the individual tests
// self-skip through dbtest.Unreachable, so a developer without Postgres sees
// exactly what they did before. The lane sets MEMQL_REQUIRE_DB=1, which turns
// that skip into a failure.
func TestMain(m *testing.M) {
	if _, err := dbtest.EnsureSchema(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "dbtest.EnsureSchema: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
