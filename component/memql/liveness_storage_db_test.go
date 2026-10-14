package memql

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

func livenessEngines(t *testing.T) (*MemQLEngine, *MemQLEngine, *bun.DB, *bun.DB, context.Context) {
	t.Helper()
	_, admin, ctx := sharedReadMergeEngine(t)
	schema := fmt.Sprintf("liveness_%d", time.Now().UnixNano())
	_, err := admin.ExecContext(ctx, `CREATE SCHEMA ?`, bun.Ident(schema))
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = admin.ExecContext(context.Background(), `DROP SCHEMA ? CASCADE`, bun.Ident(schema)) })
	open := func() *bun.DB {
		db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()), pgdriver.WithConnParams(map[string]any{"search_path": schema}))), pgdialect.New())
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	db, other := open(), open()
	for _, table := range []string{"MemoryNodes", "SecretMemoryNodes"} {
		_, err = db.ExecContext(ctx, `CREATE TABLE ? (LIKE public.? INCLUDING ALL)`, bun.Ident(table), bun.Ident(table))
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, `SELECT public.create_hypertable(?::regclass,'createdAt',chunk_time_interval=>interval '1 day')`, `"`+table+`"`)
		require.NoError(t, err)
	}
	for _, name := range []string{"20261013020000_shared_row_schemas.up.sql", "20261013050000_liveness.up.sql"} {
		raw, err := os.ReadFile("../database/memory-nodes/migrations/" + name)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, string(raw))
		require.NoError(t, err)
	}
	_, err = db.ExecContext(ctx, `UPDATE memory_schema_storage SET references_enabled=true`)
	require.NoError(t, err)
	engine := func(db *bun.DB) *MemQLEngine {
		e, err := New(db)
		require.NoError(t, err)
		require.NoError(t, e.Init(memorynodes.DefaultRegistry()))
		return e
	}
	return engine(db), engine(other), db, other, clusterOwnerCtx("liveness-owner")
}

func TestLivenessObservationsReadAcrossReplicasWithoutVersionGrowth(t *testing.T) {
	first, second, db, other, ctx := livenessEngines(t)
	for _, id := range []string{"reader-a", "reader-b"} {
		health := "healthy"
		if id == "reader-b" {
			health = "offline"
		}
		runMutation(t, ctx, first, "createNode", map[string]any{"id": id, "nodeType": "bff", "address": "reader:50051", "health": health})
	}
	enabled, err := memorynodes.EnableLivenessStorageWhenReady(ctx, db, "reader-a")
	require.NoError(t, err)
	require.False(t, enabled, "an older/offline reader must hold the switch")
	_, err = db.ExecContext(ctx, `UPDATE memory_liveness_storage SET next_check_at=now()`)
	require.NoError(t, err)
	enabled, err = memorynodes.EnableLivenessStorageWhenReady(ctx, other, "reader-b")
	require.NoError(t, err)
	require.True(t, enabled)
	id := "v1:cluster:node:reader-a"
	var before time.Time
	require.NoError(t, db.QueryRowContext(ctx, `SELECT max("createdAt") FROM "MemoryNodes" WHERE id=?`, id).Scan(&before))
	for i := 0; i < 5; i++ {
		writer := first
		if i%2 == 1 {
			writer = second
		}
		runMutation(t, ctx, writer, "updateNodeHealth", map[string]any{"id": "reader-a", "health": "degraded", "lastSeen": time.Now().UTC().Format(time.RFC3339Nano)})
	}
	var history, current int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM "MemoryNodes" WHERE id=?`, id).Scan(&history))
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM memory_liveness WHERE id=?`, id).Scan(&current))
	require.Equal(t, 1, history)
	require.Equal(t, 1, current)
	// The other engine has neither the writer's local cache nor its session.
	rows, err := second.loadLatestNodes(ContextWithFreshRead(ctx), []string{id}, nil)
	require.NoError(t, err)
	require.Contains(t, string(rows[id].Payload), `"degraded"`)
	require.True(t, rows[id].CreatedAt.After(before))
	result, err := second.Execute(ContextWithFreshRead(ctx), `query staleClusterNodes()`)
	require.NoError(t, err)
	found := false
	for _, n := range result.Bundle.Nodes {
		if n.Id == id {
			found = true
			require.Equal(t, "degraded", n.Payload.Fields["health"].GetStringValue())
		}
	}
	require.True(t, found)
	// An explicitly stale decision cannot restore older state after another pulse.
	stale := ContextWithRowVersionFence(ctx, "v1:cluster:node", id, before)
	_, err = second.Execute(stale, `mutation updateNodeHealth(id: "reader-a", health: "healthy", lastSeen: "2026-10-13T00:00:00Z")`)
	require.ErrorIs(t, err, ErrRowVersionChanged)
	// Two independent engine connections deciding from the same observation
	// cannot both commit, even though they update the mutable table.
	witness := ContextWithRowVersionFence(ctx, "v1:cluster:node", id, rows[id].CreatedAt)
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, writer := range []*MemQLEngine{first, second} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := writer.Execute(witness, `mutation updateNodeHealth(id: "reader-a", health: "degraded", lastSeen: "2026-10-13T01:00:00Z")`)
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else {
			require.ErrorIs(t, err, ErrRowVersionChanged)
		}
	}
	require.Equal(t, 1, successes)
	// A business change is a version, includes the observed health, and clears
	// the old observation in the same transaction.
	runMutation(t, ctx, first, "createNode", map[string]any{"id": "reader-a", "nodeType": "bff", "address": "replacement:50051", "health": "degraded"})
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM "MemoryNodes" WHERE id=?`, id).Scan(&history))
	require.Equal(t, 2, history)
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM memory_liveness WHERE id=?`, id).Scan(&current))
	require.Zero(t, current)
	runMutation(t, ctx, first, "updateNodeHealth", map[string]any{"id": "reader-a", "health": "stopped", "lastSeen": time.Now().UTC().Format(time.RFC3339Nano)})
	_, err = db.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE id=? AND "createdAt"=?`, id, before)
	require.NoError(t, err)
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM memory_liveness WHERE id=?`, id).Scan(&current))
	require.Equal(t, 1, current, "retiring old history must preserve the current observation")
	_, err = db.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE id=?`, id)
	require.NoError(t, err)
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM memory_liveness WHERE id=?`, id).Scan(&current))
	require.Zero(t, current, "retirement must not leave a live orphan")
}

func TestLivenessWorkerAuthorizationAndReadinessUseCurrentRows(t *testing.T) {
	first, second, db, _, ctx := livenessEngines(t)
	_, err := db.ExecContext(ctx, `UPDATE memory_liveness_storage SET enabled=true`)
	require.NoError(t, err)
	owner := auth.ContextWithUserActor(context.Background(), "worker-owner")
	workerID := runMutation(t, owner, first, "createWorkerRegistration", map[string]any{"registrationId": "worker", "registeredAt": time.Now().UTC().Format(time.RFC3339Nano), "identityId": "credential", "name": "Original", "capabilities": []string{"HEADLESS"}, "concurrency": map[string]any{}})
	for i := 0; i < 4; i++ {
		runMutation(t, owner, second, "updateWorkerLastSeen", map[string]any{"registrationId": workerID, "lastSeenAt": time.Now().UTC().Format(time.RFC3339Nano), "connectedNodeId": "other-replica", "activeCount": i})
	}
	var count int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM "MemoryNodes" WHERE id=?`, workerID).Scan(&count))
	require.Equal(t, 1, count)
	rows, err := first.loadLatestNodes(ContextWithFreshRead(owner), []string{workerID}, nil)
	require.NoError(t, err)
	require.Contains(t, string(rows[workerID].Payload), `"activeCount": 3`)
	stranger := auth.ContextWithUserActor(context.Background(), "different-owner")
	_, err = second.Execute(stranger, fmt.Sprintf(`mutation updateWorkerLastSeen(registrationId: %s, lastSeenAt: "2026-10-13T01:00:00Z")`, langparser.QuoteString(workerID)))
	require.Error(t, err)
	internal := auth.ContextWithInternalOrigin(ctx)
	for i := 0; i < 3; i++ {
		runMutation(t, internal, first, "recordModuleReadiness", map[string]any{"rowId": "ready", "module": "ai", "nodeId": "reader", "nodeType": "agent", "state": "configured", "reportedAt": time.Now().UTC().Format(time.RFC3339Nano)})
	}
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM "MemoryNodes" WHERE concept='v1:platform:moduleReadiness'`).Scan(&count))
	require.Equal(t, 1, count)
	reports, err := second.readModuleReadinessRows(ContextWithFreshRead(ctx))
	require.NoError(t, err)
	require.Len(t, reports, 1)
	require.Equal(t, "configured", string(reports[0].State))
}
