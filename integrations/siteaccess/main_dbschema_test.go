package siteaccess

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/znasllc-io/memql/component/database/dbtest"
)

// TestMain guarantees the shared test schema is fully migrated before any
// Postgres-gated test in this package runs, serialized against the other
// db-gated packages CI runs in the same lane (memql#2551). With no DB
// reachable EnsureSchema is a no-op and the individual tests self-skip.
//
// Adding this TestMain is what makes `integrations/siteaccess` a db-gated
// package: integration_db_test.go holds every act on a code to what the row
// actually holds afterwards, under the actor each read and write really runs
// as, and a fake engine answers both of those whichever way a test wants. That
// obliges scripts/ci/db-gated-packages.sh, memql-package.yaml and its reviewed
// fixture to name it -- scripts/cidb/dbgate_test.go asserts that in both
// directions (memql#2886).
func TestMain(m *testing.M) {
	if _, err := dbtest.EnsureSchema(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "dbtest.EnsureSchema: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
