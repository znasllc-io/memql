package database

import (
	"database/sql"
	"testing"

	"github.com/znasllc-io/memql/core/common"
)

// Every request-serving backend carries a statement timeout; the migration
// pool carries everything else and NOT that (a production instance, 2026-09-13).
func TestEveryPooledBackendCarriesAStatementTimeout(t *testing.T) {
	t.Setenv(envDBStatementTimeoutMs, "")
	params := sessionConnParams()
	if got := params["statement_timeout"]; got != "60000" {
		t.Fatalf("statement_timeout = %v, want the 60000 default", got)
	}

	t.Setenv(envDBStatementTimeoutMs, "15000")
	if got := sessionConnParams()["statement_timeout"]; got != "15000" {
		t.Fatalf("statement_timeout = %v, want the env override 15000", got)
	}

	t.Setenv(envDBStatementTimeoutMs, "0")
	if _, ok := sessionConnParams()["statement_timeout"]; ok {
		t.Fatalf("0 must omit the parameter so the server default stands")
	}
}

func TestMigrationsRunWithoutTheStatementTimeout(t *testing.T) {
	t.Setenv(envDBStatementTimeoutMs, "")
	params := migrationConnParams()
	if _, ok := params["statement_timeout"]; ok {
		t.Fatalf("the migration pool must not carry statement_timeout: %v", params)
	}
	for _, keep := range []string{"idle_in_transaction_session_timeout", "idle_session_timeout"} {
		if _, ok := params[keep]; !ok {
			t.Fatalf("the migration pool dropped %s; only statement_timeout is exempt", keep)
		}
	}
}

func TestACustomOpenerOwnsTheMigrationPoolToo(t *testing.T) {
	base, err := NewDatabase(common.ComponentName("test-opener-base"), WithDSN("postgres://user:pass@localhost:5432/db?sslmode=disable"))
	if err != nil {
		t.Fatalf("NewDatabase: %v", err)
	}
	if base.config.migrationOpener == nil {
		t.Fatalf("the default config must carry a migration opener")
	}
	d, err := NewDatabase(common.ComponentName("test-opener"), WithDSN("postgres://user:pass@localhost:5432/db?sslmode=disable"),
		base.WithSQLOpener(func(driverName, dsn string) (*sql.DB, error) { return nil, nil }))
	if err != nil {
		t.Fatalf("NewDatabase: %v", err)
	}
	if d.config.migrationOpener != nil {
		t.Fatalf("a custom sqlOpener must clear the default migration opener, or migrations would dial a real DSN a test never meant to open")
	}
}
