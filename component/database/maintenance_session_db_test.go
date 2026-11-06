package database_test

import (
	"context"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
)

// THE MAINTENANCE SESSION AGAINST A REAL DATABASE (maintenance_session.go).
//
// The property it exists for is a statement that outlives pgdriver's 10 s client
// read deadline, which every other pool here keeps: measured on the migration
// pool, `SELECT pg_sleep(12)` fails at 10.0 s with "i/o timeout" while the backend
// carries on (latest_row_index.go). Here the same kind of statement returns.
func TestAMaintenanceSessionOutlivesTheReadDeadlineAndHasNoStatementTimeout(t *testing.T) {
	reachable, err := dbtest.EnsureSchema(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reachable {
		dbtest.Unreachable(t, "maintenance session", dbtest.DSN(), nil)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	conn, closeSession, err := database.OpenMaintenanceSessionDSN(ctx, dbtest.DSN())
	if err != nil {
		t.Fatal(err)
	}
	defer closeSession()

	var statement, idle string
	if err := conn.QueryRowContext(ctx, "SHOW statement_timeout").Scan(&statement); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRowContext(ctx, "SHOW idle_in_transaction_session_timeout").Scan(&idle); err != nil {
		t.Fatal(err)
	}
	if statement != "0" {
		t.Errorf("a maintenance session has a statement_timeout of %q, want none", statement)
	}
	if idle != "1min" {
		t.Errorf("a maintenance session may sit idle in a transaction for %q, want the request pool's 1min", idle)
	}

	// Past the 10 s read deadline, inside the context's minute.
	started := time.Now()
	if _, err := conn.ExecContext(ctx, "SELECT pg_sleep(10.5)"); err != nil {
		t.Fatalf("a statement of 10.5 s failed on the maintenance session after %s: %v", time.Since(started).Round(time.Millisecond), err)
	}

	// A session SET holds across statements: the caller's lock_timeout and
	// statement_timeout depend on it.
	if _, err := conn.ExecContext(ctx, "SET lock_timeout = '1s'"); err != nil {
		t.Fatal(err)
	}
	var lock string
	if err := conn.QueryRowContext(ctx, "SHOW lock_timeout").Scan(&lock); err != nil {
		t.Fatal(err)
	}
	if lock != "1s" {
		t.Errorf("lock_timeout did not hold across statements: %q", lock)
	}
}
