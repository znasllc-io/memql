package database_test

import (
	"context"
	"os"
	"testing"

	"github.com/znasllc-io/memql/component/database"
)

// The collapse migration (memql#5325), run against a transaction-local table
// that shadows the live one. Run TWICE: the second pass must find nothing,
// because a migration that is not idempotent is one that deletes a verdict
// the cluster wrote between two runs.
//
// The collapse is Go now (memql#5604) and runs here inside the test's
// transaction, each of its own transactions a savepoint, so the shadow table
// is all it ever sees. A batch of ONE version, the smallest there is, so the
// three-version row is collapsed over several statements rather than one. The
// bounded, resumable behaviour itself is pinned against a real hypertable in
// module_readiness_history_collapse_bounded_db_test.go.
func TestModuleReadinessHistoryCollapseMigration(t *testing.T) {
	db := retiredFieldDB(t)
	if db == nil {
		return
	}
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback() //nolint:errcheck // the rollback IS the cleanup
	execFile := func(path string) {
		t.Helper()
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
	execFile("testdata/module_readiness_history_collapse_setup.sql")
	for range 2 {
		if _, err := database.CollapseModuleReadinessHistory(ctx, tx, nil, 1); err != nil {
			t.Fatalf("collapse: %v", err)
		}
		execFile("testdata/module_readiness_history_collapse_assert.sql")
	}
	// The down is a no-op by decision, not by omission: running it must leave
	// the collapsed table exactly as it is.
	if err := database.RevertModuleReadinessHistoryCollapse(ctx, db); err != nil {
		t.Fatalf("down: %v", err)
	}
	execFile("testdata/module_readiness_history_collapse_assert.sql")
}
