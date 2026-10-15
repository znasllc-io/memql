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
	"sync/atomic"
	"time"

	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/core/gatecontext"
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
// MEMQL_DB_GATE_POOL_SIZE connections, 4 by default), never on the request pool
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

// ErrWriteGateBusy is what a write gate answers when its WAIT ended before it could
// be taken: another holder has the lock, or every connection of the gate pool is
// held. NOTHING WAS ATTEMPTED: the caller may try again later, and a path that sends
// mail must treat it as exactly that -- never as a final outcome of the message,
// because no send began. It should say so in words a person can read when it
// reaches one; the cause is not theirs to see.
//
// The answer is wrapped (errors.Is finds ErrWriteGateBusy, and the cause with it)
// and is given for exactly three things, and nothing else:
//
//   - SQLSTATE 55P03 (lock_not_available): the server's lock_timeout gave up on the
//     advisory lock;
//   - SQLSTATE 57014 (query_canceled) while waiting for it;
//   - the wait's context ending, by the gate's own deadline or by the caller's
//     cancellation or deadline, while the gate waits for a connection of the pool
//     or for a lock.
//
// Everything else passes through as the engine reported it: a transaction that
// could not begin on a live context, a backend that was terminated, a connection
// that was lost or timed out -- a net i/o timeout is the driver's read deadline,
// not the gate's wait -- and a pool the gate refused (ErrWriteGatePoolUnsuitable).
var ErrWriteGateBusy = errors.New("shared write coordination is busy; try again")

// IsWriteGateBusy reports whether err is a write gate that could not be taken in
// time: by type while the error still carries it, and by its words when it has
// crossed a boundary that kept only the message (a builtin called through the
// engine, a step of a workflow). A caller that sends mail uses it to tell "nothing
// was attempted, try later" from a failure of the message.
func IsWriteGateBusy(err error) bool {
	return err != nil && (errors.Is(err, ErrWriteGateBusy) || strings.Contains(err.Error(), ErrWriteGateBusy.Error()))
}

// ErrWriteGatePoolUnsuitable is what a write gate answers when the pool it was
// given ends idle transactions sooner than a gated phase can be trusted to
// finish -- the request pool, 60 s. See the header.
var ErrWriteGatePoolUnsuitable = errors.New("write gates need a pool whose sessions may sit idle in a transaction for minutes, not the request pool")

// ErrWriteGatePoolMissing is what a write gate answers when its caller was given
// no pool to take it on: whoever built the caller did not wire the gate pool
// (database.Database.GateDB), or wired a resolver that has none. It is not busy:
// nothing was attempted, and waiting will not change it -- the wiring must. Every
// caller that takes gates says, with it, which wiring it lacks
// (ResolveWriteGatePool).
var ErrWriteGatePoolMissing = errors.New("shared write coordination is unavailable: no write-gate pool was wired")

// ResolveWriteGatePool resolves the pool a caller's write gates run on, or says
// which wiring is missing: `who` names the caller, `setter` how it is given its
// pool. A caller built without a resolver and a resolver that has no pool (the
// database has not started, has stopped, or could not open its gate pool) are two
// different mistakes and are told apart.
func ResolveWriteGatePool(resolve func() *sql.DB, who, setter string) (*sql.DB, error) {
	if resolve == nil {
		return nil, fmt.Errorf("%s: %w: it was built without one; %s", who, ErrWriteGatePoolMissing, setter)
	}
	if db := resolve(); db != nil {
		return db, nil
	}
	return nil, fmt.Errorf("%s: %w: its pool resolver has none -- the database has not started, has stopped, or could not open its write-gate pool (its log says \"failed to open the write-gate pool\")", who, ErrWriteGatePoolMissing)
}

// ErrWriteGateLost is what a held gate answers when its locks are no longer
// held: PostgreSQL ended its session (an idle limit, an administrator, a failover)
// and the advisory locks went with it without a word to the holder. A holder asks
// (GateHold.Verify) before it writes, and does not write.
var ErrWriteGateLost = errors.New("a write gate's lock is no longer held; nothing was written")

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
	hold, err := AcquireGateHold(ctx, db, wait, keys...)
	if err != nil {
		return nil, err
	}
	return hold.Release, nil
}

// GateHold is an open write gate: the transaction holding its advisory locks.
type GateHold struct {
	tx    *sql.Tx
	conn  *sql.Conn
	pool  *sql.DB
	keys  []string
	taken time.Time
	once  sync.Once
	done  atomic.Bool
}

// Context explicitly lends this gate's connection to synchronous nested gates.
// They still take their own locks, in the caller's fixed order, but retain them
// until this outer hold ends. Do not use the context for parallel operations.
func (h *GateHold) Context(ctx context.Context) context.Context {
	return gatecontext.WithTransaction(ctx, h.pool, h.tx)
}

// AcquireGateHold takes the locks like AcquireWriteGatesWithin and returns the
// open gate, which can say whether its locks are still held (Verify) as well as
// give them up (Release). A holder whose writes must not happen without the lock
// verifies immediately before them.
func AcquireGateHold(ctx context.Context, db *sql.DB, wait time.Duration, keys ...string) (*GateHold, error) {
	if db == nil {
		return nil, fmt.Errorf("%w: a caller of a write gate is handed database.Database.GateDB -- on a node through "+
			"PluginContext.GateDB or its own constructor, in a test or a tool from database.NewGatePool", ErrWriteGatePoolMissing)
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("a write gate needs a key")
	}
	if wait <= 0 {
		wait = writeGateWait
	}
	if err := ctx.Err(); err != nil {
		// Not waiting yet: the caller's context was already over.
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
		// The caller's deadline is so near that no wait is left.
		return nil, fmt.Errorf("%w: %w", ErrWriteGateBusy, context.DeadlineExceeded)
	}
	waitCtx, cancelWait := context.WithDeadline(ctx, deadline)
	defer cancelWait()

	tx := gatecontext.Transaction(ctx, db)
	var conn *sql.Conn
	var err error
	if tx == nil {
		if err = checkGatePool(waitCtx, db); err != nil {
			return nil, gateWaitError(err, waitCtx)
		}
		conn, err = db.Conn(waitCtx)
		if err != nil {
			return nil, gateWaitError(err, waitCtx)
		}
		// The transaction lives as long as the caller, not the acquisition wait.
		tx, err = conn.BeginTx(ctx, nil)
		if err != nil {
			_ = conn.Close()
			return nil, gateWaitError(err, ctx)
		}
	}
	hold := &GateHold{tx: tx, conn: conn, pool: db, keys: append([]string(nil), keys...), taken: time.Now()}

	lockCtx, cancelLock := context.WithDeadline(ctx, serverDeadline.Add(writeGateGrace))
	defer cancelLock()
	for _, key := range keys {
		remaining := time.Until(serverDeadline)
		if remaining <= 0 {
			hold.Release()
			return nil, fmt.Errorf("%w: the wait for the lock of %q was over", ErrWriteGateBusy, key)
		}
		// The server enforces what is left of the wait (see the header).
		if _, err = tx.ExecContext(lockCtx, "SELECT set_config('lock_timeout', $1, true)", strconv.FormatInt(max(1, remaining.Milliseconds()), 10)); err != nil {
			hold.Release()
			return nil, gateWaitError(err, lockCtx, waitCtx)
		}
		if _, err = tx.ExecContext(lockCtx, "SELECT pg_advisory_xact_lock(1296387409, hashtext($1))", key); err != nil {
			hold.Release()
			return nil, gateWaitError(err, lockCtx, waitCtx)
		}
	}
	return hold, nil
}

// Release gives the locks up and returns the connection to the gate pool. Safe to
// call more than once, and on a gate that was never opened.
func (h *GateHold) Release() {
	if h == nil || h.tx == nil {
		return
	}
	h.once.Do(func() {
		h.done.Store(true)
		if h.conn == nil {
			return // Borrowed transaction: the outer gate owns every lock's lifetime.
		}
		if held := time.Since(h.taken); held > gateHeldTooLong {
			slog.Warn("a write gate was held long enough that PostgreSQL may have ended it: a gate session is ended at its idle_in_transaction_session_timeout and the advisory lock goes with it, without a word to the holder",
				"held", held.Round(time.Second).String(), "keys", h.keys)
		}
		_ = h.tx.Rollback()
		_ = h.conn.Close()
	})
}

// Verify reports whether this gate's session still holds every one of its locks.
// It runs on the gate's own transaction: a session PostgreSQL ended (its idle
// limit, an administrator, a failover) answers with an error or without the locks,
// and the answer is ErrWriteGateLost. A holder calls it immediately before a write
// that the lock exists to protect. A caller whose own context is done gets that
// error back, not a lost lock.
func (h *GateHold) Verify(ctx context.Context) error {
	if h == nil || h.tx == nil || h.done.Load() {
		return ErrWriteGateLost
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, key := range h.keys {
		var held bool
		err := h.tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks WHERE locktype = 'advisory' AND granted
			AND pid = pg_backend_pid() AND objsubid = 2 AND classid = 1296387409::oid AND objid = hashtext($1)::oid)`, key).Scan(&held)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("%w (%v)", ErrWriteGateLost, err)
		}
		if !held {
			return ErrWriteGateLost
		}
	}
	return nil
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

// gateWaitError says why a step of taking a gate failed. ErrWriteGateBusy, wrapped
// with its cause, is the answer for exactly the ways a WAIT ends (see its
// documentation): the server's lock_timeout (55P03), a cancelled statement (57014),
// and one of the contexts the wait ran under being over -- whichever of them,
// whether the error the driver gave for it said so or reported an i/o timeout when
// it abandoned the read. Anything else is the engine's own error, unchanged: a
// connection lost or timed out on a LIVE context is not a lock that was busy.
func gateWaitError(err error, waits ...context.Context) error {
	if errors.Is(err, ErrWriteGatePoolUnsuitable) {
		return err
	}
	var ended error
	for _, ctx := range waits {
		if ctx == nil {
			continue
		}
		if ctx.Err() != nil {
			ended = ctx.Err()
			break
		}
		// pgdriver applies this deadline to the socket too. Its timer can
		// report an i/o timeout before the context timer publishes Err().
		// The wait has still ended; a future deadline does not turn an
		// unrelated network timeout into a busy gate.
		if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
			ended = context.DeadlineExceeded
			break
		}
	}
	switch state := sqlState(err); {
	case (state == "55P03" || state == "57014") && ended != nil:
		// The server gave up and so did the caller's context: both are the cause.
		return fmt.Errorf("%w: %w: %w", ErrWriteGateBusy, err, ended)
	case state == "55P03" || state == "57014":
		return fmt.Errorf("%w: %w", ErrWriteGateBusy, err)
	case ended != nil:
		return fmt.Errorf("%w: %w", ErrWriteGateBusy, ended)
	}
	return err
}

// sqlState is the SQLSTATE of a PostgreSQL error the driver reported, or "".
func sqlState(err error) string {
	var pgerr pgdriver.Error
	if errors.As(err, &pgerr) {
		return pgerr.Field('C')
	}
	return ""
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
