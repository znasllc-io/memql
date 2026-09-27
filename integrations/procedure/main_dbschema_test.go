package procedure

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/znasllc-io/memql/component/database/dbtest"
)

// TestMain routes this package's schema migration through the shared,
// advisory-locked EnsureSchema, serialized against the other db-gated
// packages the db-tests lane runs in parallel against ONE database
// (memql#2551).
//
// This package joined the lane with ladder_lock_db_test.go (review finding C1,
// epic memql#5408): the ladder's per-construct advisory lock is a claim about
// Postgres, and a fake engine serializes nothing -- it passes the correct
// implementation and the one that locks nothing alike.
//
// When no DB is reachable EnsureSchema is a no-op and the db-gated cases
// self-skip; MEMQL_REQUIRE_DB=1 turns that skip into a failure.
func TestMain(m *testing.M) {
	if _, err := dbtest.EnsureSchema(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "dbtest.EnsureSchema: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
