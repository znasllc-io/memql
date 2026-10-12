package memql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/database/dbtest"
)

// AcquireWriteGate tells a wait that ended from every other failure, against a
// real database. A gate is an advisory lock taken in a transaction, so a caller
// that wants one somebody holds waits for it, and the wait ends one of four
// ways without the lock: the pool cancels the waiting statement
// (statement_timeout, 57014), a lock timeout runs out (55P03), the caller's
// deadline passes, or the caller gives up. Those are "somebody else holds this";
// a missing database, a transaction that cannot begin, or a lost connection are
// not, and are returned as they were.
func TestAcquireWriteGateNamesAWaitThatEndedWithoutTheLock(t *testing.T) {
	open := func(t *testing.T, params map[string]any, more ...pgdriver.Option) *sql.DB {
		t.Helper()
		options := append([]pgdriver.Option{pgdriver.WithDSN(dbtest.DSN())}, more...)
		if len(params) > 0 {
			options = append(options, pgdriver.WithConnParams(params))
		}
		db := sql.OpenDB(pgdriver.NewConnector(options...))
		t.Cleanup(func() { _ = db.Close() })
		ping, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := db.PingContext(ping); err != nil {
			dbtest.Unreachable(t, "write gate", dbtest.DSN(), err)
		}
		return db
	}

	// Somebody holds the gate for the whole test. Their pool sets no limits, so
	// nothing ends their hold but the test.
	key := fmt.Sprintf("write-gate-test-%d", time.Now().UnixNano())
	release, err := AcquireWriteGate(context.Background(), open(t, nil), key)
	if err != nil {
		t.Fatalf("taking a free gate: %v", err)
	}
	defer release()

	pgCode := func(err error) string {
		var pgErr pgdriver.Error
		if errors.As(err, &pgErr) {
			return pgErr.Field('C')
		}
		return ""
	}

	for _, c := range []struct {
		name   string
		params map[string]any
		more   []pgdriver.Option
		ctx    func() (context.Context, context.CancelFunc)
		cause  func(error) bool
	}{
		{"the pool cancels the waiting statement", map[string]any{"statement_timeout": "300"}, nil,
			func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), 20*time.Second)
			},
			func(err error) bool { return pgCode(err) == "57014" }},
		{"a lock timeout runs out", map[string]any{"lock_timeout": "300"}, nil,
			func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), 20*time.Second)
			},
			func(err error) bool { return pgCode(err) == "55P03" }},
		{"the caller's deadline passes", nil, nil,
			func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), 300*time.Millisecond)
			},
			func(error) bool { return true }},
		// pgdriver watches the context's deadline and not its cancellation, so a
		// caller who gives up is heard when the driver's own read times out.
		{"the caller gives up", nil, []pgdriver.Option{pgdriver.WithReadTimeout(400 * time.Millisecond)},
			func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				time.AfterFunc(100*time.Millisecond, cancel)
				return ctx, cancel
			},
			func(error) bool { return true }},
		{"the driver's read times out first", nil, []pgdriver.Option{pgdriver.WithReadTimeout(300 * time.Millisecond)},
			func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), 20*time.Second)
			},
			func(error) bool { return true }},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := c.ctx()
			defer cancel()
			started := time.Now()
			_, err := AcquireWriteGate(ctx, open(t, c.params, c.more...), key)
			if err == nil {
				t.Fatal("took a gate somebody holds")
			}
			if !errors.Is(err, ErrWriteGateBusy) {
				t.Errorf("a wait that ended without the lock is %v, which is not ErrWriteGateBusy", err)
			}
			if !c.cause(err) {
				t.Errorf("the original error is not in the chain: %v", err)
			}
			if waited := time.Since(started); waited > 10*time.Second {
				t.Errorf("waited %v", waited)
			}
		})
	}

	t.Run("a failure that is not a wait is returned as it was", func(t *testing.T) {
		closed := open(t, nil)
		_ = closed.Close()
		for name, call := range map[string]func() error{
			"no database": func() error { _, err := AcquireWriteGate(context.Background(), nil, key); return err },
			"a database that cannot begin": func() error {
				_, err := AcquireWriteGate(context.Background(), closed, key)
				return err
			},
		} {
			err := call()
			if err == nil {
				t.Errorf("%s: took a gate", name)
				continue
			}
			if errors.Is(err, ErrWriteGateBusy) {
				t.Errorf("%s: %v is not somebody else holding the gate", name, err)
			}
		}
	})

	t.Run("the connection is lost while waiting", func(t *testing.T) {
		// The statement fails for a reason that is not the wait ending: its backend
		// is terminated. That is a broken connection, and it is returned as it was.
		application := fmt.Sprintf("write-gate-test-%d", time.Now().UnixNano())
		waiter := open(t, nil, pgdriver.WithApplicationName(application))
		admin := open(t, nil)
		done := make(chan error, 1)
		go func() {
			_, err := AcquireWriteGate(context.Background(), waiter, key)
			done <- err
		}()
		deadline := time.Now().Add(10 * time.Second)
		for {
			var terminated bool
			err := admin.QueryRow(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity
				WHERE application_name = $1 AND wait_event_type = 'Lock' LIMIT 1`, application).Scan(&terminated)
			if err == nil && terminated {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the waiting backend never appeared: %v", err)
			}
			time.Sleep(20 * time.Millisecond)
		}
		select {
		case err := <-done:
			if err == nil || errors.Is(err, ErrWriteGateBusy) {
				t.Errorf("a terminated connection is %v, which is not somebody else holding the gate", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the waiter never returned")
		}
	})

	t.Run("no connection to begin with, before the caller's deadline", func(t *testing.T) {
		// One connection, and it is in use: the transaction waits for a connection
		// and the caller's deadline ends the wait.
		pool := open(t, nil)
		pool.SetMaxOpenConns(1)
		conn, err := pool.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		if _, err := AcquireWriteGate(ctx, pool, key+"-other"); !errors.Is(err, ErrWriteGateBusy) || !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("a transaction that waited for a connection until the deadline: %v", err)
		}
	})

	t.Run("a free gate is taken, and taken again once released", func(t *testing.T) {
		free := fmt.Sprintf("%s-free", key)
		pool := open(t, nil)
		first, err := AcquireWriteGate(context.Background(), pool, free)
		if err != nil {
			t.Fatal(err)
		}
		first()
		second, err := AcquireWriteGate(context.Background(), pool, free)
		if err != nil {
			t.Fatalf("a released gate: %v", err)
		}
		second()
	})
}
