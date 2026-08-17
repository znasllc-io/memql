package backup

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/znasllc-io/memql/component/database/dbtest"
)

// TestMain guarantees the shared test schema is fully migrated before any
// Postgres-gated test in this package runs, serialized against the other
// db-gated packages CI runs in the same lane. Without it, these *_db_test.go
// suites race the other packages' migration on the shared DB and
// intermittently fail with `relation "MemoryNodes" does not exist`
// (memql#2551). When no DB is reachable EnsureSchema is a no-op and the
// individual tests self-skip.
//
// This package became db-gated with staged_data_db_test.go (memql#3985), which
// is what put it in the db-tests lane -- and joining that lane is precisely
// what makes the advisory lock necessary, since the lane runs each package's
// test binary as a parallel process against ONE database.
func TestMain(m *testing.M) {
	if _, err := dbtest.EnsureSchema(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "dbtest.EnsureSchema: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
