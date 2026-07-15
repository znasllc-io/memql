package steps

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/znasllc-io/memql/component/database/dbtest"
)

// TestMain guarantees the shared test schema is fully migrated before any
// Postgres-gated test in this package runs, serialized against the other
// db-gated packages CI runs in the same lane. Without it,
// TestAccountDeletionSweep_HardDeletesExpiredUser_DBAcceptance raced
// examples/referencepack's migration on the shared DB and intermittently
// failed with `relation "MemoryNodes" does not exist` (memql#2551). When no
// DB is reachable EnsureSchema is a no-op and the individual tests self-skip.
func TestMain(m *testing.M) {
	if _, err := dbtest.EnsureSchema(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "dbtest.EnsureSchema: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
