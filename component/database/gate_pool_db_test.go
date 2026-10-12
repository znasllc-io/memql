package database_test

import (
	"context"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
)

// THE GATE POOL AGAINST A REAL DATABASE (memql#6022, F9 fix round 3).
//
// What the unit tests in gate_pool_test.go say about the parameters, these say
// about the sessions PostgreSQL actually gives the pool.

func TestAGatePoolSessionAllowsALongIdleTransactionAndHasNoStatementTimeout(t *testing.T) {
	reachable, err := dbtest.EnsureSchema(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reachable {
		dbtest.Unreachable(t, "gate pool", dbtest.DSN(), nil)
		return
	}
	pool, err := database.NewGatePool(dbtest.DSN(), 3)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	var idle, statement, idleSession string
	ctx := context.Background()
	if err := pool.QueryRowContext(ctx, "SHOW idle_in_transaction_session_timeout").Scan(&idle); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRowContext(ctx, "SHOW statement_timeout").Scan(&statement); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRowContext(ctx, "SHOW idle_session_timeout").Scan(&idleSession); err != nil {
		t.Fatal(err)
	}
	if idle != "5min" {
		t.Errorf("a gate session may sit idle in a transaction for %q, want 5min", idle)
	}
	if statement != "0" {
		t.Errorf("a gate session has a statement_timeout of %q, want none", statement)
	}
	if idleSession != "5min" {
		t.Errorf("a gate session may sit idle for %q, want 5min", idleSession)
	}
	if got := pool.Stats().MaxOpenConnections; got != 3 {
		t.Errorf("the pool is bounded at %d connections, want 3", got)
	}
}

// TestAGatePoolIsBoundedBySize: the number of gates open at once is the size of
// the pool, and the extra caller waits for a connection of the gate pool.
func TestAGatePoolIsBoundedBySize(t *testing.T) {
	reachable, err := dbtest.EnsureSchema(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reachable {
		dbtest.Unreachable(t, "gate pool", dbtest.DSN(), nil)
		return
	}
	pool, err := database.NewGatePool(dbtest.DSN(), 2)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	ctx := context.Background()
	first, err := pool.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := pool.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	waiting, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	if third, err := pool.Conn(waiting); err == nil {
		third.Close()
		t.Fatal("a third connection was handed out of a pool of two")
	}
}
