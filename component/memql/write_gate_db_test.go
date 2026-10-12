package memql

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
)

// write_gate_db_test.go -- the write gate against a real PostgreSQL
// (memql#6022, F9 fix rounds 2 and 3).
//
// A gate is a transaction that holds an advisory lock, and so a connection, until
// it is released. What the tests pin is where that connection comes from (a pool
// of its own, never the one its holder works on), what the session is allowed to
// do while it waits (sit idle in the transaction for minutes), and how a wait for
// the lock ends.

// gateReachable reports whether the test database is up, failing under
// MEMQL_REQUIRE_DB=1 and skipping otherwise.
func gateReachable(t *testing.T) bool {
	t.Helper()
	reachable, err := dbtest.EnsureSchema(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reachable {
		dbtest.Unreachable(t, "write gate", dbtest.DSN(), nil)
		return false
	}
	return true
}

// gateDB opens a plain pool for a test: maxOpen connections, none for 0. Its
// sessions carry no limits of their own, like a bare connection to the test
// server.
func gateDB(t *testing.T, maxOpen int) *sql.DB {
	t.Helper()
	if !gateReachable(t) {
		return nil
	}
	db := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN())))
	db.SetMaxOpenConns(maxOpen)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// deployedGatePool is a gate pool as the Database component opens it: the
// deployed session parameters and the given size.
func deployedGatePool(t *testing.T, size int) *sql.DB {
	t.Helper()
	if !gateReachable(t) {
		return nil
	}
	db, err := database.NewGatePool(dbtest.DSN(), size)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// requestLikePool is a pool whose sessions carry the request pool's deployed
// limits: a statement timeout of 8 s and 60 s idle in a transaction.
func requestLikePool(t *testing.T, size int) *sql.DB {
	t.Helper()
	if !gateReachable(t) {
		return nil
	}
	db := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()), pgdriver.WithConnParams(map[string]any{
		"statement_timeout":                   fmt.Sprint(database.DefaultRequestStatementTimeout.Milliseconds()),
		"idle_in_transaction_session_timeout": fmt.Sprint(database.DefaultRequestIdleInTxTimeout.Milliseconds()),
	})))
	db.SetMaxOpenConns(size)
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

// waiters is how many requests for the gate's advisory lock for the key the server
// has not granted. Only this key's: the database is shared with whatever else is
// running against it, and a waiter of someone else's is not a leftover of this test.
func waiters(t *testing.T, db *sql.DB, key string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted
		AND objsubid = 2 AND classid = 1296387409::oid AND objid = hashtext($1)::oid`, key).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestAcquireWriteGatesTakesEveryLockOnOneConnection: a caller that needs two locks
// names both and holds ONE connection for them.
func TestAcquireWriteGatesTakesEveryLockOnOneConnection(t *testing.T) {
	pool, other := deployedGatePool(t, 4), gateDB(t, 0)
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
	pool, other := deployedGatePool(t, 4), gateDB(t, 0)
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
	for waiters(t, other, second) == 0 {
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
	pool, other := deployedGatePool(t, 4), gateDB(t, 0)
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
	if n := waiters(t, other, key); n != 0 {
		t.Errorf("%d requests for the gate's lock are left on the server", n)
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

// TestTheLockWaitEndsAtTheCallersDeadline: a caller with 300 ms left does not
// wait six seconds for a lock. The server is told to give up when the caller's
// deadline is nearly up -- not at the gate's default wait, which would leave a
// request for the lock on the server long after the caller stopped listening.
func TestTheLockWaitEndsAtTheCallersDeadline(t *testing.T) {
	pool, other := deployedGatePool(t, 4), gateDB(t, 0)
	key := gateKey(t, "caller-deadline")
	holder(t, other, key)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := AcquireWriteGates(ctx, pool, key)
	elapsed := time.Since(started)

	if err == nil {
		t.Fatal("the gate was taken while another session held it")
	}
	if !errors.Is(err, ErrWriteGateBusy) && !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the gate answered %v, want the gate's busy answer or the caller's own deadline", err)
	}
	if elapsed > 1500*time.Millisecond {
		t.Errorf("a caller with 300ms left waited %s for a lock", elapsed)
	}
	// The server's request for the lock ends with the wait, not seconds later.
	deadline := time.Now().Add(time.Second)
	for waiters(t, other, key) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("a request for the lock is still waiting on the server %s after the caller's deadline", time.Since(started).Round(time.Millisecond))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if inUse := pool.Stats().InUse; inUse != 0 {
		t.Errorf("%d connections are still held", inUse)
	}
}

// TestAGateWaitIsBelowEveryLimitItDependsOn: the lock wait is below the driver's
// read deadline -- the CLIENT's, which cannot tell the server to stop waiting --
// and a gated phase is short against the gate session's idle limit. The numbers
// are read from where they are set, not repeated here.
func TestAGateWaitIsBelowEveryLimitItDependsOn(t *testing.T) {
	read := pgdriver.NewConnector().Config().ReadTimeout
	if read <= 0 {
		t.Fatalf("pgdriver reports no read deadline (%s); the test needs the number it enforces", read)
	}
	if writeGateWait+writeGateGrace >= read {
		t.Errorf("a gate waits %s (and its statement context %s longer): the driver abandons a read at %s, which would end the wait instead of the gate's own lock_timeout",
			writeGateWait, writeGateGrace, read)
	}
	// A gate session carries no statement timeout of its own, so nothing else ends
	// the wait (gate_pool_test.go pins the parameters; the session is checked next).
	if gateHeldTooLong >= database.DefaultGateIdleInTxTimeout {
		t.Errorf("a gate is reported as held too long at %s: the gate session is ended by PostgreSQL at %s, so the report would come after the lock was gone",
			gateHeldTooLong, database.DefaultGateIdleInTxTimeout)
	}
	// And the pool a gate must be refused on is the one it is not built for.
	if database.DefaultRequestIdleInTxTimeout >= minGateIdleTimeout {
		t.Errorf("the request pool ends an idle transaction at %s, which the gate would accept (minimum %s)",
			database.DefaultRequestIdleInTxTimeout, minGateIdleTimeout)
	}
	if minGateIdleTimeout >= database.DefaultGateIdleInTxTimeout {
		t.Errorf("the gate pool's idle limit (%s) is not above the minimum a gate accepts (%s)", database.DefaultGateIdleInTxTimeout, minGateIdleTimeout)
	}
}

// TestAGateSessionHasNoStatementTimeoutToCutItsWait: against the real pool the
// Database component opens, a lock wait of six seconds (and a hold of minutes)
// is not cut by a statement timeout.
func TestAGateSessionHasNoStatementTimeoutToCutItsWait(t *testing.T) {
	pool, other := deployedGatePool(t, 2), gateDB(t, 0)
	key := gateKey(t, "slow-holder")
	release := holder(t, other, key)
	defer release()

	// A wait that would be cancelled by the request pool's statement timeout
	// (8 s) is not: it ends at the gate's own wait.
	started := time.Now()
	_, err := AcquireWriteGatesWithin(context.Background(), pool, 1500*time.Millisecond, key)
	if !errors.Is(err, ErrWriteGateBusy) {
		t.Fatalf("the gate answered %v, want ErrWriteGateBusy", err)
	}
	if elapsed := time.Since(started); elapsed < 1200*time.Millisecond {
		t.Errorf("a wait of 1.5s ended after %s", elapsed)
	}
	var statement string
	conn, err := pool.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.QueryRowContext(context.Background(), "SHOW statement_timeout").Scan(&statement); err != nil {
		t.Fatal(err)
	}
	if statement != "0" {
		t.Errorf("a gate session has a statement_timeout of %q", statement)
	}
}

// TestGatesTakeNoConnectionFromTheRequestPool: a gate holder needs a connection
// of the request pool for the work the gate exists to protect, and with gates on
// a pool of their own it always has one. Six gates are held; the request pool,
// four connections, runs four statements at once.
func TestGatesTakeNoConnectionFromTheRequestPool(t *testing.T) {
	gates, requests := deployedGatePool(t, 8), requestLikePool(t, 4)

	var releases []func()
	defer func() {
		for _, release := range releases {
			release()
		}
	}()
	for i := 0; i < 6; i++ {
		release, err := AcquireWriteGates(context.Background(), gates, gateKey(t, fmt.Sprintf("holder%d", i)))
		if err != nil {
			t.Fatalf("gate %d: %v", i, err)
		}
		releases = append(releases, release)
	}
	if inUse := gates.Stats().InUse; inUse != 6 {
		t.Fatalf("%d gate connections are in use, want 6", inUse)
	}
	if inUse := requests.Stats().InUse; inUse != 0 {
		t.Fatalf("%d request-pool connections are in use by gates, want none", inUse)
	}

	// The holders' work: four statements at once on the four connections, each
	// taking a fifth of a second. Waiting for a connection would take twice that.
	started := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := requests.ExecContext(ctx, "SELECT pg_sleep(0.2)")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("the work under the gates: %v", err)
		}
	}
	if elapsed := time.Since(started); elapsed > 1500*time.Millisecond {
		t.Errorf("four statements on four request connections took %s: the work waited for connections", elapsed)
	}
}

// TestThreeCallersEachHoldingOneGateAndAskingForASecondComplete: a caller that
// holds a gate and asks for another needs a second connection of the gate pool.
// With the pool the size it is deployed at (four), three callers doing it at once
// hold three and share the fourth: each second gate is short, and the caller that
// finishes gives its connections back to the next. (On a pool of three they
// deadlock: each holds the connection the others wait for. See the mutation check
// in the round report.) Flows that need several locks name them in one
// acquisition instead; this is the nesting that remains -- the transport's own
// lock inside a send's, a goal's budget inside an approval's.
func TestThreeCallersEachHoldingOneGateAndAskingForASecondComplete(t *testing.T) {
	pool := deployedGatePool(t, database.DefaultGatePoolSize)

	var holding, finished sync.WaitGroup
	allHolding := make(chan struct{})
	holding.Add(3)
	errs := make(chan error, 3)
	for i := 0; i < 3; i++ {
		finished.Add(1)
		go func(i int) {
			defer finished.Done()
			first, err := AcquireWriteGatesWithin(context.Background(), pool, 3*time.Second, gateKey(t, fmt.Sprintf("outer%d", i)))
			if err != nil {
				holding.Done()
				errs <- fmt.Errorf("caller %d's first gate: %w", i, err)
				return
			}
			defer first()
			holding.Done()
			<-allHolding // every caller holds its first gate before any asks for a second
			second, err := AcquireWriteGatesWithin(context.Background(), pool, 3*time.Second, gateKey(t, fmt.Sprintf("inner%d", i)))
			if err != nil {
				errs <- fmt.Errorf("caller %d's second gate: %w", i, err)
				return
			}
			second()
			errs <- nil
		}(i)
	}
	holding.Wait()
	close(allHolding)
	finished.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
}

// TestMoreGatesThanThePoolHoldsWaitForAConnectionAndThenAreBusy: the number of
// gates open at once is the size of the pool and nothing else. The caller that
// finds them all held waits for a connection of the GATE pool and gives up as
// busy; when one is given back it takes it.
func TestMoreGatesThanThePoolHoldsWaitForAConnectionAndThenAreBusy(t *testing.T) {
	pool := deployedGatePool(t, 2)
	first, err := AcquireWriteGates(context.Background(), pool, gateKey(t, "one"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := AcquireWriteGates(context.Background(), pool, gateKey(t, "two"))
	if err != nil {
		t.Fatal(err)
	}
	defer second()

	started := time.Now()
	_, err = AcquireWriteGatesWithin(context.Background(), pool, 300*time.Millisecond, gateKey(t, "three"))
	if !errors.Is(err, ErrWriteGateBusy) {
		t.Fatalf("a third gate on a pool of two answered %v, want ErrWriteGateBusy", err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Errorf("the third caller waited %s for a 300ms wait", elapsed)
	}

	first()
	release, err := AcquireWriteGatesWithin(context.Background(), pool, time.Second, gateKey(t, "three-again"))
	if err != nil {
		t.Fatalf("after a gate was released: %v", err)
	}
	release()
}

// TestAGateIsRefusedOnAPoolThatEndsIdleTransactionsEarly: the request pool ends a
// session idle in a transaction after 60 s, and the advisory lock goes with it
// without a word to the holder. A gate on such a pool is a lock that may be gone
// while its holder goes on writing, so the gate says no -- at once, loudly, on
// the first use -- instead of working until the day a phase outlasts the limit.
func TestAGateIsRefusedOnAPoolThatEndsIdleTransactionsEarly(t *testing.T) {
	pool, other := requestLikePool(t, 4), gateDB(t, 0)
	key := gateKey(t, "wrong-pool")

	release, err := AcquireWriteGates(context.Background(), pool, key)
	if err == nil {
		release()
		t.Fatal("a gate was taken on a pool that ends idle transactions after 60 s")
	}
	if !errors.Is(err, ErrWriteGatePoolUnsuitable) {
		t.Fatalf("the gate answered %v, want ErrWriteGatePoolUnsuitable", err)
	}
	if lockedElsewhere(t, other, key) {
		t.Error("the refused gate left its lock held")
	}
	if inUse := pool.Stats().InUse; inUse != 0 {
		t.Errorf("%d connections are held by the refused gate", inUse)
	}
}

// TestAGateIsAcceptedOnAPoolWithNoIdleLimit: a session with no
// idle_in_transaction_session_timeout (the test server's, an operator's who set 0)
// cannot lose its lock that way.
func TestAGateIsAcceptedOnAPoolWithNoIdleLimit(t *testing.T) {
	pool := gateDB(t, 2)
	release, err := AcquireWriteGates(context.Background(), pool, gateKey(t, "unlimited"))
	if err != nil {
		t.Fatal(err)
	}
	release()
}

// TestAGateIsReleasedWithItsContext: the transaction is bound to the caller's
// context, so a caller that is cancelled gives the lock back instead of holding
// it for a goroutine that is gone.
func TestAGateIsReleasedWithItsContext(t *testing.T) {
	pool, other := deployedGatePool(t, 2), gateDB(t, 0)
	key := gateKey(t, "bound")
	ctx, cancel := context.WithCancel(context.Background())
	release, err := AcquireWriteGates(ctx, pool, key)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if !lockedElsewhere(t, other, key) {
		t.Fatal("the lock is not held")
	}

	cancel()
	deadline := time.Now().Add(3 * time.Second)
	for lockedElsewhere(t, other, key) {
		if time.Now().After(deadline) {
			t.Fatal("the lock outlived its caller's context")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestAGateWithoutAPoolIsUnavailable: no pool is no coordination, and the
// caller is told so rather than handed a gate that guards nothing.
func TestAGateWithoutAPoolIsUnavailable(t *testing.T) {
	if _, err := AcquireWriteGates(context.Background(), nil, "k"); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("a gate without a pool answered %v, want 'unavailable'", err)
	}
}

// TestAGateHeldTooLongSaysSo: PostgreSQL ends an idle transaction, and its lock,
// without telling the holder. The gate cannot stop that; it can say that it was
// held long enough for it to have happened.
func TestAGateHeldTooLongSaysSo(t *testing.T) {
	pool := deployedGatePool(t, 4)
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
	if strings.Contains(logged.String(), "idle_in_transaction_session_timeout") {
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

// gateBackend is the backend process that holds the gate's advisory lock for the
// key, found the way an operator would: from pg_locks.
func gateBackend(t *testing.T, db *sql.DB, key string) int {
	t.Helper()
	var pid int
	if err := db.QueryRow(`SELECT pid FROM pg_locks WHERE locktype = 'advisory' AND granted
		AND objsubid = 2 AND classid = 1296387409::oid AND objid = hashtext($1)::oid`, key).Scan(&pid); err != nil {
		t.Fatalf("no session holds the lock for %q: %v", key, err)
	}
	return pid
}

// TestAHeldGateVerifiesWhileItsSessionLives: a holder can ask, before it writes,
// whether the locks it was given are still held.
func TestAHeldGateVerifiesWhileItsSessionLives(t *testing.T) {
	pool := deployedGatePool(t, 2)
	hold, err := AcquireGateHold(context.Background(), pool, 0, gateKey(t, "site"), gateKey(t, "audience"))
	if err != nil {
		t.Fatal(err)
	}
	if err := hold.Verify(context.Background()); err != nil {
		t.Fatalf("a live gate failed its verification: %v", err)
	}
	hold.Release()
	if err := hold.Verify(context.Background()); !errors.Is(err, ErrWriteGateLost) {
		t.Fatalf("a released gate verified as %v, want ErrWriteGateLost", err)
	}
	hold.Release() // a second release is harmless
}

// TestAHeldGateWhoseSessionWasEndedFailsItsVerification: PostgreSQL ends a session
// for reasons of its own -- an idle limit, an administrator, a failover -- and the
// advisory lock goes with it without a word to the holder. The holder finds out
// by asking, before it writes, and does not write.
func TestAHeldGateWhoseSessionWasEndedFailsItsVerification(t *testing.T) {
	pool, other := deployedGatePool(t, 2), gateDB(t, 0)
	key := gateKey(t, "ended")
	hold, err := AcquireGateHold(context.Background(), pool, 0, key)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Release()
	pid := gateBackend(t, other, key)

	if _, err := other.Exec("SELECT pg_terminate_backend($1)", pid); err != nil {
		t.Fatal(err)
	}
	// The server drops the lock; the holder has not been told.
	deadline := time.Now().Add(3 * time.Second)
	for lockedElsewhere(t, other, key) {
		if time.Now().After(deadline) {
			t.Fatal("the terminated session still holds the lock")
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := hold.Verify(context.Background()); !errors.Is(err, ErrWriteGateLost) {
		t.Fatalf("a gate whose session was ended verified as %v, want ErrWriteGateLost", err)
	}
}

// TestAVerificationPastTheCallersDeadlineIsTheCallers: not a lost lock.
func TestAVerificationPastTheCallersDeadlineIsTheCallers(t *testing.T) {
	pool := deployedGatePool(t, 2)
	hold, err := AcquireGateHold(context.Background(), pool, 0, gateKey(t, "late"))
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Release()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := hold.Verify(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("a verification on a cancelled context answered %v, want the context's error", err)
	}
}

// TestABusyGateIsRecognisedByTypeAndByItsWords: a builtin called through the
// engine and a step of a workflow keep the message and lose the type, and the
// callers that send mail still have to tell "nothing was attempted" from a failure.
func TestABusyGateIsRecognisedByTypeAndByItsWords(t *testing.T) {
	for name, err := range map[string]error{
		"the error itself":      ErrWriteGateBusy,
		"wrapped":               fmt.Errorf("campaigns.sendToRecipient: %w", ErrWriteGateBusy),
		"only its words remain": errors.New("builtin campaignSendToRecipient: " + ErrWriteGateBusy.Error()),
	} {
		if !IsWriteGateBusy(err) {
			t.Errorf("%s: not recognised as a busy gate: %v", name, err)
		}
	}
	for name, err := range map[string]error{
		"nil":                nil,
		"another failure":    errors.New("Azure did not accept the message"),
		"a lost gate":        ErrWriteGateLost,
		"an unsuitable pool": ErrWriteGatePoolUnsuitable,
	} {
		if IsWriteGateBusy(err) {
			t.Errorf("%s: recognised as a busy gate: %v", name, err)
		}
	}
}

// TestTheBusyAnswerIsExactlyTheEndsOfAWait: ErrWriteGateBusy -- wrapped, so the
// cause is still there -- is what a gate answers for the lock_timeout (55P03), a
// cancelled statement (57014) and a wait context that ended; and nothing else is
// busy. A terminated backend, a connection that was lost, and a net i/o timeout on
// a context that is still live are not a lock that was held by someone: they pass
// through as the engine reported them. The errors below are the real ones, from a
// real server.
func TestTheBusyAnswerIsExactlyTheEndsOfAWait(t *testing.T) {
	pool, other := deployedGatePool(t, 4), gateDB(t, 0)
	live := context.Background()

	execFails := func(t *testing.T, db *sql.DB, ctx context.Context, setup, statement string, args ...any) error {
		t.Helper()
		tx, err := db.BeginTx(live, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if setup != "" {
			if _, err := tx.Exec(setup); err != nil {
				t.Fatal(err)
			}
		}
		_, err = tx.ExecContext(ctx, statement, args...)
		if err == nil {
			t.Fatalf("%q did not fail", statement)
		}
		return err
	}

	t.Run("the server's lock_timeout (55P03) is busy", func(t *testing.T) {
		key := gateKey(t, "55P03")
		holder(t, other, key)
		err := execFails(t, pool, live, "SELECT set_config('lock_timeout', '60', true)", "SELECT pg_advisory_xact_lock(1296387409, hashtext($1))", key)
		if got := sqlState(err); got != "55P03" {
			t.Fatalf("the test produced SQLSTATE %q, not 55P03: %v", got, err)
		}
		if answer := gateWaitError(err, live); !errors.Is(answer, ErrWriteGateBusy) || sqlState(answer) != "55P03" {
			t.Errorf("a lock_timeout answered %v, want ErrWriteGateBusy with the server's refusal inside it", answer)
		}
	})

	t.Run("a cancelled statement (57014) is busy", func(t *testing.T) {
		err := execFails(t, pool, live, "SET LOCAL statement_timeout = '60ms'", "SELECT pg_sleep(2)")
		if got := sqlState(err); got != "57014" {
			t.Fatalf("the test produced SQLSTATE %q, not 57014: %v", got, err)
		}
		if answer := gateWaitError(err, live); !errors.Is(answer, ErrWriteGateBusy) {
			t.Errorf("a cancelled statement answered %v, want ErrWriteGateBusy", answer)
		}
	})

	t.Run("a wait context that ended is busy, by its own deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(live, 80*time.Millisecond)
		defer cancel()
		err := execFails(t, pool, ctx, "", "SELECT pg_sleep(2)")
		answer := gateWaitError(err, ctx)
		if !errors.Is(answer, ErrWriteGateBusy) || !errors.Is(answer, context.DeadlineExceeded) {
			t.Errorf("a wait that ran out of time answered %v, want ErrWriteGateBusy and the deadline", answer)
		}
	})

	t.Run("a wait context that was cancelled is busy, whatever the driver reported", func(t *testing.T) {
		// pgdriver reads only a context's deadline, never its cancellation, so the
		// statement itself runs on until the server ends it; the gate asks the
		// context, not the error.
		ctx, cancel := context.WithCancel(live)
		cancel()
		for _, reported := range []error{errors.New("driver: bad connection"), io.EOF, &net.OpError{Op: "read", Err: os.ErrDeadlineExceeded}} {
			answer := gateWaitError(reported, ctx)
			if !errors.Is(answer, ErrWriteGateBusy) || !errors.Is(answer, context.Canceled) {
				t.Errorf("a cancelled wait answered %v for %v, want ErrWriteGateBusy and the cancellation", answer, reported)
			}
		}
	})

	t.Run("a net i/o timeout on a live context is not busy", func(t *testing.T) {
		if !gateReachable(t) {
			return
		}
		slow := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()), pgdriver.WithReadTimeout(150*time.Millisecond)))
		defer slow.Close()
		err := execFails(t, slow, live, "", "SELECT pg_sleep(2)")
		var netErr net.Error
		if !errors.As(err, &netErr) || !netErr.Timeout() || sqlState(err) != "" {
			t.Fatalf("the test did not produce a net i/o timeout: %v", err)
		}
		if answer := gateWaitError(err, live); IsWriteGateBusy(answer) || answer.Error() != err.Error() {
			t.Errorf("a net i/o timeout answered %v, want it unchanged and not busy", answer)
		}
	})

	t.Run("a terminated backend is not busy", func(t *testing.T) {
		conn, err := pool.Conn(live)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		var pid int
		if err := conn.QueryRowContext(live, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
			t.Fatal(err)
		}
		if _, err := other.Exec("SELECT pg_terminate_backend($1)", pid); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
		_, err = conn.ExecContext(live, "SELECT 1")
		if err == nil {
			t.Fatal("a statement ran on a terminated backend")
		}
		if answer := gateWaitError(err, live); IsWriteGateBusy(answer) || answer.Error() != err.Error() {
			t.Errorf("a terminated backend answered %v, want it unchanged and not busy", answer)
		}
	})

	t.Run("another SQL error is not busy", func(t *testing.T) {
		err := execFails(t, pool, live, "", "SELEKT 1")
		if answer := gateWaitError(err, live); IsWriteGateBusy(answer) || answer.Error() != err.Error() {
			t.Errorf("a syntax error answered %v, want it unchanged and not busy", answer)
		}
	})

	t.Run("a pool the gate refused stays refused", func(t *testing.T) {
		refused := fmt.Errorf("%w (this pool's limit is 1m0s)", ErrWriteGatePoolUnsuitable)
		if answer := gateWaitError(refused, live); answer.Error() != refused.Error() || IsWriteGateBusy(answer) {
			t.Errorf("a refused pool answered %v, want it unchanged", answer)
		}
	})

	t.Run("through the gate: a caller who cancels while the gate waits gets the busy answer and the cancellation", func(t *testing.T) {
		key := gateKey(t, "cancel-while-waiting")
		holder(t, other, key)
		ctx, cancel := context.WithCancel(live)
		go func() { time.Sleep(100 * time.Millisecond); cancel() }()
		// The driver does not interrupt a statement for a cancellation, so the
		// cancellation is read when the bounded wait ends.
		_, err := AcquireWriteGatesWithin(ctx, pool, 400*time.Millisecond, key)
		if !errors.Is(err, ErrWriteGateBusy) || !errors.Is(err, context.Canceled) {
			t.Errorf("a gate waiting for a held lock answered %v when the caller cancelled, want ErrWriteGateBusy and the cancellation", err)
		}
	})

	t.Run("through the gate: its own wait running out is busy", func(t *testing.T) {
		key := gateKey(t, "own-wait")
		holder(t, other, key)
		_, err := AcquireWriteGatesWithin(live, pool, 200*time.Millisecond, key)
		if !errors.Is(err, ErrWriteGateBusy) {
			t.Errorf("a gate whose wait ran out answered %v, want ErrWriteGateBusy", err)
		}
	})

	t.Run("through the gate: a pool that is closed is not busy", func(t *testing.T) {
		closed, err := database.NewGatePool(dbtest.DSN(), 2)
		if err != nil {
			t.Fatal(err)
		}
		_ = closed.Close()
		_, err = AcquireWriteGates(live, closed, gateKey(t, "closed"))
		if err == nil || IsWriteGateBusy(err) {
			t.Errorf("a gate on a closed pool answered %v, want the pool's own error and not busy", err)
		}
	})
}
