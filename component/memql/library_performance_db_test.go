package memql

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// Measure real compiled queries, not just the presence of an index. The
// fixture spans fourteen time chunks with production's random_page_cost=4.
// Relative buffer-work assertions survive slower machines and shared CI.
func TestLibraryPagesAndRunFilesDoNotReadTheWholeLibrary(t *testing.T) {
	eng, db := libraryPerformanceEngine(t)
	ctx := context.Background()
	owner := "library-perf-" + uniqueSuffix("directory")
	marker := "library-perf:" + owner
	_, err := db.ExecContext(ctx, `INSERT INTO "MemoryNodes" (id,concept,type,"createdBy","createdAt",payload,schema)
SELECT 'v1:library:artifact:'||?||'-'||n,'v1:library:artifact','object',?,
 TIMESTAMPTZ '2025-01-01'+(n/1000)*interval '8 days'+(n%1000)*interval '1 second',
 jsonb_build_object('ownerUserId','v1:identity:user:'||?, 'lens','artifact','kind','file','title','Document '||n||'.pdf',
 'folderId','v1:library:folder:'||CASE WHEN n%1000=0 THEN 'small' ELSE 'large' END,
 'producedByRunId','v1:work:run:'||CASE WHEN n%1000=0 THEN 'selected' ELSE 'other' END,
 'summary',repeat(md5(n::text),20)), '{}'::jsonb
FROM generate_series(0,13999) n`, owner, marker, owner)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = db.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE "createdBy"=?`, marker) })
	_, err = db.ExecContext(ctx, `ANALYZE "MemoryNodes"`)
	require.NoError(t, err)
	hook := &latestPlanCapture{}
	db.AddQueryHook(hook)
	reader := clusterOwnerCtx(owner)
	page, scans := capturedScans(t, eng, hook, reader, `query libraryArtifactsBrowse()`)
	require.Len(t, page.Bundle.Nodes, 10)
	require.NotEmpty(t, page.GetMeta().Cursor)
	require.NotEmpty(t, scans)
	require.Contains(t, scans[0], "NOT EXISTS", "the real named query must take the bounded scan")
	require.NotContains(t, scans[0], "latest_keys", "a page must not enumerate every artifact")
	first := pageIDs(t, page)
	next, err := eng.Execute(ContextWithCursor(ContextWithFreshRead(reader), page.GetMeta().Cursor), `query libraryArtifactsBrowse()`)
	require.NoError(t, err)
	require.Len(t, next.Bundle.Nodes, 10)
	for _, id := range pageIDs(t, next) {
		require.NotContains(t, first, id)
	}

	baseline := fmt.Sprintf(`SELECT mn.* FROM
 (SELECT DISTINCT ON (id) id,"createdAt" FROM "MemoryNodes" WHERE concept='v1:library:artifact' ORDER BY id,"createdAt" DESC) latest_keys
 JOIN LATERAL (SELECT * FROM (SELECT * FROM "MemoryNodes" UNION ALL SELECT * FROM memory_liveness) observed
 WHERE id=latest_keys.id AND "createdAt"=latest_keys."createdAt" LIMIT 1) mn ON true
 WHERE payload->>'ownerUserId'='v1:identity:user:%s' AND payload->>'lens'='artifact'
 AND coalesce(payload->>'archived','false')<>'true' ORDER BY "createdAt" DESC,id DESC LIMIT 11`, owner)
	explain := func(query string) (float64, float64) {
		t.Helper()
		var raw []byte
		require.NoError(t, db.QueryRowContext(ctx, "EXPLAIN (ANALYZE, BUFFERS, TIMING OFF, FORMAT JSON) "+query).Scan(&raw))
		var results []struct {
			Plan      map[string]any
			Planning  float64 `json:"Planning Time"`
			Execution float64 `json:"Execution Time"`
		}
		require.NoError(t, json.Unmarshal(raw, &results))
		r := results[0]
		buffers := r.Plan["Shared Hit Blocks"].(float64) + r.Plan["Shared Read Blocks"].(float64)
		return buffers, r.Planning + r.Execution
	}
	beforeBuffers, beforeMS := explain(baseline)
	afterBuffers, afterMS := explain(scans[0])
	t.Logf("Library page: before %.0f buffers / %.1f ms; after %.0f buffers / %.1f ms", beforeBuffers, beforeMS, afterBuffers, afterMS)
	require.Less(t, afterBuffers, beforeBuffers/5, "a ten-row page must cost much less than the population")
	for _, query := range []string{`query artifactsForRun(runId: "selected")`, `query libraryArtifactsBrowseOldest()`, `query libraryArtifactsBrowse(folderId: "small")`} {
		result, queries := capturedScans(t, eng, hook, reader, query)
		require.NotEmpty(t, result.Bundle.Nodes)
		require.NotEmpty(t, queries)
		if !strings.Contains(query, "Oldest") {
			require.True(t, strings.Contains(queries[0], "payload->>'folderId'") || strings.Contains(queries[0], "payload->>'producedByRunId'"))
		}
		buffers, ms := explain(queries[0])
		t.Logf("%s: %.0f buffers / %.1f ms", query, buffers, ms)
		require.Less(t, buffers, beforeBuffers/5)
	}
}

// Production planner settings belong to this fixture's pool, not the whole
// test server: other suites exercise the CI image's own defaults.
func libraryPerformanceEngine(t *testing.T) (*MemQLEngine, *bun.DB) {
	t.Helper()
	sharedReadMergeEngine(t) // initialize the immutable registries and schema
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()),
		pgdriver.WithConnParams(map[string]any{"random_page_cost": "4", "effective_cache_size": "3GB", "work_mem": "8MB"}))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	var pageCost string
	require.NoError(t, db.QueryRowContext(context.Background(), `SHOW random_page_cost`).Scan(&pageCost))
	require.Equal(t, "4", pageCost, "benchmark must keep production planner settings")
	engine, err := New(db)
	require.NoError(t, err)
	t.Cleanup(engine.cache.close)
	require.NoError(t, bootPrivateEngine(engine, memorynodes.DefaultRegistry()))
	return engine, db
}
