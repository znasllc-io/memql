package identity

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
// WORTH SAYING PLAINLY, as recoverykey's identical TestMain does: this
// package's db-gated tests do not need a schema. They exercise
// pg_advisory_lock with a faked engine and touch no table. The TestMain is
// here because lane membership is what makes those tests RUN IN CI at all,
// and membership requires it -- a concurrency assertion that only executes on
// the machine of whoever happened to have Postgres up is indistinguishable
// from one nobody wrote (memql#4301).
//
// When no DB is reachable EnsureSchema is a no-op and the tests self-skip, so
// a developer without Postgres sees what they did before. The lane sets
// MEMQL_REQUIRE_DB=1, so a skip THERE is a failure.
func TestMain(m *testing.M) {
	if _, err := dbtest.EnsureSchema(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "dbtest.EnsureSchema: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
