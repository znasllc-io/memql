package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/common"
)

// closeProbeConnector is a no-network driver.Connector: sql.OpenDB never dials
// until a query runs, and DB.Close() short-circuits before any Connect, so the
// pool can be opened + closed without a real database. Connect is wired to
// error in case anything tries to use it.
type closeProbeConnector struct{}

func (closeProbeConnector) Connect(context.Context) (driver.Conn, error) {
	return nil, errors.New("closeProbeConnector: no real connection in test")
}

func (closeProbeConnector) Driver() driver.Driver { return nil }

func newTestDatabase(t *testing.T) *Database {
	t.Helper()

	d, err := NewDatabase(
		common.ComponentName("test-db"),
		WithDSN("postgres://user:pass@localhost:5432/db?sslmode=disable"),
	)
	if err != nil {
		t.Fatalf("NewDatabase: %v", err)
	}

	return d
}

// TestPoolStatsReportsLivePoolPresence: PoolStats reports ok=false with no pool
// handle, ok=true once one is attached.
func TestPoolStatsReportsLivePoolPresence(t *testing.T) {
	d := newTestDatabase(t)

	if _, ok := d.PoolStats(); ok {
		t.Fatalf("PoolStats should report no live pool before a handle is attached")
	}

	pool := sql.OpenDB(closeProbeConnector{})
	d.Lock()
	d.DB = pool
	d.isRunning = true
	d.Unlock()

	if _, ok := d.PoolStats(); !ok {
		t.Fatalf("PoolStats should report a live pool once a handle is attached")
	}
}

// TestClosePoolReleasesConnections: the explicit drain-phase pool close detaches
// the handle, closes the underlying pool (releasing connections), and is
// idempotent on a second call (the later dependency Stop sweep). This is the
// memql#1875 pool-close-on-drain behavior.
func TestClosePoolReleasesConnections(t *testing.T) {
	d := newTestDatabase(t)

	pool := sql.OpenDB(closeProbeConnector{})
	d.Lock()
	d.DB = pool
	d.isRunning = true
	d.Unlock()

	if _, ok := d.PoolStats(); !ok {
		t.Fatalf("precondition: expected a live pool before ClosePool")
	}

	d.ClosePool(context.Background())

	// Handle detached from the component.
	if _, ok := d.PoolStats(); ok {
		t.Errorf("expected no live pool after ClosePool; handle should be detached")
	}

	// Underlying pool actually closed: a closed *sql.DB short-circuits Ping
	// with a "database is closed" error before touching the connector.
	if err := pool.PingContext(context.Background()); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Errorf("expected closed-pool error pinging the released pool, got %v", err)
	}

	// Idempotent: a second close (e.g. the dependency Stop sweep) must not panic.
	d.ClosePool(context.Background())
}
