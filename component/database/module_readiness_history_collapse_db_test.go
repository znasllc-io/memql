package database_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// The collapse migration (memql#5325), run against a transaction-local table
// that shadows the live one. Run TWICE: the second pass must find nothing,
// because a migration that is not idempotent is one that deletes a verdict
// the cluster wrote between two runs.
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
		execFile(filepath.Join(migrationsDir, "20260921000000_module_readiness_history_collapse.up.sql"))
		execFile("testdata/module_readiness_history_collapse_assert.sql")
	}
	// The down is a no-op by decision, not by omission: running it must leave
	// the collapsed table exactly as it is.
	execFile(filepath.Join(migrationsDir, "20260921000000_module_readiness_history_collapse.down.sql"))
	execFile("testdata/module_readiness_history_collapse_assert.sql")
}
