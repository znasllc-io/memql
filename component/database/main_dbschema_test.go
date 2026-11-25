package database_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/znasllc-io/memql/component/database/dbtest"
)

// TestMain ensures the schema exists before this package's db-gated cases run
// (memql#2551), which is the precondition for joining the `db-tests` lane.
//
// The lane runs per-package binaries as PARALLEL PROCESSES against ONE
// database, so without this the first db-gated case here would race every other
// package's migration -- and a migration race does not fail cleanly, it fails
// as whichever half lost.
//
// It lives in the EXTERNAL test package because that is where this package's
// db-gated file has to live: dbtest imports memory-nodes, which imports this
// package, so an in-package test importing dbtest is an import cycle. Both test
// packages compile into one binary, so this TestMain governs the in-package
// tests too.
//
// The db-gated cases self-skip when no Postgres is reachable and fail under
// MEMQL_REQUIRE_DB=1, exactly as every other db-gated package does.
func TestMain(m *testing.M) {
	// A helper process playing a migrating replica that the parent test kills
	// (migration_lock_killed_holder_db_test.go) works in the parent's scratch
	// schema and needs nothing of the shared one.
	if os.Getenv("MEMQL_TEST_MIGRATION_HOLDER") != "" {
		os.Exit(m.Run())
	}
	if _, err := dbtest.EnsureSchema(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "dbtest.EnsureSchema: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
