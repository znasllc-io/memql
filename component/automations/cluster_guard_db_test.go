package automations

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
)

// openTestDB connects to a Postgres for the #561 guard integration test,
// skipping the test when none is reachable so it never blocks CI. DSN comes
// from MEMORY_NODES_DATABASE_DSN, else the dev-compose default.
func openTestDB(t *testing.T) *bun.DB {
	t.Helper()
	dsn := os.Getenv("MEMORY_NODES_DATABASE_DSN")
	if dsn == "" {
		dsn = "postgres://memql:memql_dev@localhost:5432/memql?sslmode=disable"
	}
	connector := pgdriver.NewConnector(pgdriver.WithDSN(dsn))
	db := bun.NewDB(sql.OpenDB(connector), pgdialect.New())

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		t.Skipf("no Postgres reachable for the cluster-guard integration test: %v", err)
	}

	// The guard relies on the migration's table; create it here so the test is
	// self-contained (idempotent, matches the migration DDL).
	if _, err := db.DB.ExecContext(context.Background(), `
		CREATE TABLE IF NOT EXISTS automation_execution_claims (
		  automation_name text NOT NULL,
		  dedup_key       text NOT NULL,
		  claimed_by      text NOT NULL,
		  claimed_at      timestamptz NOT NULL DEFAULT now(),
		  PRIMARY KEY (automation_name, dedup_key)
		)`); err != nil {
		_ = db.Close()
		t.Skipf("could not ensure claim table: %v", err)
	}
	return db
}

// TestClusterGuard_DBDedup is the real cross-replica test: two Claims for the
// same (automation, key) -- as two replicas would make for one event -- yield
// exactly one winner; the second is a counted, prevented duplicate.
func TestClusterGuard_DBDedup(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	// Unique key namespace per run so reruns don't collide with old rows.
	automation := fmt.Sprintf("test_guard_%d", time.Now().UnixNano())
	key := "head-shared-event"
	t.Cleanup(func() {
		_, _ = db.DB.ExecContext(context.Background(),
			`DELETE FROM automation_execution_claims WHERE automation_name = $1`, automation)
	})

	g := NewClusterExecutionGuard(func() *bun.DB { return db }, nil)
	ctx := context.Background()

	// First replica wins.
	assert.True(t, g.Claim(ctx, automation, key), "first claim wins")
	// Second replica (same event) is collapsed.
	assert.False(t, g.Claim(ctx, automation, key), "duplicate claim is prevented")
	// A different event still runs.
	assert.True(t, g.Claim(ctx, automation, "head-other-event"), "distinct event claims independently")

	assert.Equal(t, int64(2), g.Claimed(), "two distinct events claimed")
	assert.Equal(t, int64(1), g.DuplicatesPrevented(), "one cross-replica duplicate prevented")
	assert.Equal(t, int64(0), g.ClaimErrors())
}

// TestClusterGuard_DBPrune verifies the retention sweep deletes stale claim
// rows so the table stays small.
func TestClusterGuard_DBPrune(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	automation := fmt.Sprintf("test_prune_%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = db.DB.ExecContext(context.Background(),
			`DELETE FROM automation_execution_claims WHERE automation_name = $1`, automation)
	})

	// Insert a row stamped two hours ago (older than the 1h retention).
	_, err := db.DB.ExecContext(context.Background(),
		`INSERT INTO automation_execution_claims (automation_name, dedup_key, claimed_by, claimed_at)
		 VALUES ($1, $2, $3, now() - interval '2 hours')`,
		automation, "stale", "node-x")
	require.NoError(t, err)

	g := NewClusterExecutionGuard(func() *bun.DB { return db }, nil)
	g.prune(context.Background())

	var remaining int
	require.NoError(t, db.DB.QueryRowContext(context.Background(),
		`SELECT count(*) FROM automation_execution_claims WHERE automation_name = $1`,
		automation).Scan(&remaining))
	assert.Equal(t, 0, remaining, "stale claim rows must be pruned")
}
