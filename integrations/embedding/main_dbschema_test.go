package embedding

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/znasllc-io/memql/component/database/dbtest"
)

// TestMain guarantees the shared test schema is fully migrated before any
// Postgres-gated test in this package runs, serialized against the other
// db-gated packages CI runs in the same lane -- without it these suites race
// the other packages' migrations on the shared DB and intermittently fail with
// `relation "MemoryNodes" does not exist` (memql#2551). When no DB is
// reachable EnsureSchema is a no-op and the individual tests self-skip.
//
// Adding this TestMain is what makes `integrations/embedding` a db-gated
// package, which obliges the db-tests lane selector in .github/workflows/ci.yml
// to name it -- scripts/cidb/dbgate_test.go asserts that in both directions
// (memql#2886).
func TestMain(m *testing.M) {
	if _, err := dbtest.EnsureSchema(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "dbtest.EnsureSchema: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
