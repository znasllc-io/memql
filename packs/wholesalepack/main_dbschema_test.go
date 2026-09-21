package wholesalepack_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/znasllc-io/memql/component/database/dbtest"
)

// TestMain routes this package's schema migration through the shared,
// advisory-locked EnsureSchema so it is serialized against the other
// db-gated packages CI runs in the same lane (memql#2551). The lane runs
// per-package binaries as parallel processes against ONE database, so
// without this the wholesale live-e2e suite would race every sibling's
// migration -- which is the failure examples/referencepack's own TestMain
// was written for, and packs/reviewspack keeps for the same reason.
//
// When no database is reachable EnsureSchema is a no-op and the suite's
// db-gated tests self-skip; MEMQL_REQUIRE_DB=1 turns that skip into a
// failure.
func TestMain(m *testing.M) {
	if _, err := dbtest.EnsureSchema(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "dbtest.EnsureSchema: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
