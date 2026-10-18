package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"log/slog"
	"testing"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/migrate"

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

// A post-migration hook assumes the entire schema exists. In particular, the
// Timescale hook must not enable compression while an earlier ID-rewriting
// migration still needs to run. A failed attempt must remain retryable without
// applying any post-migration effects to its partial schema.
func TestRunMigrationsDefersPostHooksUntilTheSchemaSucceeds(t *testing.T) {
	for _, failure := range []string{"custom migrator", "migration initialization"} {
		t.Run(failure, func(t *testing.T) {
			cfg := &Database{}
			calls := 0
			d := newMigrationTestDB(t, cfg.WithMigrateOnStart(true),
				cfg.WithPostMigrationHook(func(context.Context, *bun.DB, *slog.Logger) error {
					calls++
					return nil
				}))
			if failure == "custom migrator" {
				d.config.migrator = stubMigrator{err: errors.New("migration refused")}
			} else {
				// Init must fail because this connector cannot reach a database.
				d.config.migrations = migrate.NewMigrations()
			}
			db := newStubBun()
			t.Cleanup(func() { _ = db.Close() })
			d.runMigrations(context.Background(), db)
			if calls != 0 || d.MigrationError() == nil || !d.migrationsPending() {
				t.Fatalf("failed attempt: hooks=%d, error=%v, pending=%v", calls, d.MigrationError(), d.migrationsPending())
			}
			// A later successful attempt must run the hook exactly once and
			// clear the original refusal instead of poisoning recovery.
			d.config.migrator = stubMigrator{}
			d.config.migrations = nil
			d.runMigrations(context.Background(), db)
			if calls != 1 || d.MigrationError() != nil || d.migrationsPending() {
				t.Fatalf("successful retry: hooks=%d, error=%v, pending=%v", calls, d.MigrationError(), d.migrationsPending())
			}
		})
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
