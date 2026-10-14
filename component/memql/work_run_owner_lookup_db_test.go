package memql

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// Nexus asks for 50 summaries, but a concept-wide latest-key scan still reads
// every system run before applying its owner filter. Exercise the real query
// over multiple chunks, including ownership changes and more than two pages.
func TestWorkRunOwnerLookupBoundsSummaryPagesAndKeepsLatestOwner(t *testing.T) {
	ctx := context.Background()
	admin := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = admin.Close() })
	if err := admin.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, "work run owner lookup", dbtest.DSN(), err)
	}
	var timescale bool
	require.NoError(t, admin.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_extension WHERE extname='timescaledb')`).Scan(&timescale))
	if !timescale {
		t.Skip("requires TimescaleDB chunk planner")
	}
	schema := fmt.Sprintf("runownerlookup_%d", time.Now().UnixNano())
	_, err := admin.ExecContext(ctx, `CREATE SCHEMA `+schema)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = admin.ExecContext(ctx, `DROP SCHEMA `+schema+` CASCADE`) })
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()),
		pgdriver.WithConnParams(map[string]any{"search_path": schema}))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	for _, stmt := range []string{
		`CREATE TABLE "MemoryNodes" (LIKE public."MemoryNodes" INCLUDING ALL)`,
		`CREATE TABLE memory_liveness (LIKE public.memory_liveness INCLUDING ALL)`,
		`SELECT public.create_hypertable('"MemoryNodes"','createdAt',chunk_time_interval=>interval '1 day')`,
		`INSERT INTO "MemoryNodes" (id,concept,"createdAt","createdBy",schema,payload)
         SELECT 'v1:work:run:other-'||n,'v1:work:run',TIMESTAMPTZ '2026-01-01'+(n%3)*interval '1 day'+v*interval '1 hour','fixture','{}',
           jsonb_build_object('ownerUserId',CASE WHEN n%2=0 THEN '' ELSE 'v1:identity:user:other-owner' END,
             'automationName','background','status','succeeded')
         FROM generate_series(1,12000) n CROSS JOIN generate_series(0,1) v`,
	} {
		_, err := db.ExecContext(ctx, stmt)
		require.NoError(t, err)
	}
	const owner = "v1:identity:user:run-owner"
	const stranger = "v1:identity:user:other-owner"
	before := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	after := before.Add(48 * time.Hour)
	var nodes []memorynodes.MemoryNode
	add := func(id, user, status string, at time.Time) {
		payload, err := json.Marshal(map[string]any{
			"ownerUserId": user, "status": status, "automationName": "owner-work",
			"classification": map[string]any{"workTitle": id},
		})
		require.NoError(t, err)
		nodes = append(nodes, memorynodes.MemoryNode{ID: "v1:work:run:" + id, Concept: "v1:work:run", CreatedAt: at, CreatedBy: "fixture", Payload: payload})
	}
	var want []string
	for n := 0; n < 110; n++ {
		id := fmt.Sprintf("owned-%03d", n)
		add(id, owner, "running", before.Add(time.Duration(n)*time.Second))
		add(id, owner, "succeeded", after.Add(time.Duration(n)*time.Second))
		want = append([]string{"v1:work:run:" + id}, want...)
	}
	add("moved-away", owner, "running", before)
	add("moved-away", stranger, "succeeded", after.Add(time.Hour))
	add("moved-in", stranger, "running", before)
	add("moved-in", owner, "succeeded", after.Add(time.Hour))
	want = append([]string{"v1:work:run:moved-in"}, want...)
	_, err = db.NewInsert().Model(&nodes).Exec(ctx)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `VACUUM ANALYZE "MemoryNodes"`)
	require.NoError(t, err)
	_, err = LoadUnifiedConcepts(nil)
	require.NoError(t, err)
	eng, err := New(db)
	require.NoError(t, err)
	require.NoError(t, eng.Init(memorynodes.DefaultRegistry()))
	hook := &latestPlanCapture{}
	db.AddQueryHook(hook)
	reader := nonClusterOwnerCtx(owner)
	const query = `query workRunSummariesForOwner()`
	var all, firstScans []string
	cursor := ""
	for page := 0; page < 4; page++ {
		pageCtx := ContextWithCursor(reader, cursor)
		result, scans := capturedScans(t, eng, hook, pageCtx, query)
		require.NotEmpty(t, scans)
		require.Contains(t, scans[0], `(payload->>'ownerUserId')`, "enumerate only the caller's candidate IDs")
		collapsed, _ := collapsedScans(t, eng, hook, pageCtx, query)
		require.Equal(t, pageIDs(t, collapsed), pageIDs(t, result), "lookup and concept-wide reads must return the same ordered page")
		if page == 0 {
			firstScans = scans
		}
		ids := pageIDs(t, result)
		require.LessOrEqual(t, len(ids), 50)
		all = append(all, ids...)
		for _, row := range result.Bundle.Nodes {
			require.Equal(t, "succeeded", row.Payload.Fields["status"].GetStringValue(), "never return an older matching version")
		}
		next := result.GetMeta().Cursor
		if next == "" {
			break
		}
		require.NotEqual(t, cursor, next, "a nonterminal page must advance")
		cursor = next
	}
	require.Equal(t, want, all, "no stale ownership, skipped run, duplicate, or system run across pages")
	missing, scans := capturedScans(t, eng, hook, nonClusterOwnerCtx("v1:identity:user:missing-owner"), query)
	require.Empty(t, missing.Bundle.Nodes)
	require.NotEmpty(t, scans)
	require.Contains(t, scans[0], `(payload->>'ownerUserId')`, "an empty owner's feed must also use the lookup")

	// Analyze only this isolated fixture. Production diagnosis uses EXPLAIN
	// without ANALYZE; its recorded plan hydrated every owner's latest run.
	for n, scan := range append(firstScans, scans[0]) {
		var raw []byte
		require.NoError(t, db.QueryRowContext(ctx, `EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) `+scan).Scan(&raw))
		var plans []map[string]any
		require.NoError(t, json.Unmarshal(raw, &plans))
		plan := plans[0]["Plan"].(map[string]any)
		if n == 0 || n == len(firstScans) {
			// A small fixture may fit the buffer bound even with a sequential
			// scan. Prove the owner index serves both a hit and an empty feed.
			ownerIndex := false
			var inspect func(map[string]any)
			inspect = func(node map[string]any) {
				condition, _ := node["Index Cond"].(string)
				ownerIndex = ownerIndex || strings.Contains(condition, "ownerUserId")
				children, _ := node["Plans"].([]any)
				for _, child := range children {
					inspect(child.(map[string]any))
				}
			}
			inspect(plan)
			require.True(t, ownerIndex, "candidate enumeration must probe ownerUserId through an index: %s", raw)
		}
		hit, _ := plan["Shared Hit Blocks"].(float64)
		read, _ := plan["Shared Read Blocks"].(float64)
		t.Logf("owner summary scan touched %.0f buffers", hit+read)
		require.Less(t, hit+read, float64(5000), "a page must not hydrate 12,000 unrelated current runs: %s", raw)
	}
}
