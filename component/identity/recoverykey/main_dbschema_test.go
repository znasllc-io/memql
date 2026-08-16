package recoverykey

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/znasllc-io/memql/component/database/dbtest"
)

// TestMain is what lets this package join the "db-tests" lane (memql#2551).
//
// The lane runs per-package test binaries as PARALLEL PROCESSES against ONE
// shared database, so without this the suites race each other's migration and
// fail intermittently with `relation "MemoryNodes" does not exist`.
//
// WORTH SAYING PLAINLY: this package's one db-gated test does not need a
// schema. It exercises pg_advisory_xact_lock and fakes the engine, so it
// touches no table. The TestMain is here because lane membership is what makes
// the test RUN IN CI at all, and membership requires it -- a db-gated test
// outside the lane is a test that only ever runs on the machine of whoever
// happened to have Postgres up, which for a concurrency assertion is
// indistinguishable from not having written it (memql#3965).
//
// When no DB is reachable EnsureSchema is a no-op and the test self-skips, so a
// developer without Postgres sees what they did before. The lane sets
// MEMQL_REQUIRE_DB=1, so a skip THERE is a failure.
func TestMain(m *testing.M) {
	if _, err := dbtest.EnsureSchema(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "dbtest.EnsureSchema: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
