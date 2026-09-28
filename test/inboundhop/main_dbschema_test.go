package inboundhop

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/znasllc-io/memql/component/database/dbtest"
)

// TestMain applies the schema once before the package's db-gated tests run,
// so the first test does not race another package's migration on the shared
// database (memql#2551). When no database is reachable EnsureSchema is a
// no-op and each test self-skips through dbtest.Unreachable.
func TestMain(m *testing.M) {
	if _, err := dbtest.EnsureSchema(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "dbtest.EnsureSchema: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
