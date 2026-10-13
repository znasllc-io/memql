package memql

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/core/common"
)

func TestReconcileObservationsKeepCadenceAndFreshnessWithoutHistory(t *testing.T) {
	first, second, db, other, ctx := livenessEngines(t)
	raw, err := os.ReadFile("../database/memory-nodes/migrations/20261013060000_reconcile_observations.up.sql")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, string(raw))
	require.NoError(t, err)
	for _, id := range []string{"a", "b"} {
		runMutation(t, ctx, first, "createNode", map[string]any{"id": id, "nodeType": "bff", "address": "replica:50051", "health": "healthy"})
	}
	enabled, err := memorynodes.EnableReconcileObservationsWhenReady(ctx, db, "a")
	require.NoError(t, err)
	require.False(t, enabled)
	// Merely supporting heartbeats is insufficient for these two concepts.
	_, err = db.ExecContext(ctx, `INSERT INTO memory_liveness_readers VALUES ('b',now())`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `UPDATE memory_reconcile_observation_storage SET next_check_at=now()`)
	require.NoError(t, err)
	enabled, err = memorynodes.EnableReconcileObservationsWhenReady(ctx, db, "a")
	require.NoError(t, err)
	require.False(t, enabled)
	_, err = db.ExecContext(ctx, `UPDATE memory_reconcile_observation_storage SET next_check_at=now()`)
	require.NoError(t, err)
	enabled, err = memorynodes.EnableReconcileObservationsWhenReady(ctx, other, "b")
	require.NoError(t, err)
	require.True(t, enabled)
	ctx = auth.ContextWithInternalOrigin(ctx)
	syncArgs := map[string]any{"stateId": "steady", "conceptId": "v1:test:mirror", "connector": "test", "direction": "inbound", "lastReconcileAt": "2026-10-13T00:00:00Z", "lastAttemptAt": "2026-10-13T00:00:00Z"}
	syncID := runMutation(t, ctx, first, "upsertSyncState", syncArgs)
	domainConcept, err := first.concepts.Get("v1:platform:customDomain")
	require.NoError(t, err)
	schema, err := domainConcept.DefinitionSchema()
	require.NoError(t, err)
	domainID := "v1:platform:customDomain:steady"
	_, err = db.ExecContext(ctx, `INSERT INTO "MemoryNodes" (id,"createdAt","createdBy",concept,type,schema,payload,provenance) VALUES (?,now(),'fixture','v1:platform:customDomain','object',?::jsonb,'{"siteId":"v1:platform:site:fixture","hostname":"fixture.example.net","token":"token","status":"verifying","failureReason":"dns_not_found","failureDetail":"Waiting"}','{}')`, domainID, string(schema))
	require.NoError(t, err)
	writes := 0
	ctx = common.ContextWithWriteObserver(ctx, func(concept, id string) { writes++ })
	for i := 1; i <= 4; i++ {
		stamp := time.Date(2026, 10, 13, 0, i, 0, 0, time.UTC).Format(time.RFC3339)
		syncArgs["lastReconcileAt"], syncArgs["lastAttemptAt"] = stamp, stamp
		runMutation(t, ctx, second, "upsertSyncState", syncArgs)
		runMutation(t, ctx, second, "recordCustomDomainCheck", map[string]any{"domainId": domainID, "failureReason": "dns_not_found", "failureDetail": "Waiting", "lastCheckedAt": stamp})
	}
	require.Zero(t, writes, "checks that change no business state must leave a held journal closed")
	for _, id := range []string{syncID, domainID} {
		var count int
		require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM "MemoryNodes" WHERE id=?`, id).Scan(&count))
		require.Equal(t, 1, count)
		require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM memory_liveness WHERE id=?`, id).Scan(&count))
		require.Equal(t, 1, count)
	}
	rows, err := first.loadLatestNodes(ContextWithFreshRead(ctx), []string{syncID, domainID}, nil)
	require.NoError(t, err)
	require.Contains(t, string(rows[syncID].Payload), `"lastReconcileAt": "2026-10-13T00:04:00Z"`)
	require.Contains(t, string(rows[domainID].Payload), `"lastCheckedAt": "2026-10-13T00:04:00Z"`)
	// A changed failure is durable and opens the held journal exactly once.
	syncArgs["lastError"] = "origin refused"
	runMutation(t, ctx, first, "upsertSyncState", syncArgs)
	require.Equal(t, 1, writes)
	runMutation(t, ctx, second, "upsertSyncState", syncArgs)
	require.Equal(t, 1, writes, "an identical failure is a no-op")
}
