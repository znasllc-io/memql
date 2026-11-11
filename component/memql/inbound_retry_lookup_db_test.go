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

// THE RETRY PASS MUST NOT COST THE INBOX (store mirror, U4). Every webhook
// delivery is a v1:platform:inboundRequest row carrying its whole body, and an
// installation keeps them: an unfiltered read of the concept times out. The
// datasync retry pass asks inboundRequestsDueForRetry every minute, and the
// operator's given-up list asks inboundRequestsGivenUp. Without a lookup both
// collapse every delivery to its latest version and read its payload -- body
// and all -- to keep a handful of failed ones. With the status lookup
// (memory_nodes_inbound_failed_lookup_idx, partial on status = failed) they
// enumerate only the deliveries that ever failed.
//
// The fixture is 4,000 processed deliveries of two TOASTed versions each and
// six that failed. A bound that holds here holds because the read no longer
// depends on the inbox, not because the inbox is small.
func TestTheInboundRetryReadsLookUpOnlyFailedDeliveries(t *testing.T) {
	db, eng, hook := inboxFixture(t)
	reader := clusterOwnerCtx("v1:identity:user:inbox-owner")

	for _, tc := range []struct {
		name, query string
		want        []string
	}{
		{
			name:  "what is due",
			query: `query inboundRequestsDueForRetry(asOf: "2026-11-11T12:00:00Z")`,
			want:  []string{"v1:platform:inboundRequest:inbox-due-old", "v1:platform:inboundRequest:inbox-due"},
		},
		{
			name:  "what was given up",
			query: `query inboundRequestsGivenUp(source: "inbox")`,
			want:  []string{"v1:platform:inboundRequest:inbox-given-up"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, scans := capturedScans(t, eng, hook, reader, tc.query)
			require.NotEmpty(t, scans)
			require.Contains(t, lookupSubquery(t, scans[0]), `(payload->>'status')`,
				"the failed deliveries' lookup must start the read, not the whole inbox")
			require.Equal(t, tc.want, pageIDs(t, result))
			requireCollapsedAgrees(t, eng, hook, reader, tc.query, result)
			requireBuffersBelow(t, db, scans[0], 400, "a read of the failed deliveries must not cost the inbox")
		})
	}
}

func inboxFixture(t *testing.T) (*bun.DB, *MemQLEngine, *latestPlanCapture) {
	t.Helper()
	ctx := context.Background()
	admin := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = admin.Close() })
	if err := admin.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, "inbound retry reads", dbtest.DSN(), err)
	}
	var timescale bool
	require.NoError(t, admin.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_extension WHERE extname='timescaledb')`).Scan(&timescale))
	if !timescale {
		t.Skip("requires TimescaleDB chunk planner")
	}
	schema := fmt.Sprintf("inbox_%d", time.Now().UnixNano())
	_, err := admin.ExecContext(ctx, `CREATE SCHEMA `+schema)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = admin.ExecContext(ctx, `DROP SCHEMA `+schema+` CASCADE`) })
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()),
		pgdriver.WithConnParams(map[string]any{"search_path": schema,
			"random_page_cost": "4", "effective_cache_size": "3GB", "work_mem": "8MB"}))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	for _, stmt := range []string{
		`CREATE TABLE "MemoryNodes" (LIKE public."MemoryNodes" INCLUDING ALL)`,
		`CREATE TABLE memory_liveness (LIKE public.memory_liveness INCLUDING ALL)`,
		`SELECT public.create_hypertable('"MemoryNodes"','createdAt',chunk_time_interval=>interval '1 day')`,
		// Two versions per delivery, received then processed, each carrying a
		// body of incompressible text, so every payload read is a TOAST read.
		`INSERT INTO "MemoryNodes" (id,concept,"createdAt","createdBy",schema,payload)
         SELECT 'v1:platform:inboundRequest:inbox-'||n,'v1:platform:inboundRequest',
           TIMESTAMPTZ '2026-11-01'+n*interval '2 minutes'+v*interval '5 seconds','fixture','{}',
           jsonb_build_object('source','inbox','medium','webhook','status',CASE WHEN v=0 THEN 'received' ELSE 'processed' END,
             'attempts',v,'body',(SELECT string_agg(md5(n||'-'||g),'') FROM generate_series(1,120) g))
         FROM generate_series(1,4000) n CROSS JOIN generate_series(0,1) v`,
	} {
		_, err := db.ExecContext(ctx, stmt)
		require.NoError(t, err)
	}
	start := time.Date(2026, 11, 11, 11, 0, 0, 0, time.UTC)
	var nodes []memorynodes.MemoryNode
	add := func(id string, at time.Duration, fields map[string]any) {
		fields["source"], fields["medium"], fields["body"] = "inbox", "webhook", "{}"
		payload, err := json.Marshal(fields)
		require.NoError(t, err)
		nodes = append(nodes, memorynodes.MemoryNode{ID: "v1:platform:inboundRequest:" + id, Concept: "v1:platform:inboundRequest",
			CreatedAt: start.Add(at), CreatedBy: "fixture", Payload: payload})
	}
	failed := func(attempts int, next string) map[string]any {
		f := map[string]any{"status": "failed", "attempts": attempts}
		if next != "" {
			f["nextAttemptAt"] = next
		}
		return f
	}
	add("inbox-due-old", 0, failed(1, "2026-11-11T11:01:00Z"))
	add("inbox-due", time.Minute, failed(2, "2026-11-11T11:59:00Z"))
	add("inbox-not-yet", 2*time.Minute, failed(1, "2026-11-11T12:01:00Z"))
	add("inbox-given-up", 3*time.Minute, failed(4, ""))
	add("inbox-refused", 4*time.Minute, failed(1, ""))
	// Found by the lookup through its failed version, refused on its latest.
	add("inbox-recovered", 5*time.Minute, failed(1, "2026-11-11T11:06:00Z"))
	add("inbox-recovered", 6*time.Minute, map[string]any{"status": "processed", "attempts": 2})
	_, err = db.NewInsert().Model(&nodes).Exec(ctx)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `VACUUM ANALYZE "MemoryNodes"`)
	require.NoError(t, err)
	_, err = LoadUnifiedConcepts(nil)
	require.NoError(t, err)
	eng, err := New(db)
	require.NoError(t, err)
	require.NoError(t, bootPrivateEngine(eng, memorynodes.DefaultRegistry()))
	hook := &latestPlanCapture{}
	db.AddQueryHook(hook)
	return db, eng, hook
}
