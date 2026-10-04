package pipelinerun

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/znasllc-io/memql/component/database/dbtest"
)

// TestMain guarantees the shared test schema is fully migrated before the
// Postgres-gated test in this package runs, serialized against the other
// db-gated packages CI runs in the same lane (memql#2551): the lane runs
// per-package test binaries as parallel processes against one database.
//
// When no database is reachable EnsureSchema is a no-op and the db-gated test
// self-skips, so every other test here runs exactly as it does without one;
// MEMQL_REQUIRE_DB=1 turns that skip into a failure.
func TestMain(m *testing.M) {
	if _, err := dbtest.EnsureSchema(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "dbtest.EnsureSchema: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
