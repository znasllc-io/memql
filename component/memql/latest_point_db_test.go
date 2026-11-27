package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

func TestLatestPointReadVersionsObservationsAndSnapshot(t *testing.T) {
	e, db, _ := sharedReadMergeEngine(t)
	ctx := context.Background()
	id := "v1:worker:registration:" + uniqueSuffix("point")
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM memory_liveness WHERE id=?`, id)
		_, _ = db.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE id=?`, id)
	})
	_, err := db.ExecContext(ctx, `INSERT INTO "MemoryNodes" (id,concept,"createdAt","createdBy",payload,schema) VALUES (?,'v1:worker:registration',?,'point-fixture','{"status":"old"}','{}')`, id, at)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO memory_liveness (id,concept,"createdAt","createdBy",payload,schema) VALUES (?,'v1:worker:registration',?,'point-fixture','{"status":"observed"}','{}')`, id, at.Add(time.Minute))
	require.NoError(t, err)
	read := func(cutoff *time.Time) memorynodes.MemoryNode {
		t.Helper()
		rows, err := e.loadLatestNodes(ctx, []string{id, id}, cutoff)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		return rows[id]
	}
	require.JSONEq(t, `{"status":"observed"}`, string(read(nil).Payload))
	require.JSONEq(t, `{"status":"old"}`, string(read(&at).Payload))
	before := at.Add(-time.Second)
	rows, err := e.loadLatestNodes(ctx, []string{id}, &before)
	require.NoError(t, err)
	require.Empty(t, rows)
	rows, err = e.loadLatestNodes(ctx, []string{"missing-' OR true --"}, nil)
	require.NoError(t, err)
	require.Empty(t, rows, "ID is always a bound value")
	// No stale cached data, and no concept inferred from the ID prefix.
	_, err = db.ExecContext(ctx, `INSERT INTO "MemoryNodes" (id,concept,"createdAt","createdBy",payload,schema) VALUES (?,'v1:test:changed',?,'point-fixture','{"status":"changed"}','{}')`, id, at.Add(2*time.Minute))
	require.NoError(t, err)
	require.Equal(t, "v1:test:changed", read(nil).Concept)

	var volatility string
	var definer bool
	require.NoError(t, db.QueryRowContext(ctx, `SELECT provolatile, prosecdef FROM pg_proc WHERE oid='public.memql_latest_node(text,timestamptz)'::regprocedure`).Scan(&volatility, &definer))
	require.Equal(t, "s", volatility, "both internal reads must share the statement snapshot")
	require.False(t, definer, "the function must never elevate database privileges")

	// Replacing the mutable row repeatedly must not make it vanish between
	// selecting its timestamp and fetching its payload. Read from another pool
	// connection while each replacement commits independently.
	done := make(chan error, 1)
	go func() {
		for n := 0; n < 100; n++ {
			_, err := db.ExecContext(ctx, `UPDATE memory_liveness SET "createdAt"=?, payload=jsonb_build_object('version',?) WHERE id=?`, at.Add(3*time.Minute+time.Duration(n)*time.Microsecond), n, id)
			if err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	for n := 0; n < 100; n++ {
		read(nil)
	}
	require.NoError(t, <-done)
	require.Contains(t, string(read(nil).Payload), `"version": 99`)
}

func TestLatestPointReadAvoidsHistoricalPayloadPlanning(t *testing.T) {
	_, db := libraryPerformanceEngine(t)
	ctx := context.Background()
	id := uniqueSuffix("point-cost")
	_, err := db.ExecContext(ctx, `INSERT INTO "MemoryNodes" (id,concept,"createdAt","createdBy",payload,schema)
SELECT ?, 'v1:test:point-cost', TIMESTAMPTZ '2025-01-01'+day*interval '8 days'+v*interval '1 second',
'point-fixture',jsonb_build_object('version',v,'detail',repeat(md5(v::text),40)), '{}'::jsonb
FROM generate_series(0,13) day CROSS JOIN generate_series(1,500) v`, id)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = db.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE id=?`, id) })
	_, err = db.ExecContext(ctx, `ANALYZE "MemoryNodes"`)
	require.NoError(t, err)
	// Use one physical connection: the same per-connection plan reuse as the
	// production pool. It caches plans, not answers, and never needs warm data
	// from an earlier version of this engine.
	conn, err := db.Conn(ctx)
	require.NoError(t, err)
	defer conn.Close()
	baseline := fmt.Sprintf(`SELECT mn.* FROM
(SELECT DISTINCT ON(id) id,"createdAt" FROM %s observed WHERE id=? ORDER BY id,"createdAt" DESC) latest_keys
JOIN LATERAL(SELECT * FROM %s observed WHERE id=latest_keys.id AND "createdAt"=latest_keys."createdAt" LIMIT 1) mn ON true`, memorynodes.ObservedRowsSQL, memorynodes.ObservedRowsSQL)
	after := `SELECT * FROM public.memql_latest_node(?::text, NULL)`
	explain := func(query string) (float64, float64) {
		t.Helper()
		var raw []byte
		require.NoError(t, conn.QueryRowContext(ctx, "EXPLAIN (ANALYZE, BUFFERS, TIMING OFF, FORMAT JSON) "+query, id).Scan(&raw))
		var result []struct {
			Plan          map[string]any
			PlanningStats map[string]float64 `json:"Planning"`
			Planning      float64            `json:"Planning Time"`
			Execution     float64            `json:"Execution Time"`
		}
		require.NoError(t, json.Unmarshal(raw, &result))
		p := result[0]
		return p.Plan["Shared Hit Blocks"].(float64) + p.Plan["Shared Read Blocks"].(float64) + p.PlanningStats["Shared Hit Blocks"] + p.PlanningStats["Shared Read Blocks"], p.Planning + p.Execution
	}
	beforeBuffers, beforeMS := explain(baseline)
	_, coldMS := explain(after)
	afterBuffers, afterMS := explain(after)
	t.Logf("Point read, 7000 versions / 14 chunks: before %.0f total buffers / %.1f ms; first %.1f ms; reused plan %.0f buffers / %.1f ms", beforeBuffers, beforeMS, coldMS, afterBuffers, afterMS)
	require.Less(t, afterBuffers, beforeBuffers/5)
}
