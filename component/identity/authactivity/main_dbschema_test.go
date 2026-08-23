package authactivity

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/znasllc-io/memql/component/database/dbtest"
)

// TestMain is what puts this package in the "db-tests" lane (memql#2551).
//
// The lane runs per-package binaries as parallel processes against ONE shared
// database, so without this the suites race each other's migration and fail
// intermittently with `relation "MemoryNodes" does not exist`.
//
// It matters more here than in most packages: this job's whole behaviour is a
// DELETE against the shared node table, and there is no useful way to test
// that without a table. A prune_db_test.go outside the lane would only ever
// run on the machine of whoever happened to have Postgres up -- which for a
// statement that deletes rows is indistinguishable from not having written it.
func TestMain(m *testing.M) {
	if _, err := dbtest.EnsureSchema(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "dbtest.EnsureSchema: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
