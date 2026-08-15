package edge_test

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
// This package joined the lane in memql#3768, when the cross-schema site
// promote arrived with the round-trip assertions that are the whole
// justification for choosing two schemas over two databases.
//
// Note this package's own DB tests do NOT depend on the shared schema: they
// build their two environment schemas from scratch and drop them again, because
// what they assert is which SCHEMA a row lands in and a full migration per
// schema would make each case minutes long. The TestMain is here for the
// LANE's sake -- membership is what makes the assertions run in CI at all, and
// the lane's contract is that every member serializes on EnsureSchema.
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
