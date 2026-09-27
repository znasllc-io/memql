package datasync

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/znasllc-io/memql/component/database/dbtest"
)

// TestMain guarantees the shared test schema is fully migrated before any
// Postgres-gated test in this package runs, serialized against the other
// db-gated packages CI runs in the same lane. Without it these suites race
// the other packages' migrations on the shared DB and intermittently fail
// with `relation "MemoryNodes" does not exist` (memql#2551). With no DB
// reachable EnsureSchema is a no-op and the individual tests self-skip.
//
// Adding this TestMain is what makes `component/datasync` a db-gated
// package, which obliges the db-tests lane selector
// (scripts/ci/db-gated-packages.sh) to name it -- scripts/cidb/dbgate_test.go
// asserts that in both directions (memql#2886). The package earned it with
// syncstate_write_db_test.go: the sync-state health row is written through
// the real engine, and the claim under test -- an id with no colon in its
// short segment is accepted and the row reads back with every field -- is
// about the engine's id validation and typed-field check over a real
// insert, which no fake engine exercises.
func TestMain(m *testing.M) {
	if _, err := dbtest.EnsureSchema(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "dbtest.EnsureSchema: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
