package memql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
)

// write_gate.go -- the write gate (memql#6022, F9 fix rounds 2 and 3).
//
// A write gate serialises a multi-statement capability across replicas: a
// transaction that takes a transaction-scoped advisory lock and sits idle in it
// until its holder releases it.
//
// # Where the connection comes from
//
// A gate is taken on a pool of its own (component/database, gate_pool.go;
// MEMQL_DB_GATE_POOL_SIZE connections, 8 by default), never on the request pool
// its holder does its work on. Gates on the request pool took the connections the
// work under them needed, and the request pool ends a session idle in a transaction
// after 60 s -- ending the advisory lock with it, silently, while the holder went
// on writing. The gate pool's sessions are allowed minutes idle, and the callers
// bound their own gated phases far below that.
//
// The pool is the only limit on how many gates are open at once. There is no
// per-process slot count: with a pool that carries nothing else, "every connection
// is held by a gate" is not a deadlock, just a caller that waits (up to the wait
// below) and is told the pod is busy.
//
// A gate REFUSES a pool whose sessions are ended earlier than minGateIdleTimeout
// (the request pool's 60 s): handing a gate the wrong pool fails on its first use,
// loudly, instead of working until the day a phase outlasts the limit.
//
// # The wait
//
// A gate waits at most writeGateWait, in total, for a connection of the pool and
// its locks. The lock part is enforced by the SERVER with a lock_timeout on the
// gate's own transaction, set from what is left of the wait -- and of the CALLER'S
// deadline, whichever is sooner. A client-side timeout would leave the request
// for the lock on the server: pgdriver's read deadline (10 s) abandons the read
// and the backend keeps waiting, to take the lock later in a transaction nobody
// will finish. The wait stays below that deadline, so the server's refusal is
// always read.

// ErrWriteGateBusy is what a write gate answers when it could not be taken in time:
// another holder has the lock, or every connection of the gate pool is held.
// NOTHING WAS ATTEMPTED: the caller may try again later, and a path that sends
// mail must treat it as exactly that -- never as a final outcome of the message,
// because no send began. It should say so in words a person can read when it
// reaches one; the cause is not theirs to see.
var ErrWriteGateBusy = errors.New("shared write coordination is busy; try again")

// ErrWriteGatePoolUnsuitable is what a write gate answers when the pool it was
// given ends idle transactions sooner than a gated phase can be trusted to
// finish -- the request pool, 60 s. See the header.
var ErrWriteGatePoolUnsuitable = errors.New("write gates need a pool whose sessions may sit idle in a transaction for minutes, not the request pool")

// writeGateWait is how long a write gate waits, in total, for a connection of the
// gate pool and its locks. It is below pgdriver's read deadline (10 s), the
// client's: a lock wait that outlasts it fails with "i/o timeout" while its backend
// keeps waiting (the header). The gate session carries no statement_timeout of its
// own, so nothing on the server cuts the wait but the lock_timeout the gate sets.
const writeGateWait = 6 * time.Second

// writeGateGrace is how much longer than the wait the client keeps a lock statement's
// context, so the server's lock_timeout error is read before the client gives up.
const writeGateGrace = time.Second

// gateCallerMargin is how much sooner than the caller's own deadline the server is
// told to give up, so its refusal arrives before the caller's context cancels the
// read (and abandons the backend) rather than after.
const gateCallerMargin = 100 * time.Millisecond

// minGateIdleTimeout is the shortest idle-in-transaction limit a gate accepts on
// its pool: every gated phase is bounded at under a minute, and PostgreSQL must
// not end the session under a live one. The request pool's is 60 s.
const minGateIdleTimeout = 2 * time.Minute

// gateHeldTooLong is when releasing a gate says that it was held long enough to be
// at risk (a variable only so a test can shorten it). The gate session is ended by
// PostgreSQL at its idle limit (5 min by default) and the advisory lock with it,
// without telling the holder; every gated phase is bounded far below that, and
// this is the sign that one was not.
var gateHeldTooLong = 2 * time.Minute

// gatePoolsAccepted holds the pools whose sessions were checked and accepted.
var gatePoolsAccepted sync.Map // *sql.DB -> struct{}

// AcquireWriteGate serializes a multi-statement capability across replicas.
// Callers must read fresh while holding it and stamp the write after that read.
// The transaction holds only the advisory lock; the work under it uses the
// caller's own (request) connections. A process crash releases the lock in
// PostgreSQL. db is the GATE pool (database.Database.GateDB).
func AcquireWriteGate(ctx context.Context, db *sql.DB, key string) (func(), error) {
	return AcquireWriteGates(ctx, db, key)
}

// AcquireWriteGates takes the locks for several keys, in the order given, in ONE
// transaction on ONE connection, and returns the function that releases them all.
//
// A caller that needs several locks names them all here: two gates taken one after
// the other are two connections, and a caller that holds one while it waits for a
// second can only make progress if the pool has room for the second. The order is
// the caller's to keep fixed: every caller that takes both of two keys must take
// them in the same order, or two of them can each hold the lock the other wants.
//
// It waits at most writeGateWait for everything together and then returns
// ErrWriteGateBusy with nothing held.
func AcquireWriteGates(ctx context.Context, db *sql.DB, keys ...string) (func(), error) {
	return AcquireWriteGatesWithin(ctx, db, writeGateWait, keys...)
}

// AcquireWriteGatesWithin is AcquireWriteGates with the wait chosen by the caller;
// zero is the default. A wait is only meaningful below pgdriver's read deadline
// (see the header): a longer one is enforced by the client's timer, which cannot
// tell the server to stop waiting. The caller's own deadline shortens it further.
func AcquireWriteGatesWithin(ctx context.Context, db *sql.DB, wait time.Duration, keys ...string) (func(), error) {
	if db == nil {
		return nil, fmt.Errorf("shared write coordination is unavailable")
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("a write gate needs a key")
	}
	if wait <= 0 {
		wait = writeGateWait
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// The wait ends at the sooner of its own limit and the caller's deadline; when
	// the caller's is the sooner the server is told to give up a little before it.
	deadline := time.Now().Add(wait)
	serverDeadline := deadline
	if callerDeadline, ok := ctx.Deadline(); ok && callerDeadline.Before(deadline) {
		deadline = callerDeadline
		serverDeadline = callerDeadline.Add(-gateCallerMargin)
	}
	if !time.Now().Before(serverDeadline) {
		return nil, context.DeadlineExceeded
	}
	waitCtx, cancelWait := context.WithDeadline(ctx, deadline)
	defer cancelWait()

	if err := checkGatePool(waitCtx, db); err != nil {
		return nil, gateWaitError(ctx, err)
	}
	conn, err := db.Conn(waitCtx)
	if err != nil {
		return nil, gateWaitError(ctx, err)
	}
	// The transaction lives as long as the caller's context, not the wait's: it must
	// outlast this function, and it is rolled back when the caller's context ends.
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	taken := time.Now()
	var once sync.Once
	release := func() {
		once.Do(func() {
			if held := time.Since(taken); held > gateHeldTooLong {
				slog.Warn("a write gate was held long enough that PostgreSQL may have ended it: a gate session is ended at its idle_in_transaction_session_timeout and the advisory lock goes with it, without a word to the holder",
					"held", held.Round(time.Second).String(), "keys", keys)
			}
			_ = tx.Rollback()
			_ = conn.Close()
		})
	}

	lockCtx, cancelLock := context.WithDeadline(ctx, serverDeadline.Add(writeGateGrace))
	defer cancelLock()
	for _, key := range keys {
		remaining := time.Until(serverDeadline)
		if remaining <= 0 {
			release()
			return nil, ErrWriteGateBusy
		}
		// The server enforces what is left of the wait (see the header).
		if _, err = tx.ExecContext(lockCtx, "SELECT set_config('lock_timeout', $1, true)", strconv.FormatInt(max(1, remaining.Milliseconds()), 10)); err != nil {
			release()
			return nil, gateWaitError(ctx, err)
		}
		if _, err = tx.ExecContext(lockCtx, "SELECT pg_advisory_xact_lock(1296387409, hashtext($1))", key); err != nil {
			release()
			return nil, gateWaitError(ctx, err)
		}
	}
	return release, nil
}

// checkGatePool refuses a pool whose sessions are ended too soon for a gate, once
// per pool. A limit of 0 is no limit. A failed check is not remembered: a
// transient error must not condemn a pool, and an accepted pool is never asked
// again.
func checkGatePool(ctx context.Context, db *sql.DB) error {
	if _, ok := gatePoolsAccepted.Load(db); ok {
		return nil
	}
	var ms int64
	if err := db.QueryRowContext(ctx, `SELECT setting::bigint FROM pg_settings WHERE name = 'idle_in_transaction_session_timeout'`).Scan(&ms); err != nil {
		return err
	}
	if limit := time.Duration(ms) * time.Millisecond; limit > 0 && limit < minGateIdleTimeout {
		slog.Error("a write gate was asked to run on a pool whose sessions are ended after "+limit.String()+" idle in a transaction; it refuses, because the advisory lock would go with the session while its holder went on writing. Gates belong on the gate pool (database.Database.GateDB), not the request pool",
			"limit", limit.String(), "minimum", minGateIdleTimeout.String())
		return fmt.Errorf("%w (this pool's limit is %s, the least a gate accepts is %s)", ErrWriteGatePoolUnsuitable, limit, minGateIdleTimeout)
	}
	gatePoolsAccepted.Store(db, struct{}{})
	return nil
}

// gateWaitError turns the ways a gate's wait can end into one answer: the caller's
// own cancellation is theirs to see, a pool that was refused stays refused, and
// everything else that is the wait running out -- the gate's deadline, the server's
// lock_timeout (55P03), a statement timeout (57014) -- is ErrWriteGateBusy.
func gateWaitError(caller context.Context, err error) error {
	if caller.Err() != nil {
		return caller.Err()
	}
	if errors.Is(err, ErrWriteGatePoolUnsuitable) {
		return err
	}
	msg := err.Error()
	if errors.Is(err, context.DeadlineExceeded) ||
		strings.Contains(msg, "55P03") || strings.Contains(msg, "lock timeout") ||
		strings.Contains(msg, "57014") || strings.Contains(msg, "statement timeout") {
		return ErrWriteGateBusy
	}
	return err
}

// VersionTimeAfter keeps a serialized write newer even if a replica's clock
// lags. PostgreSQL stores microseconds, so nanosecond differences are not enough.
func VersionTimeAfter(prior, now time.Time) time.Time {
	now = now.UTC().Truncate(time.Microsecond)
	prior = prior.UTC().Truncate(time.Microsecond)
	if now.After(prior) {
		return now
	}
	return prior.Add(time.Microsecond)
}
