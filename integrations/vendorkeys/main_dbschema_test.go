package vendorkeys

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/znasllc-io/memql/component/database/dbtest"
)

// TestMain guarantees the shared test schema is fully migrated before any
// Postgres-gated test in this package runs, serialized against the other
// db-gated packages CI runs in the same lane (memql#2551). With no database
// reachable EnsureSchema is a no-op and the individual tests self-skip.
//
// This TestMain is what makes `integrations/vendorkeys` a db-gated package.
// Which keys can pay for a call, and what a call learned about one, are claims
// about rows read fresh under a particular actor and written under a lock, and
// a fake engine answers all of that whichever way a test wants.
func TestMain(m *testing.M) {
	if _, err := dbtest.EnsureSchema(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "dbtest.EnsureSchema: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
