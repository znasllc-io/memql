package database_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// A transaction-local table shadows the live table: even fixture IDs matching
// real system IDs never write to the cluster's data. Run the actual migration
// twice, comparing the complete surviving rows after each run.
func TestRetiredPortalSiteMigration(t *testing.T) {
	db := retiredFieldDB(t)
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
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
	execFile("testdata/retired_portal_site_setup.sql")
	for range 2 {
		execFile(filepath.Join(migrationsDir, "20260915220000_retired_portal_site.up.sql"))
		execFile("testdata/retired_portal_site_assert.sql")
	}
}
