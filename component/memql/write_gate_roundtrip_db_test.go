package memql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
)

// Delay the real PostgreSQL frontend query packet, not an authority result or
// fake driver. The earlier Calls latency oracle delayed Engine.Execute only;
// the separate gate connection's serial SQL round trips were still localhost.
// No packet contents are retained. Startup/authentication are not delayed.
type gateRoundtripConn struct {
	net.Conn
	delay   *atomic.Int64
	queries *atomic.Int64
}

func (c *gateRoundtripConn) Write(p []byte) (int, error) {
	if len(p) >= 5 && p[0] == 'Q' {
		c.queries.Add(1)
		time.Sleep(time.Duration(c.delay.Load()))
	}
	return c.Conn.Write(p)
}
func gateRoundtripPool(t *testing.T) (*sql.DB, *atomic.Int64, *atomic.Int64) {
	t.Helper()
	if !gateReachable(t) {
		return nil, nil, nil
	}
	delay, queries := new(atomic.Int64), new(atomic.Int64)
	connector := pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()), pgdriver.WithConnParams(map[string]any{
		"statement_timeout": "0", "idle_in_transaction_session_timeout": fmt.Sprint(database.DefaultGateIdleInTxTimeout.Milliseconds()), "lock_timeout": "5s",
	}))
	original := connector.Config().Dialer
	connector.Config().Dialer = func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, e := original(ctx, network, addr)
		if e != nil {
			return nil, e
		}
		return &gateRoundtripConn{Conn: c, delay: delay, queries: queries}, nil
	}
	pool := sql.OpenDB(connector)
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = pool.Close() })
	// Warm pool validation separately, as a running BFF has already done.
	release, e := AcquireWriteGates(context.Background(), pool, gateKey(t, "warm"))
	if e != nil {
		t.Fatal(e)
	}
	release()
	return pool, delay, queries
}

func TestWriteGateFiveKeysTransportLatency(t *testing.T) {
	pool, delay, queries := gateRoundtripPool(t)
	if pool == nil {
		return
	}
	keys := []string{gateKey(t, "room"), gateKey(t, "account-a"), gateKey(t, "key-a"), gateKey(t, "account-b"), gateKey(t, "key-b")}
	delay.Store(int64(120 * time.Millisecond))
	before := queries.Load()
	start := time.Now()
	release, err := AcquireWriteGatesWithin(context.Background(), pool, time.Second, keys...)
	elapsed := time.Since(start)
	used := queries.Load() - before
	delay.Store(0)
	if release != nil {
		defer release()
	}
	t.Logf("five actual keys: elapsed=%s frontend_query_roundtrips=%d injected_delay=120ms err=%v", elapsed, used, err)
	if err != nil {
		t.Fatalf("five uncontended keys spent their bound in gate SQL transport: %v", err)
	}
	if used < 6 {
		t.Fatalf("transport injection missed real BEGIN and five lock statements: %d", used)
	}
	other := gateDB(t, 0)
	for _, key := range keys {
		if !lockedElsewhere(t, other, key) {
			t.Fatal("successful acquisition did not hold every requested key")
		}
	}
}

func TestWriteGateTimeoutConfiguredBeforeLockAndTransactionLocal(t *testing.T) {
	pool, _, _ := gateRoundtripPool(t)
	if pool == nil {
		return
	}
	other := gateDB(t, 0)
	key := gateKey(t, "held")
	unlock := holder(t, other, key)
	start := time.Now()
	release, err := AcquireWriteGatesWithin(context.Background(), pool, 150*time.Millisecond, key)
	if release != nil {
		release()
	}
	elapsed := time.Since(start)
	if !errors.Is(err, ErrWriteGateBusy) || sqlState(err) != "55P03" {
		t.Fatalf("held lock must use new 150ms server timeout, got %v", err)
	}
	if elapsed > time.Second {
		t.Fatalf("used inherited5s timeout rather than per-key bound: %s", elapsed)
	}
	if n := waiters(t, other, key); n != 0 {
		t.Fatalf("%d waiters remain", n)
	}
	unlock()
	var timeout string
	if err := pool.QueryRow("SHOW lock_timeout").Scan(&timeout); err != nil {
		t.Fatal(err)
	}
	if timeout != "5s" {
		t.Fatalf("transaction-local timeout leaked to warm session: %s", timeout)
	}
	release, err = AcquireWriteGatesWithin(context.Background(), pool, time.Second, key)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if lockedElsewhere(t, other, key) {
		t.Fatal("released lock remains held")
	}
}

func TestWriteGateLaterKeyUsesRemainingWait(t *testing.T) {
	pool := deployedGatePool(t, 1)
	if pool == nil {
		return
	}
	other := gateDB(t, 0)
	first, second := gateKey(t, "first"), gateKey(t, "second")
	unlockFirst := holder(t, other, first)
	unlockSecond := holder(t, other, second)
	defer unlockSecond()
	timer := time.AfterFunc(600*time.Millisecond, unlockFirst)
	defer timer.Stop()
	started := time.Now()
	release, err := AcquireWriteGatesWithin(context.Background(), pool, time.Second, first, second)
	if release != nil {
		release()
	}
	if !errors.Is(err, ErrWriteGateBusy) {
		t.Fatalf("second held key must exhaust the original wait: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 1400*time.Millisecond {
		t.Fatalf("later key restarted the wait: %s", elapsed)
	}
	if lockedElsewhere(t, other, first) || waiters(t, other, first) != 0 || waiters(t, other, second) != 0 {
		t.Fatal("failed multi-key acquisition left a partial hold or waiter")
	}
}

func TestWriteGateBorrowedTransactionRetainsEveryLock(t *testing.T) {
	pool := deployedGatePool(t, 1)
	if pool == nil {
		return
	}
	other := gateDB(t, 0)
	keys := []string{gateKey(t, "outer"), gateKey(t, "inner-a"), gateKey(t, "inner-b")}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	outer, err := AcquireGateHold(ctx, pool, time.Second, keys[0])
	if err != nil {
		t.Fatal(err)
	}
	defer outer.Release()
	inner, err := AcquireWriteGatesWithin(outer.Context(ctx), pool, time.Second, keys[1:]...)
	if err != nil {
		t.Fatal(err)
	}
	inner()
	for _, key := range keys {
		if !lockedElsewhere(t, other, key) {
			t.Fatal("inner release lost a lock owned by the outer transaction")
		}
	}
	outer.Release()
	for _, key := range keys {
		if lockedElsewhere(t, other, key) {
			t.Fatal("outer release left a borrowed lock held")
		}
	}
}
