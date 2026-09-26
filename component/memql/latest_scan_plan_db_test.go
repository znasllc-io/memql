package memql

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

type latestPlanCaptureKey struct{}
type latestPlanCapture struct {
	mu      sync.Mutex
	queries []string
}

func (h *latestPlanCapture) BeforeQuery(ctx context.Context, _ *bun.QueryEvent) context.Context {
	return ctx
}
func (h *latestPlanCapture) AfterQuery(ctx context.Context, event *bun.QueryEvent) {
	if ctx.Value(latestPlanCaptureKey{}) != true || event.Err != nil || !strings.HasPrefix(event.Query, "SELECT") || !strings.Contains(event.Query, "latest_keys") {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.queries = append(h.queries, event.Query)
}

// Many OTHER IDs make inherited n_distinct a poor estimate for a fleet with
// only three machines. That caused the ordinary join to hash/scan historical
// JSON payloads in production even though current-key enumeration was cheap.
// Explain the actual Bun SQL for both the DSL scan and latest-version recheck.
func TestLatestScanPlanBoundsPayloadReadsAcrossTimescaleChunks(t *testing.T) {
	ctx := context.Background()
	admin := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = admin.Close() })
	if err := admin.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, "latest payload plan", dbtest.DSN(), err)
	}
	var timescale bool
	require.NoError(t, admin.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_extension WHERE extname='timescaledb')`).Scan(&timescale))
	if !timescale {
		t.Skip("requires TimescaleDB chunk planner")
	}
	schema := fmt.Sprintf("latestplan_%d", time.Now().UnixNano())
	_, err := admin.ExecContext(ctx, `CREATE SCHEMA `+schema)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = admin.ExecContext(ctx, `DROP SCHEMA `+schema+` CASCADE`) })
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()), pgdriver.WithConnParams(map[string]any{"search_path": schema}))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	for _, stmt := range []string{
		`CREATE TABLE "MemoryNodes" (LIKE public."MemoryNodes" INCLUDING ALL)`,
		`SELECT public.create_hypertable('"MemoryNodes"','createdAt',chunk_time_interval=>interval '1 day')`,
		`INSERT INTO "MemoryNodes" (id,concept,"createdAt","createdBy",schema,payload)
         SELECT 'v1:worker:registration:machine-'||n,'v1:worker:registration',TIMESTAMPTZ '2026-01-01'+day*interval '1 day'+v*interval '1 second','fixture','{}',
           jsonb_build_object('ownerUserId','fixture','connectedNodeId','test-replica','lastSeenAt','2026-01-01T00:00:00Z','detail',repeat(md5(v::text),40))
         FROM generate_series(1,3) n CROSS JOIN generate_series(0,2) day CROSS JOIN generate_series(1,3000) v`,
		`INSERT INTO "MemoryNodes" (id,concept,"createdAt","createdBy",schema,payload)
         SELECT 'other-'||n,'v1:test:other',TIMESTAMPTZ '2026-01-03','fixture','{}','{}' FROM generate_series(1,40000) n`,
		`VACUUM ANALYZE "MemoryNodes"`,
	} {
		_, err := db.ExecContext(ctx, stmt)
		require.NoError(t, err)
	}
	_, err = LoadUnifiedConcepts(nil)
	require.NoError(t, err)
	eng, err := New(db)
	require.NoError(t, err)
	require.NoError(t, eng.Init(memorynodes.DefaultRegistry()))
	hook := &latestPlanCapture{}
	db.AddQueryHook(hook)
	readCtx := context.WithValue(clusterOwnerCtx("latest-plan-owner"), latestPlanCaptureKey{}, true)
	result, err := eng.Execute(readCtx, `query allWorkersWithStatus()`)
	require.NoError(t, err)
	require.Len(t, result.Bundle.Nodes, 3)
	result, err = eng.Execute(readCtx, `query registrationsWithStaleHold(lastSeenBefore: "2026-02-01T00:00:00Z")`)
	require.NoError(t, err)
	require.Len(t, result.Bundle.Nodes, 3)
	cutoff := time.Date(2026, 1, 2, 0, 30, 0, 0, time.UTC)
	latest, err := eng.loadLatestNodes(readCtx, []string{"v1:worker:registration:machine-1", "v1:worker:registration:machine-1", "missing"}, &cutoff)
	require.NoError(t, err)
	require.Len(t, latest, 1)
	require.True(t, latest["v1:worker:registration:machine-1"].CreatedAt.Equal(cutoff))
	hook.mu.Lock()
	queries := append([]string(nil), hook.queries...)
	hook.mu.Unlock()
	require.GreaterOrEqual(t, len(queries), 3, "capture the real scan AND latest recheck queries")
	for _, query := range queries {
		var raw []byte
		require.NoError(t, db.QueryRowContext(ctx, `EXPLAIN (ANALYZE,FORMAT JSON) `+query).Scan(&raw))
		var plans []map[string]any
		require.NoError(t, json.Unmarshal(raw, &plans))
		payloadProbes := 0
		var inspect func(map[string]any)
		inspect = func(plan map[string]any) {
			relation, _ := plan["Relation Name"].(string)
			nodeType, _ := plan["Node Type"].(string)
			loops, _ := plan["Actual Loops"].(float64)
			if strings.Contains(relation, "_chunk") && loops > 0 && nodeType != "Index Only Scan" && nodeType != "Custom Scan" {
				condition, _ := plan["Index Cond"].(string)
				filter, _ := plan["Filter"].(string)
				require.Equal(t, "Index Scan", nodeType, "historical payload scan: %s", raw)
				require.Contains(t, condition, `"createdAt"`, "payload probe must select one timestamp: %s", raw)
				require.Contains(t, condition+filter, "id", "payload probe must select one ID: %s", raw)
				require.LessOrEqual(t, plan["Actual Rows"].(float64), float64(1), "historical payloads: %s", raw)
				// A timestamp index can filter ID after the lookup. This fixture
				// has three IDs per timestamp; bound discarded payloads too.
				if discarded, ok := plan["Rows Removed by Filter"].(float64); ok {
					require.LessOrEqual(t, discarded, float64(3), "historical payloads discarded after scan: %s", raw)
				}
				payloadProbes++
			}
			children, _ := plan["Plans"].([]any)
			for _, child := range children {
				inspect(child.(map[string]any))
			}
		}
		inspect(plans[0]["Plan"].(map[string]any))
		require.Positive(t, payloadProbes, "must exercise canonical payload retrieval")
	}
}
