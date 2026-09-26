package shopify

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Pending app credentials are a pair. Serialize Save and conditional clearing
// across replicas so a callback cannot erase a newer Save between its comparison
// and writes. A transaction-scoped lock is released even if the process dies.
func (c *Connector) acquireConnectApp(ctx context.Context, storeID string) (func(), error) {
	if c.connectAppGate != nil {
		return c.connectAppGate(ctx, storeID)
	}
	if c.db == nil || c.db() == nil {
		return nil, fmt.Errorf("shopify: connection storage is unavailable")
	}
	return acquireConnectAppDB(ctx, c.db(), storeID)
}

func acquireConnectAppDB(ctx context.Context, db *sql.DB, storeID string) (func(), error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	// Bound acquisition only: the transaction must hold the lock for the entire
	// caller operation, even when its writes take longer than acquisition.
	waitCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if _, err = tx.ExecContext(waitCtx, "SELECT pg_advisory_xact_lock(1397247824, hashtext($1))", storeID); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return func() { _ = tx.Rollback() }, nil
}
