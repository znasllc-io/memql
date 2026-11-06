package memql

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/auth"
)

// A machine's live reports are current observations, not versions of its
// registration (migration 20261106150000_worker_registration_observations).
//
// The stream-holding replica republishes sharingActivity every 15 s with a
// fresh observedAt, and every heartbeat flush re-asserts capacity and setup
// with the agent's reportedAt. Before the migration each of those writes
// appended a whole registration version, because the observation-field list
// predated the three fields. The test drives the real mutations from two
// engines (two replicas: neither holds the other's cache or session) and
// counts rows in the two tables, so it fails on a list that loses a field as
// well as on a reader that stops merging memory_liveness.
func TestWorkerLiveReportsAreObservationsNotRegistrationVersions(t *testing.T) {
	first, second, db, _, ctx := livenessEngines(t)
	applyLivenessMigration(t, ctx, db, "20261013060000_reconcile_observations.up.sql")
	applyLivenessMigration(t, ctx, db, "20261106150000_worker_registration_observations.up.sql")
	_, err := db.ExecContext(ctx, `UPDATE memory_liveness_storage SET enabled=true`)
	require.NoError(t, err)

	owner := auth.ContextWithUserActor(context.Background(), "worker-owner")
	workerID := runMutation(t, owner, first, "createWorkerRegistration", map[string]any{
		"registrationId": "observed", "registeredAt": time.Now().UTC().Format(time.RFC3339Nano),
		"identityId": "credential", "name": "Observed", "capabilities": []string{"HEADLESS"}, "concurrency": map[string]any{},
	})
	// updateWorkerSharingActivity is @serverOnly: only the stream holder
	// publishes counts, under the owner's actor.
	publisher := auth.ContextWithInternalOrigin(owner)
	var lastObserved, lastReported string
	for i := 0; i < 4; i++ {
		writer := first
		if i%2 == 1 {
			writer = second
		}
		lastObserved = time.Now().UTC().Format(time.RFC3339Nano)
		runMutation(t, publisher, writer, "updateWorkerSharingActivity", map[string]any{
			"registrationId": workerID,
			"sharingActivity": map[string]any{
				"nodeId": "agent-a", "startedAt": "2026-11-06T00:00:00Z", "observedAt": lastObserved,
				"ownerCalls": i, "sharedCalls": 0, "users": []any{}, "groups": []any{},
			},
		})
		lastReported = time.Now().UTC().Format(time.RFC3339Nano)
		runMutation(t, owner, writer, "updateWorkerLastSeen", map[string]any{
			"registrationId": workerID, "lastSeenAt": lastReported, "connectedNodeId": "agent-a",
			"capacity": map[string]any{
				"memoryTotalBytes": 68719476736, "memoryFreeBytes": 34359738368 - i, "memoryKind": "unified",
				"loadedModels": []any{}, "reportedAt": lastReported,
			},
			"setup": map[string]any{
				"state":      "running",
				"stages":     []any{map[string]any{"name": "models", "state": "running", "completedBytes": i * 1024, "totalBytes": 4096}},
				"reportedAt": lastReported,
			},
		})
	}
	history, current := registrationRowCounts(t, ctx, db, workerID)
	require.Equal(t, 1, history, "sharingActivity, capacity and setup reports appended registration versions")
	require.Equal(t, 1, current)

	// The other replica reads the observation, not the stale business version.
	rows, err := second.loadLatestNodes(ContextWithFreshRead(owner), []string{workerID}, nil)
	require.NoError(t, err)
	payload := string(rows[workerID].Payload)
	require.Contains(t, payload, lastObserved, "the latest sharing report is not what a reader sees")
	require.Contains(t, payload, `"ownerCalls": 3`)
	require.Contains(t, payload, `"completedBytes": 3072`)
	require.Contains(t, payload, lastReported)

	// A business change still appends a version, carries the observed values
	// with it, and clears the observation in the same transaction.
	runMutation(t, owner, first, "updateWorkerPermissions", map[string]any{
		"registrationId": workerID,
		"permissions":    map[string]any{"accessibility": true, "accessibility_state": "granted", "detail": "measured"},
	})
	history, current = registrationRowCounts(t, ctx, db, workerID)
	require.Equal(t, 2, history)
	require.Zero(t, current)
	var latest string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT payload::text FROM "MemoryNodes" WHERE id=? ORDER BY "createdAt" DESC LIMIT 1`, workerID).Scan(&latest))
	require.Contains(t, latest, lastObserved, "the business version dropped the observed sharing report")

	// The down migration restores the older list and is safe at any time: a
	// sharing-only report appends again rather than being lost.
	applyLivenessMigration(t, ctx, db, "20261106150000_worker_registration_observations.down.sql")
	runMutation(t, publisher, second, "updateWorkerSharingActivity", map[string]any{
		"registrationId":  workerID,
		"sharingActivity": map[string]any{"nodeId": "agent-a", "startedAt": "2026-11-06T00:00:00Z", "observedAt": time.Now().UTC().Format(time.RFC3339Nano), "ownerCalls": 0, "sharedCalls": 0, "users": []any{}, "groups": []any{}},
	})
	history, current = registrationRowCounts(t, ctx, db, workerID)
	require.Equal(t, 3, history)
	require.Zero(t, current)
}

func applyLivenessMigration(t *testing.T, ctx context.Context, db *bun.DB, name string) {
	t.Helper()
	raw, err := os.ReadFile("../database/memory-nodes/migrations/" + name)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, string(raw))
	require.NoError(t, err)
}

// registrationRowCounts answers the two questions the storage split turns on:
// how many business versions the record has, and whether a current
// observation sits beside them.
func registrationRowCounts(t *testing.T, ctx context.Context, db *bun.DB, id string) (history, current int) {
	t.Helper()
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM "MemoryNodes" WHERE id=?`, id).Scan(&history))
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM memory_liveness WHERE id=?`, id).Scan(&current))
	return history, current
}
