package clustere2e

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
// no-op and each db-gated test self-skips through dbtest.Unreachable.
//
// Untagged on purpose, like the db-gated test it serves
// (email_rule_arming_db_test.go): the db-tests lane passes no -tags. Under
// -tags clustere2e it runs too and does exactly what it does in every other
// db-gated package -- migrates the database dbtest.DSN() names, or nothing
// when none answers there. The live-cluster tests never read that database.
func TestMain(m *testing.M) {
	if _, err := dbtest.EnsureSchema(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "dbtest.EnsureSchema: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
