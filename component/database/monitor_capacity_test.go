package database

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

func TestMonitorReportsSaturatedPoolAndRecoversWithoutReplacingIt(t *testing.T) {
	d := newRetryTestDB(t, &countingMigrator{})
	d.migrationsRan = true
	d.DB = sql.OpenDB(pingableConnector{})
	d.DB.SetMaxOpenConns(1)
	defer d.DB.Close()
	pool := d.DB
	held, err := pool.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	done := make(chan bool, 1)
	go func() { done <- d.tryPing(context.Background()) }()
	select {
	case healthy := <-done:
		if healthy {
			t.Fatal("a pool with no available request connection reported healthy")
		}
	case <-time.After(7 * time.Second):
		t.Fatal("health monitor blocked behind a retained connection")
	}
	if d.DB != pool {
		t.Fatal("capacity pressure replaced the pool and expanded the connection budget")
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	if !d.tryPing(context.Background()) {
		t.Fatal("monitor did not recover after the request connection was released")
	}
}
