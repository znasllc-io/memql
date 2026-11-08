package memql

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// A pipeline run's lease renewal is a current observation only once every
// reader says it reads one (migration 20261108190000_pipeline_run_lease_observations).
//
// The concept set is what a reader must know: a reader that does not list
// v1:pipelines:run in memorynodes.HasLiveness enumerates the run's latest keys
// from MemoryNodes alone and reports every renewal to a held journal as a
// business write. So this concept has its own handshake, and a reader that
// advertised only the two earlier ones must hold it. The renewals are the
// driver's own mutation, unfenced by the caller (the engine fences a write to
// a concept in the set itself), from two engines.
func TestPipelineRunObservationsWaitForTheirOwnReaderHandshake(t *testing.T) {
	first, second, db, other, ctx := livenessEngines(t)
	applyLivenessMigration(t, ctx, db, "20261013060000_reconcile_observations.up.sql")
	applyLivenessMigration(t, ctx, db, "20261106150000_worker_registration_observations.up.sql")
	applyLivenessMigration(t, ctx, db, "20261108190000_pipeline_run_lease_observations.up.sql")
	for _, id := range []string{"a", "b"} {
		runMutation(t, ctx, first, "createNode", map[string]any{"id": id, "nodeType": "agent", "address": "replica:50051", "health": "healthy"})
	}
	// Both earlier observation stores are on: every reader merges
	// memory_liveness and knows the five earlier concepts.
	_, err := db.ExecContext(ctx, `UPDATE memory_liveness_storage SET enabled=true`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `UPDATE memory_reconcile_observation_storage SET enabled=true`)
	require.NoError(t, err)

	// The run, written as the driver writes it: under its owner's borrowed
	// authority, stamped internal (both mutations are @serverOnly).
	driver := auth.ContextWithInternalOrigin(auth.ContextWithUserActor(context.Background(), "pipeline-owner"))
	runID := runMutation(t, driver, first, "createPipelineRun", map[string]any{
		"runId": "observed", "pipelineId": "observed-pipeline", "repository": "acme/shop",
		"sha": strings.Repeat("a", 40), "mode": "affected", "event": "pull_request", "runKey": "observed-key",
		"attempt": 1, "trigger": "webhook", "status": "queued", "queuedAt": "2026-11-08T12:00:00Z",
	})
	runMutation(t, driver, first, "updatePipelineRun", map[string]any{
		"runId": "observed", "status": "in_progress", "driverNodeId": "a", "driverLeaseId": "lease-a",
		"driverHeartbeatAt": "2026-11-08T12:00:00Z",
	})
	renew := func(eng *MemQLEngine, at string) {
		t.Helper()
		runMutation(t, driver, eng, "updatePipelineRun", map[string]any{"runId": "observed", "driverHeartbeatAt": at})
	}

	// Off until every reader says it reads them: a renewal is still a version.
	renew(second, "2026-11-08T12:00:30Z")
	history, current := registrationRowCounts(t, ctx, db, runID)
	require.Equal(t, 3, history, "a renewal must stay a version while the switch is off")
	require.Zero(t, current)

	enabled, err := memorynodes.EnablePipelineRunObservationsWhenReady(ctx, db, "a")
	require.NoError(t, err)
	require.False(t, enabled, "replica b has not said it reads pipeline-run observations")
	// Advertising the two earlier handshakes says nothing about this concept.
	_, err = db.ExecContext(ctx, `INSERT INTO memory_liveness_readers VALUES ('b',now())`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO memory_reconcile_observation_readers VALUES ('b',now())`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `UPDATE memory_pipeline_run_observation_storage SET next_check_at=now()`)
	require.NoError(t, err)
	enabled, err = memorynodes.EnablePipelineRunObservationsWhenReady(ctx, db, "a")
	require.NoError(t, err)
	require.False(t, enabled, "a reader of the earlier observation stores enabled this one")
	_, err = db.ExecContext(ctx, `UPDATE memory_pipeline_run_observation_storage SET next_check_at=now()`)
	require.NoError(t, err)
	enabled, err = memorynodes.EnablePipelineRunObservationsWhenReady(ctx, other, "b")
	require.NoError(t, err)
	require.True(t, enabled)

	// On: renewals from either replica replace one current observation.
	var last string
	for i := 1; i <= 4; i++ {
		writer := first
		if i%2 == 0 {
			writer = second
		}
		last = time.Date(2026, 11, 8, 12, 1, 0, 0, time.UTC).Add(time.Duration(i) * 30 * time.Second).Format(time.RFC3339)
		renew(writer, last)
	}
	history, current = registrationRowCounts(t, ctx, db, runID)
	require.Equal(t, 3, history, "renewals appended versions of the run")
	require.Equal(t, 1, current)
	rows, err := second.loadLatestNodes(ContextWithFreshRead(ctx), []string{runID}, nil)
	require.NoError(t, err)
	require.Contains(t, string(rows[runID].Payload), `"driverHeartbeatAt": "`+last+`"`)

	// Once on, the switch stays on: a reader that stops advertising does not
	// turn it off, because rows already live in the observation table.
	_, err = db.ExecContext(ctx, `DELETE FROM memory_pipeline_run_observation_readers`)
	require.NoError(t, err)
	enabled, err = memorynodes.EnablePipelineRunObservationsWhenReady(ctx, db, "a")
	require.NoError(t, err)
	require.True(t, enabled)

	// Deleting superseded versions keeps the observation; retiring the whole
	// run, as integrations/work's operational retention does, takes it too.
	_, err = db.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE id=? AND "createdAt"<(SELECT max("createdAt") FROM "MemoryNodes" WHERE id=?)`, runID, runID)
	require.NoError(t, err)
	history, current = registrationRowCounts(t, ctx, db, runID)
	require.Equal(t, 1, history)
	require.Equal(t, 1, current, "deleting superseded versions removed the current observation")
	_, err = db.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE id=?`, runID)
	require.NoError(t, err)
	history, current = registrationRowCounts(t, ctx, db, runID)
	require.Zero(t, history)
	require.Zero(t, current, "retiring the run left its observation readable on its own")
}
