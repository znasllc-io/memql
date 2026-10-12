package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// gate_pool_test.go -- the pool write gates run on (memql#6022, F9 fix round 3).
//
// A write gate is a transaction that holds an advisory lock, and so a connection,
// for as long as its holder works. On the request pool that cost the holders their
// own work (the work needs a connection of the same pool) and exposed the lock to
// the request pool's 60 s idle-in-transaction limit, which ends the session and the
// lock with it without a word to the holder. A gate takes its connection from a pool
// of its own.

// TestGateConnParams: a gate session is allowed to sit idle in its transaction
// for as long as a gated phase can take, and carries no statement timeout -- the
// wait for a lock is bounded per acquisition by lock_timeout, not by a limit on
// every statement the session ever runs.
func TestGateConnParams(t *testing.T) {
	t.Setenv(envDBGateIdleInTxTimeoutMs, "")
	t.Setenv(envDBIdleInTxTimeoutMs, "")
	t.Setenv(envDBStatementTimeoutMs, "")
	t.Setenv(envDBIdleSessionTimeoutMs, "")

	params := gateConnParams()

	if got := params["idle_in_transaction_session_timeout"]; got != "300000" {
		t.Errorf("idle_in_transaction_session_timeout = %v, want 300000 (5 min)", got)
	}
	if _, ok := params["statement_timeout"]; ok {
		t.Errorf("a gate session has a statement_timeout: %v", params["statement_timeout"])
	}
	// The orphan reaper is the request pool's too: an idle session is cleared at 5 min.
	if got := params["idle_session_timeout"]; got != "300000" {
		t.Errorf("idle_session_timeout = %v, want 300000", got)
	}
}

// TestGateConnParamsAreNotTheRequestPoolsKnobs: the request pool's limits are the
// ones that made gates unsafe on it, so lowering or raising them must not move
// the gate pool's.
func TestGateConnParamsAreNotTheRequestPoolsKnobs(t *testing.T) {
	t.Setenv(envDBGateIdleInTxTimeoutMs, "")
	t.Setenv(envDBIdleInTxTimeoutMs, "1000")
	t.Setenv(envDBStatementTimeoutMs, "50")

	params := gateConnParams()

	if got := params["idle_in_transaction_session_timeout"]; got != "300000" {
		t.Errorf("the request pool's idle limit moved the gate pool's: %v", got)
	}
	if _, ok := params["statement_timeout"]; ok {
		t.Errorf("the request pool's statement timeout reached a gate session: %v", params["statement_timeout"])
	}
	if got := sessionConnParams()["idle_in_transaction_session_timeout"]; got != "1000" {
		t.Errorf("the request pool lost its own knob: %v", got)
	}
}

func TestGateConnParamsOverride(t *testing.T) {
	t.Setenv(envDBGateIdleInTxTimeoutMs, "180000")

	if got := gateConnParams()["idle_in_transaction_session_timeout"]; got != "180000" {
		t.Fatalf("idle_in_transaction_session_timeout = %v, want 180000", got)
	}

	t.Setenv(envDBGateIdleInTxTimeoutMs, "0")
	if _, ok := gateConnParams()["idle_in_transaction_session_timeout"]; ok {
		t.Fatal("an explicit 0 should leave the server default standing, not set 0")
	}
}

// TestGatePoolSizeFromEnv: four gates per process by default (memql#6022, fix
// round 4) -- enough for the bounded phases that hold them, and what lets six
// Deployments of two replicas fit the smallest preset with every pool counted
// (docs/public/operate/db-connection-budget.md).
func TestGatePoolSizeFromEnv(t *testing.T) {
	t.Setenv(envDBGatePoolSize, "")
	if got := gatePoolSize(); got != 4 || DefaultGatePoolSize != 4 {
		t.Errorf("default gate pool size = %d (exported %d), want 4", got, DefaultGatePoolSize)
	}
	t.Setenv(envDBGatePoolSize, "3")
	if got := gatePoolSize(); got != 3 {
		t.Errorf("gate pool size = %d, want 3", got)
	}
	for _, bad := range []string{"0", "-2", "many"} {
		t.Setenv(envDBGatePoolSize, bad)
		if got := gatePoolSize(); got != DefaultGatePoolSize {
			t.Errorf("MEMQL_DB_GATE_POOL_SIZE=%q gives %d, want the default %d", bad, got, DefaultGatePoolSize)
		}
	}
}

// TestGateDefaultsKeepAGatedPhaseInsideTheirLimits: the numbers other packages
// build on, read from here rather than copied.
func TestGateDefaultsKeepAGatedPhaseInsideTheirLimits(t *testing.T) {
	if DefaultGateIdleInTxTimeout <= DefaultRequestIdleInTxTimeout {
		t.Errorf("a gate session (%s idle) is no safer than the request pool's (%s)", DefaultGateIdleInTxTimeout, DefaultRequestIdleInTxTimeout)
	}
	if DefaultRequestStatementTimeout != 8*time.Second || DefaultRequestIdleInTxTimeout != time.Minute {
		t.Errorf("the request pool's defaults moved (statement %s, idle in transaction %s); the gate documentation names them",
			DefaultRequestStatementTimeout, DefaultRequestIdleInTxTimeout)
	}
}

// TestADatabaseOpensAndClosesItsGatePool: the gate pool is its own *sql.DB,
// opened with the request pool and closed with it.
func TestADatabaseOpensAndClosesItsGatePool(t *testing.T) {
	var requestOpens, gateOpens int
	var base Database
	d := newDirectTestDB(t, fakeOpener(&requestOpens), base.WithGateOpener(fakeOpener(&gateOpens)))

	if d.GateDB() != nil {
		t.Fatal("a gate pool exists before the database started")
	}
	_, cancel, err := d.prepareForRun(context.Background())
	if err != nil {
		t.Fatalf("prepareForRun: %v", err)
	}
	defer cancel()

	gate := d.GateDB()
	if gate == nil {
		t.Fatal("no gate pool was opened")
	}
	if gate == d.DB {
		t.Fatal("the gate pool is the request pool: a gate would take the connections its holder needs")
	}
	if requestOpens != 1 || gateOpens != 1 {
		t.Errorf("opens: request %d, gate %d; want one each", requestOpens, gateOpens)
	}
	if got := gate.Stats().MaxOpenConnections; got != DefaultGatePoolSize {
		t.Errorf("the gate pool is bounded at %d connections, want %d", got, DefaultGatePoolSize)
	}

	d.cleanupAfterRun()
	if d.GateDB() != nil {
		t.Error("the gate pool outlived the database")
	}
	if err := gate.PingContext(context.Background()); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Errorf("the gate pool is still open after the database stopped (ping: %v)", err)
	}
}

// TestAGatePoolFollowsTheConfiguredSize.
func TestAGatePoolFollowsTheConfiguredSize(t *testing.T) {
	t.Setenv(envDBGatePoolSize, "5")
	var requestOpens, gateOpens int
	var base Database
	d := newDirectTestDB(t, fakeOpener(&requestOpens), base.WithGateOpener(fakeOpener(&gateOpens)))
	_, cancel, err := d.prepareForRun(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	defer d.cleanupAfterRun()

	if got := d.GateDB().Stats().MaxOpenConnections; got != 5 {
		t.Fatalf("the gate pool is bounded at %d connections, want 5", got)
	}
}

// TestACustomSQLOpenerLeavesNoGatePool: an opener a test supplies (sqlmock, a
// fake) stands for the whole database, as it does for the migration pool. A
// real gate pool beside it would dial a server the test never meant to reach.
func TestACustomSQLOpenerLeavesNoGatePool(t *testing.T) {
	var opens int
	d := newDirectTestDB(t, fakeOpener(&opens))
	_, cancel, err := d.prepareForRun(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	defer d.cleanupAfterRun()

	if d.GateDB() != nil {
		t.Fatal("a gate pool was opened beside a custom SQL opener")
	}
}

// TestClosePoolClosesTheGatePool: the drain path releases the gate connections
// with the rest.
func TestClosePoolClosesTheGatePool(t *testing.T) {
	var requestOpens, gateOpens int
	var base Database
	d := newDirectTestDB(t, fakeOpener(&requestOpens), base.WithGateOpener(fakeOpener(&gateOpens)))
	_, cancel, err := d.prepareForRun(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	gate := d.GateDB()
	if gate == nil {
		t.Fatal("no gate pool")
	}

	d.ClosePool(context.Background())

	if d.GateDB() != nil {
		t.Error("ClosePool left the gate pool attached")
	}
	if err := gate.PingContext(context.Background()); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Errorf("ClosePool left the gate pool open (ping: %v)", err)
	}
}

// idleConn is a connection a pool can hand out and take back; closes counts the
// ones the pool closed.
type idleConn struct{ closes *atomic.Int32 }

func (c idleConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (c idleConn) Close() error                        { c.closes.Add(1); return nil }
func (c idleConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }

type idleConnector struct{ closes atomic.Int32 }

func (c *idleConnector) Connect(context.Context) (driver.Conn, error) {
	return idleConn{closes: &c.closes}, nil
}
func (c *idleConnector) Driver() driver.Driver { return nil }

// TestAnIdleProcessHoldsNoGateConnection: a gate pool keeps at most two
// connections warm between gates -- a third given back is closed at once -- and
// closes the warm ones once they have gone unused for the pool's idle time, so a
// process that takes no gates holds none. Set by the gate pool itself, not left to
// whichever opener built it.
func TestAnIdleProcessHoldsNoGateConnection(t *testing.T) {
	idle := gatePoolMaxIdleTime
	gatePoolMaxIdleTime = 100 * time.Millisecond
	defer func() { gatePoolMaxIdleTime = idle }()
	connector := &idleConnector{}
	pool, err := openGatePool(func(string, string) (*sql.DB, error) { return sql.OpenDB(connector), nil }, "stub", "stub-dsn", DefaultGatePoolSize)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	ctx := context.Background()
	var held []*sql.Conn
	for range 3 {
		conn, err := pool.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, conn)
	}
	for _, conn := range held {
		_ = conn.Close()
	}
	if st := pool.Stats(); st.OpenConnections != 2 || st.Idle != 2 || connector.closes.Load() != 1 {
		t.Errorf("after three gates: %d open, %d idle, %d closed; want two warm and the third closed at once", st.OpenConnections, st.Idle, connector.closes.Load())
	}
	deadline := time.Now().Add(5 * time.Second)
	for pool.Stats().OpenConnections > 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if st := pool.Stats(); st.OpenConnections != 0 {
		t.Errorf("an idle process still holds %d gate connections %s after its last gate (idle time %s)", st.OpenConnections, 5*time.Second, gatePoolMaxIdleTime)
	}
}
