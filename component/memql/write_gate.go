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

// ErrWriteGateBusy is what a write gate answers when it could not be taken in time:
// another holder has the lock, or the pod has no gate slot or connection to spare.
// Nothing was written; the caller may try again, and should say so in words a
// person can read -- the cause is not theirs to see.
var ErrWriteGateBusy = errors.New("shared write coordination is busy; try again")

// writeGateWait is how long a write gate waits, in total, for everything it needs:
// a place among the pool's gate holders, a connection, and its locks.
//
// A gate is a transaction on the request pool, and the request pool's backends run
// with three limits that decide what a wait can be (component/database/database.go,
// sessionConnParams):
//
//   - statement_timeout, 8 s by default: the SERVER cancels a lock wait at 8 s, with
//     "canceling statement due to statement timeout" -- not at the 15 s this file used
//     to name, and in words nobody should read;
//   - pgdriver's read deadline, 10 s, which is the CLIENT's: a pool without a
//     statement_timeout (a test's, an operator's who raised it) has a lock wait fail at
//     10 s with "i/o timeout" while its backend keeps waiting, and when the holder lets
//     go the orphan takes the lock in a transaction nobody will finish (measured in
//     component/database/latest_row_index.go);
//   - idle_in_transaction_session_timeout, 60 s: a gate is idle in its transaction
//     while the holder works, and at 60 s PostgreSQL ends the session and the lock
//     goes with it, silently. See gateHeldTooLong.
//
// So the wait is enforced by a `lock_timeout` on the gate's own transaction, and it
// is set below both of the first two: 6 s. The server then cancels the statement
// itself, at a time that does not depend on how the pool was configured, with an
// error that is recognised (ErrWriteGateBusy) and no waiter left behind.
const writeGateWait = 6 * time.Second

// writeGateGrace is how much longer than the wait the client keeps a lock statement's
// context, so the server's lock_timeout error is read before the client gives up.
const writeGateGrace = time.Second

// gateHeldTooLong is when releasing a gate says that it was held long enough to be
// at risk (a variable only so a test can shorten it). PostgreSQL ends an idle transaction at idle_in_transaction_session_timeout
// (60 s by default) and the advisory lock with it, without telling the holder, so a
// gated section is meant to last seconds. This is the sign that one did not.
var gateHeldTooLong = 30 * time.Second

// gateSlots holds, per pool, how many write gates may be open at once. See slotsFor.
var gateSlots sync.Map // *sql.DB -> chan struct{}

// slotsFor returns the channel that bounds the gates open on db, or nil when its
// pool has no connection limit.
//
// A gate is a transaction, so it holds a connection of the pool until it is
// released, and whatever the holder does meanwhile -- the reads and writes the gate
// exists to serialise -- needs a connection of the same pool. Gates that held every
// connection would leave their own holders nothing to work with, and the pod would
// wait out the holders' deadlines with nothing else reaching the database: four
// signups on a four-connection pool did exactly that. So at most one connection
// fewer than the pool's size can be held by gates, and the last is always free for
// the work done under them. (A pool of one has no such connection to spare, and a
// gate holder on it cannot work; nothing can be done about that here.)
func slotsFor(db *sql.DB) chan struct{} {
	if v, ok := gateSlots.Load(db); ok {
		return v.(chan struct{})
	}
	limit := db.Stats().MaxOpenConnections
	if limit <= 0 {
		return nil
	}
	v, _ := gateSlots.LoadOrStore(db, make(chan struct{}, max(1, limit-1)))
	return v.(chan struct{})
}

// AcquireWriteGate serializes a multi-statement capability across replicas.
// Callers must read fresh while holding it and stamp the write after that read.
// The transaction holds only the advisory lock; engine writes use their normal
// connections. A process crash releases the lock in PostgreSQL.
func AcquireWriteGate(ctx context.Context, db *sql.DB, key string) (func(), error) {
	return AcquireWriteGates(ctx, db, key)
}

// AcquireWriteGates takes the locks for several keys, in the order given, in ONE
// transaction on ONE connection, and returns the function that releases them all.
//
// Two gates taken one after the other are two transactions and two connections, and
// two callers that each hold one and wait for another connection wait for each
// other: on a four-connection pool, two signups on two storefronts did exactly that.
// A caller that needs several locks names them all here. The order is the caller's
// to keep fixed: every caller that takes both of two keys must take them in the same
// order, or two of them can each hold the lock the other wants.
//
// It waits at most writeGateWait for everything together and then returns
// ErrWriteGateBusy with nothing held. The number of gates open at once per pool is
// bounded (see slotsFor).
func AcquireWriteGates(ctx context.Context, db *sql.DB, keys ...string) (func(), error) {
	return AcquireWriteGatesWithin(ctx, db, writeGateWait, keys...)
}

// AcquireWriteGatesWithin is AcquireWriteGates with the wait chosen by the caller;
// zero is the default. A wait is only meaningful below the request pool's
// statement_timeout and pgdriver's read deadline (see writeGateWait): a longer one
// is enforced by whichever of those fires first, not by the gate.
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
	deadline := time.Now().Add(wait)
	waitCtx, cancelWait := context.WithDeadline(ctx, deadline)
	defer cancelWait()

	slot, err := takeGateSlot(waitCtx, db)
	if err != nil {
		return nil, gateWaitError(ctx, err)
	}
	conn, err := db.Conn(waitCtx)
	if err != nil {
		slot()
		return nil, gateWaitError(ctx, err)
	}
	// The transaction lives as long as the caller's context, not the wait's: it must
	// outlast this function.
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		_ = conn.Close()
		slot()
		return nil, err
	}
	taken := time.Now()
	var once sync.Once
	release := func() {
		once.Do(func() {
			if held := time.Since(taken); held > gateHeldTooLong {
				slog.Warn("a write gate was held long enough that PostgreSQL may have ended it: an idle transaction is ended at idle_in_transaction_session_timeout (60 s by default) and the advisory lock goes with it, without a word to the holder",
					"held", held.Round(time.Second).String(), "keys", keys)
			}
			_ = tx.Rollback()
			_ = conn.Close()
			slot()
		})
	}

	lockCtx, cancelLock := context.WithDeadline(ctx, deadline.Add(writeGateGrace))
	defer cancelLock()
	for _, key := range keys {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			release()
			return nil, ErrWriteGateBusy
		}
		// The server enforces what is left of the wait (see writeGateWait).
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

// takeGateSlot waits for a place among the pool's gate holders and returns the
// function that gives it back.
func takeGateSlot(ctx context.Context, db *sql.DB) (func(), error) {
	slots := slotsFor(db)
	if slots == nil {
		return func() {}, nil
	}
	select {
	case slots <- struct{}{}:
		return func() { <-slots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// gateWaitError turns the ways a gate's wait can end into one answer: the caller's
// own cancellation is theirs to see, and everything else that is the wait running
// out -- the gate's deadline, the server's lock_timeout (55P03), the pool's
// statement_timeout (57014) -- is ErrWriteGateBusy.
func gateWaitError(caller context.Context, err error) error {
	if caller.Err() != nil {
		return caller.Err()
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
