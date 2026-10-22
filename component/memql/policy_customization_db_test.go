package memql

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
)

// Explicit isolated test database only: never run against the user's cluster.
func TestPolicyCustomizationPostgres(t *testing.T) {
	dsn := os.Getenv("MEMQL_POLICY_TEST_DSN")
	if dsn == "" {
		t.Skip("set MEMQL_POLICY_TEST_DSN to isolated fleet_policy_test database")
	}
	require.Contains(t, dsn, "/fleet_policy_test")
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	defer db.Close()
	ctx := context.Background()
	migration, err := os.ReadFile("../database/memory-nodes/migrations/20260915183000_router_policy_customization.up.sql")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, string(migration))
	require.NoError(t, err)
	migration, err = os.ReadFile("../database/memory-nodes/migrations/20261022160000_router_route_configuration.up.sql")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, string(migration))
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `DELETE FROM router_policy_revision WHERE revision > 0`)
	require.NoError(t, err)
	store := databasePolicyStore{database: func() *bun.DB { return db }}
	a, b := policyFixture(store), policyFixture(store)
	require.NoError(t, a.Save(policyOwner(), 0, PolicyConfig{Name: "localFirst", Primary: "app:codex", Fallbacks: []string{"fleet:fastest"}}))
	require.Error(t, b.Save(policyOwner(), 0, PolicyConfig{Name: "localOnly", Primary: "app:*"}))
	snapshot, err := b.Snapshot(ctx)
	require.NoError(t, err)
	p, _ := snapshot.Lookup("localFirst")
	require.Equal(t, "app:codex", p.Primary)
	restarted := policyFixture(databasePolicyStore{database: func() *bun.DB { return db }})
	require.NoError(t, restarted.Reset(policyOwner(), 1, "localFirst"))
	rows, err := a.Catalog(ctx)
	require.NoError(t, err)
	for _, row := range rows {
		require.False(t, row.Customized)
	}
	var count int
	err = db.NewRaw(`SELECT count(*) FROM router_policy_revision`).Scan(ctx, &count)
	require.NoError(t, err)
	require.Equal(t, 3, count)
	var author string
	err = db.NewRaw(`SELECT updated_by FROM router_policy_revision WHERE revision=1`).Scan(ctx, &author)
	require.NoError(t, err)
	require.Equal(t, "owner", strings.TrimSpace(author))

	// Exercise the actual database store on another registry. An in-memory
	// store masks dropped fields: the old writer advanced the revision while
	// silently losing both the Apps source selection and Auto's balance.
	jobs := RouteJobs{Fast: []string{"app:codex"}, Strong: []string{"app:codex"}, Reasoning: []string{"app:codex"}}
	require.NoError(t, a.SaveRouteOverride(policyOwner(), 2, RouteIdApps, jobs))
	other, err := b.RouteConfiguration(ctx)
	require.NoError(t, err)
	require.Equal(t, jobs, other.Routes[RouteIdApps])
	balance := AutoBalance{Speed: 90, Cost: 10, Quality: 40}
	require.NoError(t, b.SaveAutoBalance(policyOwner(), 3, balance))
	restarted = policyFixture(databasePolicyStore{database: func() *bun.DB { return db }})
	other, err = restarted.RouteConfiguration(ctx)
	require.NoError(t, err)
	require.Equal(t, jobs, other.Routes[RouteIdApps])
	require.Equal(t, &balance, other.Auto)
	require.NoError(t, restarted.ResetRouteOverride(policyOwner(), 4, RouteIdApps))
	other, err = a.RouteConfiguration(ctx)
	require.NoError(t, err)
	require.Empty(t, other.Routes)
	require.Equal(t, &balance, other.Auto)
}
