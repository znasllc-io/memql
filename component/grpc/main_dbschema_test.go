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
//
// For a developer WITH a reachable database, this package is the one of the
// four where the migration is NOT new: wire_bare_ids_test.go:231 already booted
// the production MemoryNodesDatabase (migrate-on-start) and took a blank
// database to a full schema, gated only on reachability. Measured on
// origin/main -- an empty database came out with 12 tables after
// TestWireBareIds_EngineRoundTrip alone.
//
// What changes is WHEN: the schema is complete before any test in this package
// runs, instead of only from the point some test boots the database itself.
// That is what makes the package safe to run as one of several parallel
// binaries against one shared database (memql#2551). The in-test boot at
// wire_bare_ids_test.go:234 is unchanged and still runs -- it is what produces
// the handle the tests use; only the migration it performs is redundant now,
// because TestMain got there first. The tests already wrote and deleted rows
// when a reachable database was also migrated; lane membership only ever
// governed CI.
//
// (This paragraph originally claimed the migration never happened here before
// memql#3030. That is true of the other three packages joining the lane and
// false of this one; it was corrected during the landing review rather than
// left as a comment asserting something the code does not do.)
func TestMain(m *testing.M) {
	if _, err := dbtest.EnsureSchema(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "dbtest.EnsureSchema: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
