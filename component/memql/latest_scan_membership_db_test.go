package memql

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// A revision reads a receipt from its current/previous attempts. Membership in
// those run IDs must use the existing journal lookup index, not hydrate every
// work step in the installation. The real DSL query retains owner, key, status,
// latest-version, and newest-completion semantics after narrowing candidate IDs.
func TestWorkReceiptMembershipBoundsReadsAndKeepsLatestOwner(t *testing.T) {
	ctx := context.Background()
	admin := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = admin.Close() })
	if err := admin.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, "work receipt membership", dbtest.DSN(), err)
	}
	var timescale bool
	require.NoError(t, admin.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_extension WHERE extname='timescaledb')`).Scan(&timescale))
	if !timescale {
		t.Skip("requires TimescaleDB chunk planner")
	}
	schema := fmt.Sprintf("receiptlookup_%d", time.Now().UnixNano())
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
         SELECT 'v1:work:step:other-'||n,'v1:work:step',TIMESTAMPTZ '2026-01-01'+(n%3)*interval '1 day'+v*interval '1 hour','fixture','{}',
           jsonb_build_object('ownerUserId','v1:identity:user:receipt-owner','runId','v1:work:run:other-'||n,
             'key','preparedImages','status','done','finishedAt','2026-01-01T00:00:00Z')
         FROM generate_series(1,12000) n CROSS JOIN generate_series(0,1) v`,
	} {
		_, err := db.ExecContext(ctx, stmt)
		require.NoError(t, err)
	}
	before := time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)
	after := before.Add(time.Hour)
	var nodes []memorynodes.MemoryNode
	for _, row := range []struct {
		id, run, owner, key, status string
		at                          time.Time
	}{
		{"old", "first", "receipt-owner", "preparedImages", "done", before},
		{"new", "second", "receipt-owner", "preparedImages", "done", after},
		{"moved", "first", "receipt-owner", "preparedImages", "done", before},
		{"moved", "outside", "receipt-owner", "preparedImages", "done", after},
		{"changed-owner", "first", "receipt-owner", "preparedImages", "done", before},
		{"changed-owner", "first", "someone-else", "preparedImages", "done", after},
		{"changed-status", "first", "receipt-owner", "preparedImages", "done", before},
		{"changed-status", "first", "receipt-owner", "preparedImages", "failed", after},
		{"different-key", "first", "receipt-owner", "captured", "done", after},
	} {
		payload, err := json.Marshal(map[string]any{
			"runId": "v1:work:run:" + row.run, "ownerUserId": "v1:identity:user:" + row.owner,
			"key": row.key, "status": row.status, "finishedAt": row.at.Format(time.RFC3339Nano),
		})
		require.NoError(t, err)
		nodes = append(nodes, memorynodes.MemoryNode{ID: "v1:work:step:" + row.id, Concept: "v1:work:step", CreatedAt: row.at, CreatedBy: "fixture", Payload: payload})
	}
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
	reader := clusterOwnerCtx("receipt-owner")
	for _, tc := range []struct {
		name, runs, want string
	}{
		{"one run", `["first"]`, "old"},
		{"several attempts", `["first","second"]`, "new"},
		{"no receipt", `["missing"]`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			query := `query workCompletedStepsForOwnerRuns(runIds: ` + tc.runs + `, stepKey: "preparedImages")`
			result, scans := capturedScans(t, eng, hook, reader, query)
			require.NotEmpty(t, scans)
			require.Contains(t, scans[0], `(payload->>'runId')`, "the actual DSL query must enumerate only matching run IDs")
			collapsed, _ := collapsedScans(t, eng, hook, reader, query)
			require.Equal(t, pageIDs(t, collapsed), pageIDs(t, result), "narrowing must preserve the concept-wide result")
			if tc.want == "" {
				require.Empty(t, result.Bundle.Nodes)
			} else {
				require.Len(t, result.Bundle.Nodes, 1)
				require.Equal(t, tc.want, BareShortId(result.Bundle.Nodes[0].Id))
			}
			for _, query := range scans {
				var raw []byte
				require.NoError(t, db.QueryRowContext(ctx, `EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) `+query).Scan(&raw))
				var plans []map[string]any
				require.NoError(t, json.Unmarshal(raw, &plans))
				plan := plans[0]["Plan"].(map[string]any)
				hit, _ := plan["Shared Hit Blocks"].(float64)
				read, _ := plan["Shared Read Blocks"].(float64)
				t.Logf("receipt scan touched %.0f buffers", hit+read)
				require.Less(t, hit+read, float64(500), "receipt lookup must cost its matching runs, not 12,000 unrelated steps: %s", raw)
			}
		})
	}

	// A historical membership lookup must not see a later run reassignment.
	query := `concept==v1:work:step && payload.runId in ["v1:work:run:first","v1:work:run:second"] && payload.key=="preparedImages" && payload.ownerUserId=="v1:identity:user:receipt-owner" && payload.status=="done"`
	plan, err := eng.parseWithFunctions(query, nil, nil, false)
	require.NoError(t, err)
	filter, ok := eng.tryCompileCombinedFilter(reader, plan.Root, "")
	require.True(t, ok)
	cutoff := before.Add(time.Minute)
	historical, err := eng.executeCombinedFilterQuery(reader, plan.Root, filter, &cutoff, 100, nil)
	require.NoError(t, err)
	ids := make([]string, 0, len(historical))
	for _, row := range historical {
		ids = append(ids, BareShortId(row.ID))
	}
	require.ElementsMatch(t, []string{"old", "moved", "changed-owner", "changed-status"}, ids)
}
