package memql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/uptrace/bun/driver/pgdriver"
)

// ErrWriteGateBusy is what an error from AcquireWriteGate matches (errors.Is)
// when the gate could not be had because the wait for it ended without the lock:
// the pool cancelled the statement that was waiting (statement_timeout, SQLSTATE
// 57014), a lock timeout ran out (55P03), or the caller's deadline or
// cancellation came first. The error keeps its own text and its cause (errors.As
// finds the driver's error under it). Every other failure (no database, a
// transaction that cannot begin, a lost connection, a network timeout that is not
// the wait's own deadline) is returned as it was: it is not "somebody else holds
// this", and a caller that said so would send a person to retry something that
// cannot work.
var ErrWriteGateBusy = errors.New("another caller holds this write gate")

// writeGateBusyError is an error from a wait that ended, tagged so that it
// matches ErrWriteGateBusy without changing what it says.
type writeGateBusyError struct{ cause error }

func (e writeGateBusyError) Error() string        { return e.cause.Error() }
func (e writeGateBusyError) Unwrap() error        { return e.cause }
func (e writeGateBusyError) Is(target error) bool { return target == ErrWriteGateBusy }

// AcquireWriteGate serializes a multi-statement capability across replicas.
// Callers must read fresh while holding it and stamp the write after that read.
// The transaction holds only the advisory lock; engine writes use their normal
// connections. A process crash releases the lock in PostgreSQL.
//
// An error from a wait that ended without the lock matches ErrWriteGateBusy.
func AcquireWriteGate(ctx context.Context, db *sql.DB, key string) (func(), error) {
	if db == nil {
		return nil, fmt.Errorf("shared write coordination is unavailable")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		// A transaction that waited for a connection until the caller's deadline
		// is a wait that ended; one that could not begin for any other reason is not.
		if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return nil, writeGateBusyError{err}
		}
		return nil, err
	}
	wait, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if _, err = tx.ExecContext(wait, "SELECT pg_advisory_xact_lock(1296387409, hashtext($1))", key); err != nil {
		_ = tx.Rollback()
		if lockWaitEnded(wait, err) {
			return nil, writeGateBusyError{err}
		}
		return nil, err
	}
	return func() { _ = tx.Rollback() }, nil
}

// lockWaitEnded says whether the lock statement failed because its wait ended: a
// SQLSTATE that says the statement was cancelled (57014) or that a lock timeout
// ran out (55P03), or the wait's own context done. The driver reports a context
// deadline as an i/o timeout of its read, not as the context's error, and does not
// watch a cancellation at all, so the context is asked directly (and its deadline
// against the clock, since the read can time out a moment before the context's own
// timer fires). An i/o timeout with neither is not a wait that ended: it is a
// network failure, and is not called one.
func lockWaitEnded(wait context.Context, err error) bool {
	if wait.Err() != nil {
		return true
	}
	if deadline, ok := wait.Deadline(); ok && !time.Now().Before(deadline) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var pgErr pgdriver.Error
	if errors.As(err, &pgErr) {
		switch pgErr.Field('C') {
		case "57014", "55P03": // query_canceled (statement_timeout), lock_not_available (lock_timeout)
			return true
		}
	}
	return false
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
