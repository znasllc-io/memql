package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
)

// recordingConn captures the statements and transaction boundaries issued
// against it, so a test can assert the order in which they happen. It
// implements just enough of database/sql/driver for *sql.DB to drive a
// transaction with ExecContext calls.
type recordingConn struct {
	events  []string
	execErr error // when set, ExecContext returns it for matching queries
	failOn  string
}

func (c *recordingConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("recordingConn: Prepare not implemented")
}

func (c *recordingConn) Close() error { return nil }

func (c *recordingConn) Begin() (driver.Tx, error) { return c.BeginTx(context.Background(), driver.TxOptions{}) }

func (c *recordingConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	c.events = append(c.events, "BEGIN")
	return &recordingTx{conn: c}, nil
}

func (c *recordingConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	c.events = append(c.events, query)
	if c.failOn != "" && strings.Contains(query, c.failOn) {
		return nil, c.execErr
	}
	return driver.RowsAffected(0), nil
}

type recordingTx struct{ conn *recordingConn }

func (t *recordingTx) Commit() error {
	t.conn.events = append(t.conn.events, "COMMIT")
	return nil
}

func (t *recordingTx) Rollback() error {
	t.conn.events = append(t.conn.events, "ROLLBACK")
	return nil
}

type recordingConnector struct{ conn *recordingConn }

func (rc *recordingConnector) Connect(context.Context) (driver.Conn, error) { return rc.conn, nil }
func (rc *recordingConnector) Driver() driver.Driver                        { return nil }

func indexOf(events []string, substr string) int {
	for i, e := range events {
		if strings.Contains(e, substr) {
			return i
		}
	}
	return -1
}

// TestCreateTimescaleExtensionLocked_SerializesUnderAdvisoryLock asserts the
// helper acquires a transaction-scoped advisory lock BEFORE issuing CREATE
// EXTENSION and commits afterward — the serialization that stops concurrent
// pod boots from racing into XX000 (#1099).
func TestCreateTimescaleExtensionLocked_SerializesUnderAdvisoryLock(t *testing.T) {
	conn := &recordingConn{}
	db := sql.OpenDB(&recordingConnector{conn: conn})
	defer db.Close()

	if err := createTimescaleExtensionLocked(context.Background(), db); err != nil {
		t.Fatalf("createTimescaleExtensionLocked: %v", err)
	}

	beginIdx := indexOf(conn.events, "BEGIN")
	lockIdx := indexOf(conn.events, "pg_advisory_xact_lock")
	createIdx := indexOf(conn.events, "CREATE EXTENSION IF NOT EXISTS timescaledb")
	commitIdx := indexOf(conn.events, "COMMIT")

	for name, idx := range map[string]int{"BEGIN": beginIdx, "advisory lock": lockIdx, "CREATE EXTENSION": createIdx, "COMMIT": commitIdx} {
		if idx < 0 {
			t.Fatalf("expected %s to be issued; events=%v", name, conn.events)
		}
	}

	// Ordering: BEGIN < advisory lock < CREATE EXTENSION < COMMIT.
	if !(beginIdx < lockIdx && lockIdx < createIdx && createIdx < commitIdx) {
		t.Fatalf("expected BEGIN < advisory lock < CREATE EXTENSION < COMMIT, got events=%v", conn.events)
	}
}

// TestCreateTimescaleExtensionLocked_NilDBNoPanic guards the defensive nil
// branch (mirrors the existing nil-guards in the hooks).
func TestCreateTimescaleExtensionLocked_NilDBNoPanic(t *testing.T) {
	if err := createTimescaleExtensionLocked(context.Background(), nil); err != nil {
		t.Fatalf("expected nil error for nil db, got %v", err)
	}
}

// TestCreateTimescaleExtensionLocked_RollsBackOnCreateFailure ensures a failing
// CREATE EXTENSION is surfaced and the transaction is rolled back (the deferred
// Rollback fires because Commit never runs).
func TestCreateTimescaleExtensionLocked_RollsBackOnCreateFailure(t *testing.T) {
	wantErr := errors.New("boom")
	conn := &recordingConn{execErr: wantErr, failOn: "CREATE EXTENSION"}
	db := sql.OpenDB(&recordingConnector{conn: conn})
	defer db.Close()

	if err := createTimescaleExtensionLocked(context.Background(), db); !errors.Is(err, wantErr) {
		t.Fatalf("expected create error to propagate, got %v", err)
	}
	if indexOf(conn.events, "COMMIT") >= 0 {
		t.Fatalf("expected no COMMIT on failure; events=%v", conn.events)
	}
	if indexOf(conn.events, "ROLLBACK") < 0 {
		t.Fatalf("expected ROLLBACK on failure; events=%v", conn.events)
	}
}
