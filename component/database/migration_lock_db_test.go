package database

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/uptrace/bun/migrate"
)

func migrationLockDB(t *testing.T, readTimeout time.Duration) (*bun.DB, *bun.DB, string) {
	t.Helper()
	dsn := os.Getenv("MEMQL_DATABASE_DSN")
	if dsn == "" {
		if os.Getenv("MEMQL_REQUIRE_DB") == "1" {
			t.Fatal("MEMQL_DATABASE_DSN is required")
		}
		t.Skip("requires Postgres")
	}
	admin := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	t.Cleanup(func() { admin.Close() })
	schema := fmt.Sprintf("migration_lock_%d", time.Now().UnixNano())
	if _, err := admin.Exec("CREATE SCHEMA ?", bun.Ident(schema)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		admin.Exec("SELECT pg_cancel_backend(pid) FROM pg_stat_activity WHERE application_name = ? AND pid <> pg_backend_pid()", schema)
		admin.Exec("DROP SCHEMA ? CASCADE", bun.Ident(schema))
	})
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn), pgdriver.WithConnParams(map[string]any{"search_path": schema}), pgdriver.WithApplicationName(schema), pgdriver.WithReadTimeout(readTimeout))), pgdialect.New())
	t.Cleanup(func() { db.Close() })
	return db, admin, schema
}

func lockedMigrationRunner(t *testing.T, migrations *migrate.Migrations) *Database {
	t.Helper()
	cfg := &Database{}
	d := newMigrationTestDB(t, cfg.WithMigrateOnStart(true))
	d.config.migrations = migrations
	d.config.migrationOpener = nil
	return d
}

func TestMigrationLockBlocksAnotherReplicaAndAllowsRetry(t *testing.T) {
	db, _, _ := migrationLockDB(t, time.Second)
	ctx := context.Background()
	calls := 0
	migrations := migrate.NewMigrations()
	migrations.Add(migrate.Migration{Name: "20260925000000", Up: func(context.Context, *migrate.Migrator, *migrate.Migration) error { calls++; return nil }})
	owner := migrate.NewMigrator(db, migrations)
	if err := owner.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := owner.Lock(ctx); err != nil {
		t.Fatal(err)
	}
	runner := lockedMigrationRunner(t, migrations)
	runner.runMigrations(ctx, db)
	if calls != 0 || runner.MigrationError() == nil || !runner.migrationsPending() {
		t.Fatalf("ran without the other replica's lock: calls=%d err=%v", calls, runner.MigrationError())
	}
	if err := owner.Unlock(ctx); err != nil {
		t.Fatal(err)
	}
	runner.runMigrations(ctx, db)
	if calls != 1 || runner.MigrationError() != nil || runner.migrationsPending() {
		t.Fatalf("retry failed: calls=%d err=%v", calls, runner.MigrationError())
	}
}

func TestMigrationReadTimeoutRetainsLockWhileBackendRuns(t *testing.T) {
	db, admin, schema := migrationLockDB(t, 100*time.Millisecond)
	migrations := migrate.NewMigrations()
	calls := 0
	migrations.Add(migrate.Migration{Name: "20260925000000", Up: func(ctx context.Context, m *migrate.Migrator, _ *migrate.Migration) error {
		calls++
		_, err := m.DB().ExecContext(ctx, "SELECT pg_sleep(3)")
		return err
	}})
	runner := lockedMigrationRunner(t, migrations)
	runner.runMigrations(context.Background(), db)
	if runner.MigrationError() == nil {
		t.Fatal("expected client timeout")
	}
	var active int
	if err := admin.NewRaw("SELECT count(*) FROM pg_stat_activity WHERE application_name = ? AND state = 'active' AND query LIKE 'SELECT pg_sleep%'", schema).Scan(context.Background(), &active); err != nil {
		t.Fatal(err)
	}
	if active != 1 {
		t.Fatalf("expected abandoned backend still executing, got %d", active)
	}
	sibling := lockedMigrationRunner(t, migrations)
	sibling.runMigrations(context.Background(), db)
	if calls != 1 || sibling.MigrationError() == nil || !strings.Contains(sibling.MigrationError().Error(), "migration lock") {
		t.Fatalf("second replica overlapped orphan: calls=%d err=%v", calls, sibling.MigrationError())
	}
}

func TestMigrationSQLFailureReleasesLockForRetry(t *testing.T) {
	db, _, _ := migrationLockDB(t, time.Second)
	calls := 0
	migrations := migrate.NewMigrations()
	migrations.Add(migrate.Migration{Name: "20260925000000", Up: func(ctx context.Context, m *migrate.Migrator, _ *migrate.Migration) error {
		calls++
		if calls == 1 {
			_, err := m.DB().ExecContext(ctx, "SELECT 1/0")
			return err
		}
		return nil
	}})
	runner := lockedMigrationRunner(t, migrations)
	runner.runMigrations(context.Background(), db)
	if runner.MigrationError() == nil {
		t.Fatal("expected SQL error")
	}
	sibling := lockedMigrationRunner(t, migrations)
	sibling.runMigrations(context.Background(), db)
	if calls != 2 || sibling.MigrationError() != nil {
		t.Fatalf("acknowledged SQL failure stranded lock: calls=%d err=%v", calls, sibling.MigrationError())
	}
}
