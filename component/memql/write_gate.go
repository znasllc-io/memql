package memql

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// AcquireWriteGate serializes a multi-statement capability across replicas.
// Callers must read fresh while holding it and stamp the write after that read.
// The transaction holds only the advisory lock; engine writes use their normal
// connections. A process crash releases the lock in PostgreSQL.
func AcquireWriteGate(ctx context.Context, db *sql.DB, key string) (func(), error) {
	if db == nil {
		return nil, fmt.Errorf("shared write coordination is unavailable")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	wait, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if _, err = tx.ExecContext(wait, "SELECT pg_advisory_xact_lock(1296387409, hashtext($1))", key); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return func() { _ = tx.Rollback() }, nil
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
