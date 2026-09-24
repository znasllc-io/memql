package shopify

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// Two connectors can handle Save and callback on different replicas. The gate
// must live in Postgres, and release on cancellation as well as normal return.
func TestConnectAppGateAcrossConnections(t *testing.T) {
	_, db := authorityEngine(t)
	if db == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	id := fmt.Sprintf("connect-lock-%d", time.Now().UnixNano())
	release, err := acquireConnectAppDB(ctx, db, id)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	other, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	var acquired bool
	err = other.QueryRowContext(context.Background(), "SELECT pg_try_advisory_lock(1397247824, hashtext($1))", id).Scan(&acquired)
	if err != nil {
		t.Fatal(err)
	}
	if acquired {
		_, _ = other.ExecContext(context.Background(), "SELECT pg_advisory_unlock(1397247824, hashtext($1))", id)
		t.Fatal("another replica acquired a held credential lock")
	}
	cancel()
	// The blocking acquisition exercises release on caller cancellation.
	waitCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	next, err := acquireConnectAppDB(waitCtx, db, id)
	if err != nil {
		t.Fatalf("cancelled caller retained its lock: %v", err)
	}
	next()
}
