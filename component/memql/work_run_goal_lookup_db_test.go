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

// THE OWNER WHO OWNS EVERY RUN (2026-10-29). Production's cluster owner owns
// every pipeline run, and every pipeline run is a v1:work:run that writes a
// new version at each step boundary, so the owner lookup range IS the run
// history. workRunsForGoal ANDed its unindexed goalId term into that range and
// read every historical payload before returning two rows, until the request
// pool cancelled it at eight seconds (SQLSTATE 57014).
//
// The fixture is that shape at small scale: one owner owns 1,200 runs of eight
// TOASTed versions each, and the rows a read must return are a handful of
// them. A bound that holds here holds because the read no longer depends on
// the history, not because the history is small: before the fix the goal read
// below touched every version (tens of thousands of buffers).
func TestWorkRunReadsDoNotScanTheOwnersHistory(t *testing.T) {
	db, eng, hook := runHistoryFixture(t)
	reader := clusterOwnerCtx(runHistoryOwner)

	t.Run("goal read enumerates the goal, not the owner", func(t *testing.T) {
		const query = `query workRunsForGoal(goalId: "v1:work:goal:target")`
		result, scans := capturedScans(t, eng, hook, reader, query)
		require.NotEmpty(t, scans)
		lookup := lookupSubquery(t, scans[0])
		require.Contains(t, lookup, `(payload->>'goalId')`, "the goal's own lookup index must start the read")
		require.NotContains(t, lookup, `(payload->>'ownerUserId')`,
			"the owner term must not ride beside the goal term: PostgreSQL ignores the statistics of partial "+
				"expression indexes, estimates both at the default selectivity and can pick the owner index, "+
				"which is the whole history")
		require.Equal(t, []string{"v1:work:run:target-new", "v1:work:run:target-old"}, pageIDs(t, result),
			"the goal's runs, newest first; a run that left the goal and a stranger's run are not returned")
		collapsed, _ := collapsedScans(t, eng, hook, reader, query)
		require.Equal(t, pageIDs(t, collapsed), pageIDs(t, result), "the lookup must return what the concept-wide read returns")
		buffers := explainBuffers(t, db, scans[0])
		t.Logf("workRunsForGoal touched %.0f buffers", buffers)
		require.Less(t, buffers, float64(2000), "one goal's runs must not cost the owner's history")
	})

	t.Run("an unindexed term is decided on the latest version, never per version", func(t *testing.T) {
		const query = `query workRunsForOwnerGoalSignature(goalSignature: "sig-target")`
		result, scans := capturedScans(t, eng, hook, reader, query)
		require.NotEmpty(t, scans)
		lookup := lookupSubquery(t, scans[0])
		require.Contains(t, lookup, `(payload->>'ownerUserId')`, "the owner index is the only one serving this read")
		require.NotContains(t, lookup, `(payload->>'goalSignature')`, "an unindexed term read in the lookup reads every version's payload")
		require.NotContains(t, lookup, `(payload->>'status')`, "an unindexed term read in the lookup reads every version's payload")
		require.Equal(t, []string{"v1:work:run:sig-new", "v1:work:run:sig-old"}, pageIDs(t, result))
		collapsed, _ := collapsedScans(t, eng, hook, reader, query)
		require.Equal(t, pageIDs(t, collapsed), pageIDs(t, result), "the lookup must return what the concept-wide read returns")

		var raw []byte
		require.NoError(t, db.QueryRowContext(context.Background(), `EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) `+scans[0]).Scan(&raw))
		var plans []map[string]any
		require.NoError(t, json.Unmarshal(raw, &plans))
		ownerScans := 0
		var inspect func(map[string]any)
		inspect = func(node map[string]any) {
			if condition, _ := node["Index Cond"].(string); strings.Contains(condition, "ownerUserId") {
				ownerScans++
				filter, _ := node["Filter"].(string)
				require.NotContains(t, filter, "payload", "the owner range must be enumerated without reading a payload: %s", raw)
			}
			children, _ := node["Plans"].([]any)
			for _, child := range children {
				inspect(child.(map[string]any))
			}
		}
		inspect(plans[0]["Plan"].(map[string]any))
		require.Positive(t, ownerScans, "the read must enumerate through the owner index: %s", raw)
	})
}

const runHistoryOwner = "v1:identity:user:history-owner"

// runHistoryFixture builds the production shape in an isolated schema: 1,200
// runs of one owner, eight versions each (seven running, then succeeded), each
// version carrying an incompressible 3 KB spine so its payload lives out of
// line in TOAST as a real run row's does. Every run has its own goal; 40
// signatures recur across them. The rows the reads must return or refuse are
// written after the history, version by version.
func runHistoryFixture(t *testing.T) (*bun.DB, *MemQLEngine, *latestPlanCapture) {
	t.Helper()
	ctx := context.Background()
	admin := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = admin.Close() })
	if err := admin.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, "work run history reads", dbtest.DSN(), err)
	}
	var timescale bool
	require.NoError(t, admin.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_extension WHERE extname='timescaledb')`).Scan(&timescale))
	if !timescale {
		t.Skip("requires TimescaleDB chunk planner")
	}
	schema := fmt.Sprintf("runhistory_%d", time.Now().UnixNano())
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
         SELECT 'v1:work:run:history-'||n,'v1:work:run',TIMESTAMPTZ '2026-01-01'+n*interval '3 minutes'+v*interval '10 seconds','fixture','{}',
           jsonb_build_object('ownerUserId','` + runHistoryOwner + `','goalId','v1:work:goal:history-'||n,
             'goalSignature','sig-history-'||(n%40),'automationName','pipeline','mode','live',
             'status',CASE WHEN v<7 THEN 'running' ELSE 'succeeded' END,
             'spine',jsonb_build_object('closure',(SELECT string_agg(md5(n||'-'||v||'-'||g),'') FROM generate_series(1,96) g)))
         FROM generate_series(1,1200) n CROSS JOIN generate_series(0,7) v`,
	} {
		_, err := db.ExecContext(ctx, stmt)
		require.NoError(t, err)
	}
	const stranger = "v1:identity:user:history-stranger"
	start := time.Date(2026, 1, 3, 12, 0, 0, 0, time.UTC)
	var nodes []memorynodes.MemoryNode
	add := func(id string, at time.Duration, fields map[string]any) {
		fields["automationName"], fields["mode"] = "pipeline", "live"
		payload, err := json.Marshal(fields)
		require.NoError(t, err)
		nodes = append(nodes, memorynodes.MemoryNode{ID: "v1:work:run:" + id, Concept: "v1:work:run",
			CreatedAt: start.Add(at), CreatedBy: "fixture", Payload: payload})
	}
	run := func(owner, goal, signature, status string) map[string]any {
		return map[string]any{"ownerUserId": owner, "goalId": goal, "goalSignature": signature, "status": status}
	}
	target := "v1:work:goal:target"
	add("target-old", 0, run(runHistoryOwner, target, "sig-history-1", "running"))
	add("target-old", time.Minute, run(runHistoryOwner, target, "sig-history-1", "succeeded"))
	add("target-new", 2*time.Minute, run(runHistoryOwner, target, "sig-history-1", "running"))
	add("target-new", 3*time.Minute, run(runHistoryOwner, target, "sig-history-1", "running"))
	// Found by the lookup through an old version, refused on its latest one.
	add("target-moved", 0, run(runHistoryOwner, target, "sig-history-1", "running"))
	add("target-moved", 4*time.Minute, run(runHistoryOwner, "v1:work:goal:elsewhere", "sig-history-1", "succeeded"))
	add("target-stranger", time.Minute, run(stranger, target, "sig-history-1", "running"))
	add("sig-old", 0, run(runHistoryOwner, "v1:work:goal:sig-old", "sig-target", "running"))
	add("sig-old", time.Minute, run(runHistoryOwner, "v1:work:goal:sig-old", "sig-target", "succeeded"))
	add("sig-new", 2*time.Minute, run(runHistoryOwner, "v1:work:goal:sig-new", "sig-target", "succeeded"))
	add("sig-running", 3*time.Minute, run(runHistoryOwner, "v1:work:goal:sig-running", "sig-target", "running"))
	add("sig-stranger", 3*time.Minute, run(stranger, "v1:work:goal:sig-stranger", "sig-target", "succeeded"))
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
	return db, eng, hook
}

// lookupSubquery is the id enumeration of a lookup read: the DISTINCT scan
// whose WHERE clause decides how many versions the read visits.
func lookupSubquery(t *testing.T, scan string) string {
	t.Helper()
	start := strings.Index(scan, `SELECT DISTINCT "mn"."id"`)
	end := strings.Index(scan, `) AS lookup`)
	require.True(t, start >= 0 && end > start, "the read must enumerate through a lookup: %s", scan)
	return scan[start:end]
}

// explainBuffers runs the captured statement under EXPLAIN ANALYZE and returns
// the buffers it touched, hit or read.
func explainBuffers(t *testing.T, db *bun.DB, scan string) float64 {
	t.Helper()
	var raw []byte
	require.NoError(t, db.QueryRowContext(context.Background(), `EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) `+scan).Scan(&raw))
	var plans []map[string]any
	require.NoError(t, json.Unmarshal(raw, &plans))
	plan := plans[0]["Plan"].(map[string]any)
	hit, _ := plan["Shared Hit Blocks"].(float64)
	read, _ := plan["Shared Read Blocks"].(float64)
	return hit + read
}
