package automations

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/database/dbtest"
)

// openTestDB connects to a Postgres for the #561 guard integration test,
// skipping the test when none is reachable so it never blocks CI. DSN comes
// from MEMQL_DATABASE_DSN, else the dev-compose default.
func openTestDB(t *testing.T) *bun.DB {
	t.Helper()
	dsn := dbtest.DSN()
	connector := pgdriver.NewConnector(pgdriver.WithDSN(dsn))
	db := bun.NewDB(sql.OpenDB(connector), pgdialect.New())

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		dbtest.Unreachable(t, "DB-gated test for the cluster-guard integration test", dsn, err)
	}

	// The claim table comes from the MIGRATIONS, applied by this package's
	// TestMain via dbtest.EnsureSchema (memql#3030).
	//
	// This used to hand-roll `CREATE TABLE IF NOT EXISTS
	// automation_execution_claims` here "so the test is self-contained". That
	// was reasonable while the package had no TestMain and ran outside the
	// db-tests lane -- but it is a SECOND definition of a shipped table
	// (20260601120000_automation_execution_claims_ensure.up.sql, memql#624),
	// and a second definition can drift from the first silently: add a column
	// to the migration and this test keeps passing against its own stale
	// shape, which is worse than not testing the table at all.
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

// TestClusterGuard_DBClaimWithTTLRewinsStaleClaim is the memql#2548 guard
// half: ClaimWithTTL(ttl>0) honours a claim within its lease but lets a peer
// re-win one whose claimed_at is older than the ttl. This is what lets the
// outbound worker recover a row orphaned by a replica that died between
// claiming an attempt and stamping its terminal status.
func TestClusterGuard_DBClaimWithTTLRewinsStaleClaim(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	automation := fmt.Sprintf("test_ttl_%d", time.Now().UnixNano())
	key := "row-r1:0"
	t.Cleanup(func() {
		_, _ = db.DB.ExecContext(context.Background(),
			`DELETE FROM automation_execution_claims WHERE automation_name = $1`, automation)
	})

	g := NewClusterExecutionGuard(func() *bun.DB { return db }, nil)
	ctx := context.Background()
	ttl := 5 * time.Minute

	// The original claimant wins.
	assert.True(t, g.ClaimWithTTL(ctx, automation, key, ttl), "first claim wins")
	// A peer re-claiming within the lease loses (the claim is still fresh).
	assert.False(t, g.ClaimWithTTL(ctx, automation, key, ttl), "a claim within its lease is honoured")

	// Age the claim past the lease (the claimant died, never stamped).
	_, err := db.DB.ExecContext(ctx,
		`UPDATE automation_execution_claims SET claimed_at = now() - interval '10 minutes'
		 WHERE automation_name = $1 AND dedup_key = $2`, automation, key)
	require.NoError(t, err)

	// A peer now re-wins the orphaned claim -- the row can be recovered.
	assert.True(t, g.ClaimWithTTL(ctx, automation, key, ttl), "a claim past its lease is re-winnable")

	// The claimed_by must now be this node (the takeover actually ran).
	var claimedBy string
	require.NoError(t, db.DB.QueryRowContext(ctx,
		`SELECT claimed_by FROM automation_execution_claims WHERE automation_name = $1 AND dedup_key = $2`,
		automation, key).Scan(&claimedBy))
	assert.Equal(t, g.nodeId, claimedBy, "the stale claim must be taken over by the re-winning node")
}

// TestClusterGuard_DBClaimNoTTLNeverRewins is the automation-execution
// regression for memql#2548: the default Claim path (ttl == 0) must NEVER let
// a claim be re-taken, no matter how old -- event-triggered automations and
// planner plan-execution depend on exactly-once within the retention window.
// Proves the shared guard's existing behavior is unchanged by the opt-in
// lease.
func TestClusterGuard_DBClaimNoTTLNeverRewins(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	automation := fmt.Sprintf("test_nottl_%d", time.Now().UnixNano())
	key := "head-shared-event"
	t.Cleanup(func() {
		_, _ = db.DB.ExecContext(context.Background(),
			`DELETE FROM automation_execution_claims WHERE automation_name = $1`, automation)
	})

	g := NewClusterExecutionGuard(func() *bun.DB { return db }, nil)
	ctx := context.Background()

	assert.True(t, g.Claim(ctx, automation, key), "first claim wins")
	// Age the claim far past any conceivable lease; the no-TTL path still
	// refuses to re-take it (only the retention prune ever removes it).
	_, err := db.DB.ExecContext(ctx,
		`UPDATE automation_execution_claims SET claimed_at = now() - interval '10 hours'
		 WHERE automation_name = $1 AND dedup_key = $2`, automation, key)
	require.NoError(t, err)
	assert.False(t, g.Claim(ctx, automation, key), "the no-TTL claim is never re-winnable (exactly-once preserved)")

	var claimedBy string
	require.NoError(t, db.DB.QueryRowContext(ctx,
		`SELECT claimed_by FROM automation_execution_claims WHERE automation_name = $1 AND dedup_key = $2`,
		automation, key).Scan(&claimedBy))
	assert.Equal(t, g.nodeId, claimedBy, "the original claim row is untouched by the losing re-claim")
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
