package memql

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/database/dbtest"
)

// write_gate_db_test.go -- the write gate against a real PostgreSQL
// (memql#6022, F9 fix round 2).
//
// A gate is a transaction that holds an advisory lock, and so a connection, until it
// is released. What the tests pin is what that does to the pool it sits on: how many
// connections one caller holds, how many callers can hold one at once, and how a
// wait for the lock ends.

// gateDB opens a pool of its own for a test: maxOpen connections, none for 0.
func gateDB(t *testing.T, maxOpen int) *sql.DB {
	t.Helper()
	reachable, err := dbtest.EnsureSchema(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reachable {
		dbtest.Unreachable(t, "write gate", dbtest.DSN(), nil)
		return nil
	}
	db := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN())))
	db.SetMaxOpenConns(maxOpen)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// gateKey is a key no other test or package uses.
func gateKey(t *testing.T, name string) string {
	t.Helper()
	return fmt.Sprintf("write-gate-test:%s:%s:%d", t.Name(), name, time.Now().UnixNano())
}

// lockedElsewhere reports whether another session holds the gate's advisory lock for
// the key, by trying to take it in a transaction of its own.
func lockedElsewhere(t *testing.T, db *sql.DB, key string) bool {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var got bool
	if err := tx.QueryRow("SELECT pg_try_advisory_xact_lock(1296387409, hashtext($1))", key).Scan(&got); err != nil {
		t.Fatal(err)
	}
	return !got
}

// holder holds the gate's lock for the key from a session of its own until released.
func holder(t *testing.T, db *sql.DB, key string) (release func()) {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("SELECT pg_advisory_xact_lock(1296387409, hashtext($1))", key); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	var once sync.Once
	release = func() { once.Do(func() { _ = tx.Rollback() }) }
	t.Cleanup(release)
	return release
}

// waiters is how many advisory-lock requests the server has not granted.
func waiters(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestAcquireWriteGatesTakesEveryLockOnOneConnection: a caller that needs two locks
// names both and holds ONE connection for them. Two gates taken one after the other
// are two transactions, two connections, and a deadlock when two callers do it on a
// small pool.
func TestAcquireWriteGatesTakesEveryLockOnOneConnection(t *testing.T) {
	pool, other := gateDB(t, 4), gateDB(t, 0)
	first, second := gateKey(t, "site"), gateKey(t, "audience")

	release, err := AcquireWriteGates(context.Background(), pool, first, second)
	if err != nil {
		t.Fatal(err)
	}
	if inUse := pool.Stats().InUse; inUse != 1 {
		t.Errorf("%d connections are held for two locks, want 1", inUse)
	}
	for _, key := range []string{first, second} {
		if !lockedElsewhere(t, other, key) {
			t.Errorf("the lock for %q is not held", key)
		}
	}
	release()
	release() // a second release is harmless
	for _, key := range []string{first, second} {
		if lockedElsewhere(t, other, key) {
			t.Errorf("the lock for %q is still held after release", key)
		}
	}
	if inUse := pool.Stats().InUse; inUse != 0 {
		t.Errorf("%d connections are still held after release", inUse)
	}
}

// TestWriteGateKeysAreTakenInTheOrderGiven: the order is the caller's to keep fixed,
// and the gate keeps it: while it waits for the second key it holds the first.
func TestWriteGateKeysAreTakenInTheOrderGiven(t *testing.T) {
	pool, other := gateDB(t, 4), gateDB(t, 0)
	first, second := gateKey(t, "first"), gateKey(t, "second")
	releaseSecond := holder(t, other, second)

	done := make(chan error, 1)
	var release func()
	go func() {
		var err error
		release, err = AcquireWriteGates(context.Background(), pool, first, second)
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for waiters(t, other) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the gate never started waiting for the second key")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !lockedElsewhere(t, other, first) {
		t.Error("the gate waits for the second key without holding the first")
	}
	releaseSecond()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	release()
}

// TestAGateWaitEndsOnTheServerWithNoWaiterLeft: the wait for a lock is enforced by
// the server (lock_timeout), below the driver's read deadline, so the gate answers
// ErrWriteGateBusy when its wait is over and the server is left with no request for
// the lock. A client-side timeout would leave the backend waiting, to take the lock
// later in a transaction nobody finishes.
func TestAGateWaitEndsOnTheServerWithNoWaiterLeft(t *testing.T) {
	pool, other := gateDB(t, 4), gateDB(t, 0)
	key := gateKey(t, "busy")
	holder(t, other, key)

	started := time.Now()
	_, err := AcquireWriteGatesWithin(context.Background(), pool, 400*time.Millisecond, key)
	elapsed := time.Since(started)

	if !errors.Is(err, ErrWriteGateBusy) {
		t.Fatalf("the gate answered %v, want ErrWriteGateBusy", err)
	}
	if elapsed < 300*time.Millisecond || elapsed > 3*time.Second {
		t.Errorf("a wait of 400ms ended after %s", elapsed)
	}
	if n := waiters(t, other); n != 0 {
		t.Errorf("%d advisory-lock requests are left on the server", n)
	}
	if inUse := pool.Stats().InUse; inUse != 0 {
		t.Errorf("%d connections are still held after the gate gave up", inUse)
	}
	// The connection the failed wait used is clean: the pool takes another gate.
	release, err := AcquireWriteGatesWithin(context.Background(), pool, time.Second, gateKey(t, "free"))
	if err != nil {
		t.Fatalf("the next gate on the pool: %v", err)
	}
	release()
}

// TestAGateWaitIsBelowEveryLimitItDependsOn: the wait is 6 s, under the request
// pool's statement_timeout (8 s, component/database defaultStatementTimeoutMs) and
// under pgdriver's read deadline (10 s). Raising it past either puts the end of the
// wait in the hands of a timer that does not clean up after itself.
func TestAGateWaitIsBelowEveryLimitItDependsOn(t *testing.T) {
	if writeGateWait+writeGateGrace >= 8*time.Second {
		t.Errorf("a gate waits %s (and its statement context %s longer): the request pool cancels a statement at 8 s, and that cancellation would end the wait instead of the gate's own lock_timeout",
			writeGateWait, writeGateGrace)
	}
	if gateHeldTooLong >= 60*time.Second {
		t.Errorf("a gate is reported as held too long at %s: PostgreSQL ends an idle transaction at 60 s (idle_in_transaction_session_timeout) and the lock with it", gateHeldTooLong)
	}
}

// TestGatesNeverTakeTheWholePool: a gate holder needs a connection of the pool it
// holds a connection of, for the work the gate exists to protect. Gates that could
// take every connection leave their holders nothing to work with. With four
// connections three gates can be open, the fourth caller waits for a place -- not
// for a connection -- and the last connection stays free.
func TestGatesNeverTakeTheWholePool(t *testing.T) {
	pool := gateDB(t, 4)
	var releases []func()
	for i := 0; i < 3; i++ {
		release, err := AcquireWriteGates(context.Background(), pool, gateKey(t, fmt.Sprintf("holder%d", i)))
		if err != nil {
			t.Fatalf("gate %d: %v", i, err)
		}
		releases = append(releases, release)
	}
	defer func() {
		for _, release := range releases {
			release()
		}
	}()

	// The work a holder does: it needs a connection, and one is there.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var one int
	if err := pool.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
		t.Fatalf("the gate holders left no connection for their own work: %v", err)
	}

	// A fourth caller waits for a place, and gives up as busy.
	started := time.Now()
	_, err := AcquireWriteGatesWithin(context.Background(), pool, 300*time.Millisecond, gateKey(t, "fourth"))
	if !errors.Is(err, ErrWriteGateBusy) {
		t.Fatalf("a fourth gate on a four-connection pool answered %v, want ErrWriteGateBusy", err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Errorf("the fourth caller waited %s for a 300ms wait", elapsed)
	}

	// A place is given back and the fourth caller takes it.
	releases[0]()
	releases = releases[1:]
	release, err := AcquireWriteGatesWithin(context.Background(), pool, time.Second, gateKey(t, "fourth-again"))
	if err != nil {
		t.Fatalf("after a gate was released: %v", err)
	}
	release()
}

// TestAGateOnAPoolWithNoLimitHasNoLimit: nothing to protect, nothing bounded.
func TestAGateOnAPoolWithNoLimitHasNoLimit(t *testing.T) {
	pool := gateDB(t, 0)
	var releases []func()
	for i := 0; i < 8; i++ {
		release, err := AcquireWriteGates(context.Background(), pool, gateKey(t, fmt.Sprintf("n%d", i)))
		if err != nil {
			t.Fatalf("gate %d: %v", i, err)
		}
		releases = append(releases, release)
	}
	for _, release := range releases {
		release()
	}
}

// TestAGateHeldTooLongSaysSo: PostgreSQL ends an idle transaction, and its lock,
// without telling the holder. The gate cannot stop that; it can say that it was
// held long enough for it to have happened.
func TestAGateHeldTooLongSaysSo(t *testing.T) {
	pool := gateDB(t, 4)
	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	defer slog.SetDefault(previous)

	release, err := AcquireWriteGates(context.Background(), pool, gateKey(t, "slow"))
	if err != nil {
		t.Fatal(err)
	}
	// Held for a moment: nothing to say.
	release()
	if logged.Len() != 0 {
		t.Fatalf("a short hold was reported: %s", logged.String())
	}

	// Held past the limit.
	limit := gateHeldTooLong
	gateHeldTooLong = 50 * time.Millisecond
	defer func() { gateHeldTooLong = limit }()
	release, err = AcquireWriteGates(context.Background(), pool, gateKey(t, "slow-again"))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(120 * time.Millisecond)
	release()
	if !strings.Contains(logged.String(), "idle_in_transaction_session_timeout") {
		t.Errorf("a gate held past its limit did not say so: %q", logged.String())
	}
}
