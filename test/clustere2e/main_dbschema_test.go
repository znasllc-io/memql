package clustere2e

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/database/dbtest"
)

// databaseDSNEnv names the one database this package's db-gated tests may
// migrate and write to. It is dbtest's own variable (unexported there).
const databaseDSNEnv = "MEMQL_DATABASE_DSN"

// TestMain applies the schema once before the package's db-gated tests run,
// so the first test does not race another package's migration on the shared
// database (memql#2551).
//
// Only a database NAMED in MEMQL_DATABASE_DSN is migrated. dbtest.EnsureSchema
// falls back to its built-in DSN, localhost:5432, when the variable is unset,
// and migrates whatever answers there -- for this package that is the
// developer's local cluster database, the one the live-cluster tests beside
// this file run against. The db-tests lane always names its database, and the
// db-gated test skips (or, under MEMQL_REQUIRE_DB=1, fails) without one.
func TestMain(m *testing.M) {
	if strings.TrimSpace(os.Getenv(databaseDSNEnv)) != "" {
		if _, err := dbtest.EnsureSchema(context.Background()); err != nil {
			fmt.Fprintf(os.Stderr, "dbtest.EnsureSchema: %v\n", err)
			os.Exit(1)
		}
	}
	os.Exit(m.Run())
}
