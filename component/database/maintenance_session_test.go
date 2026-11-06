package database

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

// maintenance_session_test.go -- what the maintenance session is opened with and
// on which endpoint (maintenance_session.go). The db-gated half,
// maintenance_session_db_test.go, says what PostgreSQL then gives it.

// TestMaintenanceConnParams: no statement_timeout from the connection, so a
// rewrite of a large chunk is not cancelled at the request deadline, and the idle
// limits kept, so a session whose process died is still reaped by the server.
func TestMaintenanceConnParams(t *testing.T) {
	t.Setenv(envDBIdleInTxTimeoutMs, "")
	t.Setenv(envDBStatementTimeoutMs, "")
	t.Setenv(envDBIdleSessionTimeoutMs, "")

	params := maintenanceConnParams()
	if _, ok := params["statement_timeout"]; ok {
		t.Errorf("a maintenance session inherits the request deadline: statement_timeout=%v", params["statement_timeout"])
	}
	if got := params["idle_in_transaction_session_timeout"]; got != "60000" {
		t.Errorf("idle_in_transaction_session_timeout = %v, want the request pool's 60000", got)
	}
	if got := params["idle_session_timeout"]; got != "300000" {
		t.Errorf("idle_session_timeout = %v, want the request pool's 300000", got)
	}

	// Lowering the request pool's statement timeout must not reach it either.
	t.Setenv(envDBStatementTimeoutMs, "50")
	if _, ok := maintenanceConnParams()["statement_timeout"]; ok {
		t.Error("MEMQL_DB_STATEMENT_TIMEOUT_MS reached the maintenance session")
	}
}

// recordingOpener records the DSN it was asked to open and returns a pool that
// cannot connect, which is all the endpoint choice needs.
func recordingOpener(dsn *string) SQLOpener {
	return func(_ string, got string) (*sql.DB, error) {
		*dsn = got
		return sql.OpenDB(stubConnector{}), nil
	}
}

// TestTheMaintenanceSessionPrefersTheDirectEndpoint: its statements are
// session-shaped (SETs that must persist, a session advisory lock, a statement
// that refuses a transaction block), which a transaction pooler would break, so
// it opens on DIRECT_DSN when there is one and on the main DSN otherwise.
func TestTheMaintenanceSessionPrefersTheDirectEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   []DatabaseArg
		wanted string
	}{
		{"direct configured", []DatabaseArg{WithDirectDSN("postgres://localhost:5432/direct?sslmode=disable")}, "postgres://localhost:5432/direct?sslmode=disable"},
		{"main only", nil, "postgres://localhost:5432/main?sslmode=disable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var opens int
			d := newDirectTestDB(t, fakeOpener(&opens), tc.args...)
			var dsn string
			d.config.maintenanceOpener = recordingOpener(&dsn)

			_, _, err := d.OpenMaintenanceSession(context.Background())
			if err == nil {
				t.Fatal("the stub connector connected; the test cannot tell what it opened")
			}
			if dsn != tc.wanted {
				t.Errorf("the maintenance session opened on %q, want %q", dsn, tc.wanted)
			}
		})
	}
}

// TestACustomSQLOpenerLeavesNoMaintenanceSession: an opener a test supplies
// stands for the whole database, as it does for the migration and gate pools; a
// real session beside it would dial a server the test never meant to reach, and
// the request pool is not a substitute (its limits are the reason the session
// exists).
func TestACustomSQLOpenerLeavesNoMaintenanceSession(t *testing.T) {
	var opens int
	d := newDirectTestDB(t, fakeOpener(&opens))
	conn, closeSession, err := d.OpenMaintenanceSession(context.Background())
	if !errors.Is(err, ErrMaintenanceSessionUnavailable) || conn != nil || closeSession != nil {
		t.Fatalf("a custom opener left a maintenance session: conn=%v err=%v", conn, err)
	}
	if opens != 0 {
		t.Fatalf("asking for a maintenance session opened the request pool %d times", opens)
	}
}
