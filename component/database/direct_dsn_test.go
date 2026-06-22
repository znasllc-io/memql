package database

import (
	"context"
	"database/sql"
	"testing"

	"github.com/znasllc-io/memql/core/common"
)

// fakeOpener returns a fresh non-connecting *sql.DB on every call and counts
// how many times it was asked to open. Distinct instances let the test assert
// the main and direct pools are genuinely separate handles.
func fakeOpener(count *int) SQLOpener {
	return func(_ string, _ string) (*sql.DB, error) {
		*count++
		return sql.OpenDB(stubConnector{}), nil
	}
}

func newDirectTestDB(t *testing.T, opener SQLOpener, args ...DatabaseArg) *Database {
	t.Helper()

	var base Database
	full := append([]DatabaseArg{
		WithDSN("postgres://localhost:5432/main?sslmode=disable"),
		base.WithSQLOpener(opener),
		base.WithMigrateOnStart(false),
	}, args...)

	d, err := NewDatabase(common.ComponentName("test-direct"), full...)
	if err != nil {
		t.Fatalf("NewDatabase: %v", err)
	}
	return d
}

// TestDirectBunDB_DistinctPoolWhenConfigured is the #1927 acceptance: with
// DIRECT_DSN set, DirectBunDB() returns a pool distinct from the main one, and
// the opener is invoked twice (one main + one direct).
func TestDirectBunDB_DistinctPoolWhenConfigured(t *testing.T) {
	var opens int
	d := newDirectTestDB(t, fakeOpener(&opens),
		WithDirectDSN("postgres://localhost:5432/direct?sslmode=disable"))

	_, cancel, err := d.prepareForRun(context.Background())
	if err != nil {
		t.Fatalf("prepareForRun: %v", err)
	}
	defer cancel()
	defer d.cleanupAfterRun()

	if d.BunDB() == nil {
		t.Fatal("main BunDB() is nil")
	}
	if d.DirectBun == nil {
		t.Fatal("DirectBun was not opened despite DIRECT_DSN being set")
	}
	if d.DirectBunDB() == nil {
		t.Fatal("DirectBunDB() is nil")
	}
	if d.DirectBunDB() == d.BunDB() {
		t.Fatal("DirectBunDB() returned the main pool; expected a distinct handle")
	}
	if opens != 2 {
		t.Fatalf("expected opener to be called twice (main + direct), got %d", opens)
	}
}

// TestDirectBunDB_FallsBackToMainWhenUnset asserts the env-agnostic fallback:
// with DIRECT_DSN unset, DirectBunDB() returns the main pool and no second
// pool is opened, so local/dev without a pooler is unaffected.
func TestDirectBunDB_FallsBackToMainWhenUnset(t *testing.T) {
	var opens int
	d := newDirectTestDB(t, fakeOpener(&opens))

	_, cancel, err := d.prepareForRun(context.Background())
	if err != nil {
		t.Fatalf("prepareForRun: %v", err)
	}
	defer cancel()
	defer d.cleanupAfterRun()

	if d.DirectBun != nil {
		t.Fatal("DirectBun should be nil when DIRECT_DSN is unset")
	}
	if d.DirectBunDB() != d.BunDB() {
		t.Fatal("DirectBunDB() should fall back to the main pool when unset")
	}
	if opens != 1 {
		t.Fatalf("expected opener to be called once (main only), got %d", opens)
	}
}

// TestWithDirectDSN_BlankIsNoop guards that a blank/whitespace DIRECT_DSN does
// not configure a direct pool (so an empty env var degrades to the main pool).
func TestWithDirectDSN_BlankIsNoop(t *testing.T) {
	cfg := defaultConfig()
	WithDirectDSN("   ").Apply(cfg)
	if cfg.directDSN != nil {
		t.Fatalf("blank DIRECT_DSN should be a no-op, got %q", *cfg.directDSN)
	}

	WithDirectDSN("postgres://host/db").Apply(cfg)
	if cfg.directDSN == nil || *cfg.directDSN != "postgres://host/db" {
		t.Fatalf("WithDirectDSN did not set the direct dsn: %v", cfg.directDSN)
	}
}

// TestEnvOptionsToArgs_DirectDSN asserts the env path threads DirectDSN through
// to the config.
func TestEnvOptionsToArgs_DirectDSN(t *testing.T) {
	args, err := EnvOptionsToArgs(DatabaseEnvOptions{
		DSN:       "postgres://main",
		DirectDSN: "postgres://direct",
	})
	if err != nil {
		t.Fatalf("EnvOptionsToArgs: %v", err)
	}

	cfg := defaultConfig()
	for _, a := range args {
		a.Apply(cfg)
	}

	if cfg.directDSN == nil || *cfg.directDSN != "postgres://direct" {
		t.Fatalf("DirectDSN did not reach config: %v", cfg.directDSN)
	}
}

// TestLoadDatabaseEnvOptions_DirectDSN asserts the loader reads DIRECT_DSN.
func TestLoadDatabaseEnvOptions_DirectDSN(t *testing.T) {
	t.Setenv("TESTDB_DSN", "postgres://main")
	t.Setenv("TESTDB_DIRECT_DSN", "postgres://direct")

	opts, err := LoadDatabaseEnvOptions(DatabaseEnvLoader{Prefix: "TESTDB_"})
	if err != nil {
		t.Fatalf("LoadDatabaseEnvOptions: %v", err)
	}
	if opts.DirectDSN != "postgres://direct" {
		t.Fatalf("DIRECT_DSN not loaded, got %q", opts.DirectDSN)
	}
}
