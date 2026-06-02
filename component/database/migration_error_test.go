package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"testing"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"

	"github.com/znasllc-io/memql/core/common"
)

// stubConnector yields a *sql.DB that never actually connects. The migration
// paths under test (the injected custom migrator) don't touch the connection,
// so we only need a non-nil *bun.DB to get past runMigrations' guard.
type stubConnector struct{}

func (stubConnector) Connect(context.Context) (driver.Conn, error) {
	return nil, errors.New("stub connector: not connectable")
}

func (stubConnector) Driver() driver.Driver { return nil }

func newStubBun() *bun.DB {
	return bun.NewDB(sql.OpenDB(stubConnector{}), pgdialect.New())
}

// stubMigrator is an injectable Migrator (WithMigrator) that returns a fixed
// result, letting us exercise the failure path without a real database.
type stubMigrator struct {
	err error
}

func (m stubMigrator) Migrate(context.Context, *bun.DB) error { return m.err }

func newMigrationTestDB(t *testing.T, args ...DatabaseArg) *Database {
	t.Helper()

	base := []DatabaseArg{WithDSN("postgres://localhost:5432/test?sslmode=disable")}
	d, err := NewDatabase(common.ComponentName("test-migrate"), append(base, args...)...)
	if err != nil {
		t.Fatalf("NewDatabase: %v", err)
	}

	// Exercise only the injected custom-migrator path: the default config ships
	// an empty bun migration set whose Init() would need a live connection,
	// which the stub *bun.DB cannot provide.
	d.config.migrations = nil

	return d
}

// TestRunMigrationsCapturesError is the #671 regression: a failed migration
// must be observable via MigrationError() (and therefore fail the gated
// pre-deploy migrate Job) rather than being only logged.
func TestRunMigrationsCapturesError(t *testing.T) {
	cfg := &Database{}
	wantErr := errors.New("boom: SQLSTATE 23502 not-null violation")

	d := newMigrationTestDB(t,
		cfg.WithMigrateOnStart(true),
		cfg.WithMigrator(stubMigrator{err: wantErr}),
	)

	d.runMigrations(context.Background(), newStubBun())

	got := d.MigrationError()
	if got == nil {
		t.Fatal("MigrationError() = nil, want the migration failure to be captured")
	}
	if !errors.Is(got, wantErr) {
		t.Fatalf("MigrationError() = %v, want it to wrap %v", got, wantErr)
	}
}

// TestRunMigrationsSuccessLeavesNoError verifies the happy path records no
// error, so a clean deploy isn't aborted spuriously.
func TestRunMigrationsSuccessLeavesNoError(t *testing.T) {
	cfg := &Database{}

	d := newMigrationTestDB(t,
		cfg.WithMigrateOnStart(true),
		cfg.WithMigrator(stubMigrator{err: nil}),
	)

	d.runMigrations(context.Background(), newStubBun())

	if got := d.MigrationError(); got != nil {
		t.Fatalf("MigrationError() = %v, want nil after a successful migration", got)
	}
}

// TestRunMigrationsSkippedLeavesNoError verifies that with migrateOnStart=false
// nothing runs and no error is recorded.
func TestRunMigrationsSkippedLeavesNoError(t *testing.T) {
	cfg := &Database{}

	d := newMigrationTestDB(t,
		cfg.WithMigrateOnStart(false),
		cfg.WithMigrator(stubMigrator{err: errors.New("must not run")}),
	)

	d.runMigrations(context.Background(), newStubBun())

	if got := d.MigrationError(); got != nil {
		t.Fatalf("MigrationError() = %v, want nil when migrateOnStart is false", got)
	}
}

// TestRunMigrationsClearsStaleError verifies a later successful attempt clears a
// previously recorded failure (the reconnect path re-runs migrations).
func TestRunMigrationsClearsStaleError(t *testing.T) {
	cfg := &Database{}
	d := newMigrationTestDB(t, cfg.WithMigrateOnStart(true))

	// First attempt fails.
	d.config.migrator = stubMigrator{err: errors.New("transient migrate failure")}
	d.runMigrations(context.Background(), newStubBun())
	if d.MigrationError() == nil {
		t.Fatal("expected first attempt to record an error")
	}

	// Second attempt succeeds and must clear the stale error.
	d.config.migrator = stubMigrator{err: nil}
	d.runMigrations(context.Background(), newStubBun())
	if got := d.MigrationError(); got != nil {
		t.Fatalf("MigrationError() = %v, want nil after a successful retry", got)
	}
}
