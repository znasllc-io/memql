package referencepack_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/znasllc-io/memql/component/database/dbtest"
)

// TestMain routes this package's schema migration through the shared,
// advisory-locked EnsureSchema so it is serialized against the other db-gated
// packages CI runs in the same lane. This package's live_e2e test is the one
// that used to migrate the shared blank DB unsynchronized -- the migration
// the sibling reader suites raced (memql#2551). Bringing it under the lock
// means the first process to acquire it performs the single real migration
// and every other migrate call (including this suite's own mnd.Start) is a
// no-op on an already-complete schema. When no DB is reachable EnsureSchema
// is a no-op and the test self-skips.
func TestMain(m *testing.M) {
	if _, err := dbtest.EnsureSchema(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "dbtest.EnsureSchema: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
