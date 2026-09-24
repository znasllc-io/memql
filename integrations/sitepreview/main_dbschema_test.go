package sitepreview

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
// Adding this TestMain is what makes `integrations/sitepreview` a db-gated
// package: readiness_guard_parity_db_test.go holds the readiness answer and
// the go-live write guard to one decision over real rows, which a fake engine
// cannot. That obliges the db-tests lane selector in .github/workflows/ci.yml
// and scripts/ci/db-gated-packages.sh to name it -- scripts/cidb/dbgate_test.go
// asserts that in both directions (memql#2886).
func TestMain(m *testing.M) {
	if _, err := dbtest.EnsureSchema(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "dbtest.EnsureSchema: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
