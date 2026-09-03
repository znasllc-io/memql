package logstore

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/znasllc-io/memql/component/database/dbtest"
)

// TestMain migrates the shared test database before any db-gated case runs
// (memql#2551): the log_line hypertable is created by
// 20260903000000_log_line_hypertable.up.sql, and without EnsureSchema the
// first db-gated case here would race every other package's migration.
//
// The db-gated cases self-skip when no Postgres is reachable and fail under
// MEMQL_REQUIRE_DB=1, exactly as every other db-gated package does.
func TestMain(m *testing.M) {
	if _, err := dbtest.EnsureSchema(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "dbtest.EnsureSchema: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
