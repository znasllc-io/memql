package dbtest

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/uptrace/bun/driver/pgdriver"
)

// A contender must wait for the migrating session, rather than spend the
// driver's socket read timeout on one blocking lock query. A short driver
// timeout reproduces the CI failure without requiring a slow migration.
func TestSchemaAdvisoryLockWaitsPastDriverReadTimeout(t *testing.T) {
	testSchemaAdvisoryWait(t, false)
}

func TestSchemaAdvisoryLockWaitHonorsCancellation(t *testing.T) {
	testSchemaAdvisoryWait(t, true)
}

func testSchemaAdvisoryWait(t *testing.T, cancelWait bool) {
	t.Helper()
	dsn := lockRaceScratchDatabase(t)
	ctx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	open := func(options ...pgdriver.Option) *sql.Conn {
		t.Helper()
		db := sql.OpenDB(pgdriver.NewConnector(append([]pgdriver.Option{pgdriver.WithDSN(dsn)}, options...)...))
		t.Cleanup(func() { _ = db.Close() })
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		return conn
	}
	holder := open()
	if _, err := holder.ExecContext(ctx, "SELECT pg_advisory_lock($1)", schemaLockKey); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = holder.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", schemaLockKey) }()

	const socketTimeout = 250 * time.Millisecond
	waiter := open(pgdriver.WithReadTimeout(socketTimeout))
	var waiterPID int
	if err := waiter.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&waiterPID); err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- acquireSchemaLock(waitCtx, waiter) }()

	// Observe the actual first lock attempt, so scheduling cannot turn this
	// into a test that releases the holder before the contender reaches it.
	for {
		var attempted bool
		if err := holder.QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND query LIKE '%advisory_lock%'
		)`, waiterPID).Scan(&attempted); err != nil {
			t.Fatal(err)
		}
		if attempted {
			break
		}
		select {
		case err := <-result:
			t.Fatalf("lock wait returned before its first attempt was observed: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
	if cancelWait {
		cancel()
		select {
		case err := <-result:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("lock wait lost caller cancellation: %v", err)
			}
		case <-ctx.Done():
			t.Fatal("lock wait ignored caller cancellation")
		}
	} else {
		select {
		case err := <-result:
			t.Fatalf("lock wait ended while the migrating session still held it: %v", err)
		case <-time.After(2 * socketTimeout):
		}
	}
	if _, err := holder.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", schemaLockKey); err != nil {
		t.Fatal(err)
	}
	if !cancelWait {
		select {
		case err := <-result:
			if err != nil {
				t.Fatalf("lock wait failed after release: %v", err)
			}
		case <-ctx.Done():
			t.Fatal("lock wait did not acquire after release")
		}
		var acquired bool
		if err := holder.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", schemaLockKey).Scan(&acquired); err != nil {
			t.Fatal(err)
		}
		if acquired {
			t.Fatal("the successful waiter did not retain the lock on its pinned session")
		}
		if _, err := waiter.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", schemaLockKey); err != nil {
			t.Fatal(err)
		}
	}
	var acquired bool
	if err := holder.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", schemaLockKey).Scan(&acquired); err != nil {
		t.Fatal(err)
	}
	if !acquired {
		t.Fatal("the completed or cancelled waiter stranded the advisory lock")
	}
}
