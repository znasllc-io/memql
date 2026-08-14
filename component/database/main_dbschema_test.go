package database_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/znasllc-io/memql/component/database/dbtest"
)

// TestMain guarantees the shared test schema is fully migrated before any
// Postgres-gated test in this package runs, serialized against the other
// db-gated packages CI runs in the same lane.
//
// Required by the "db-tests" lane rather than optional bookkeeping: the lane
// runs per-package test binaries as PARALLEL PROCESSES against ONE shared
// database, so without this the suites race each other's migration and fail
// intermittently with `relation "MemoryNodes" does not exist` (memql#2551).
//
// # Why this file is `package database_test` and not `package database`
//
// It has to import dbtest, and dbtest imports component/database/memory-nodes,
// which imports component/database. An INTERNAL test here would therefore be an
// import cycle -- `import cycle not allowed in test`, measured. The EXTERNAL
// test package breaks it, because it is compiled after the package under test.
//
// That is not merely a workaround. The gate in scripts/cidb defines "db-gated"
// as an import of dbtest from a _test.go file, and lane membership as a TestMain
// calling EnsureSchema. A DB test in this package that could do neither would be
// invisible to both -- it would provision nothing, join no lane, and self-skip
// in the ordinary go-tests lane forever, which is precisely the report-green-
// having-verified-nothing failure scripts/cidb exists to prevent (memql#3765).
//
// The cost is that the external package sees only the exported surface. For the
// environment boundary that is an improvement: the tests drive the REAL boot
// path -- MEMQL_DB_SEARCH_PATH through NewDatabase -- rather than calling the
// unexported connector wrapper directly, so what they prove is what ships.
//
// When no DB is reachable EnsureSchema is a no-op and the individual tests
// self-skip, so a developer without Postgres sees exactly what they did before.
func TestMain(m *testing.M) {
	if _, err := dbtest.EnsureSchema(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "dbtest.EnsureSchema: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
