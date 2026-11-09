package emailstats

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
// per-package test binaries against shared databases, so without this the
// suites race each other's migration and fail intermittently with `relation
// "MemoryNodes" does not exist` (memql#2551). This package also brings the
// migration that creates email_stat_fact and email_stat_watermark, so on a
// developer's existing database the first run here is the one that creates
// them.
//
// When no DB is reachable EnsureSchema is a no-op and the individual tests
// self-skip through dbtest.Unreachable, so a developer without Postgres still
// runs the pure fact rules. The lane sets MEMQL_REQUIRE_DB=1, which turns that
// skip into a failure.
func TestMain(m *testing.M) {
	if _, err := dbtest.EnsureSchema(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "dbtest.EnsureSchema: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
