package memoryNodes

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/uptrace/bun"
)

// staged-data: MUST-NOT-GATE -- this shared row source serves both ordinary
// authorized reads and maintenance. Filtering here would hide staged rows from
// mutation read-merge and erase fields on the next write. Callers apply their
// own publication and row-admission rules after selecting the current version.
// ObservedRowsSQL includes the one current observation of a record.
// Historical observation samples are deliberately not retained. Business
// versions and deletion markers remain in MemoryNodes.
const ObservedRowsSQL = `(SELECT * FROM "MemoryNodes" UNION ALL SELECT * FROM memory_liveness)`

// HasLiveness identifies the closed set whose observation-only writes live
// outside the versioned store. All authorization still runs in the engine.
func HasLiveness(concept string) bool {
	switch concept {
	case "v1:worker:registration", "v1:cluster:node", "v1:platform:moduleReadiness":
		return true
	default:
		return false
	}
}

// EnableLivenessStorageWhenReady waits for every non-stopped engine reader.
// An offline old binary may still serve reads, so it blocks this rollout.
func EnableLivenessStorageWhenReady(ctx context.Context, db *bun.DB, nodeID string) (bool, error) {
	var enabled bool
	if err := db.QueryRowContext(ctx, `SELECT enabled FROM memory_liveness_storage WHERE singleton`).Scan(&enabled); err != nil || enabled {
		return enabled, err
	}
	if strings.TrimSpace(nodeID) == "" {
		return false, nil
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO memory_liveness_readers (node_id,seen_at) VALUES (?,clock_timestamp()) ON CONFLICT (node_id) DO UPDATE SET seen_at=excluded.seen_at`, nodeID); err != nil {
		return false, err
	}
	// staged-data: MUST-NOT-GATE -- storage rollout must include all engine
	// readers, irrespective of authored concept publication or caller identity.
	err := db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		// One topology scan per cluster per interval, not one per replica.
		var singleton bool
		err := tx.QueryRowContext(ctx, `SELECT singleton FROM memory_liveness_storage WHERE singleton AND NOT enabled AND next_check_at<=clock_timestamp() FOR UPDATE SKIP LOCKED`).Scan(&singleton)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE memory_liveness_storage SET next_check_at=clock_timestamp()+interval '5 seconds' WHERE singleton`); err != nil {
			return err
		}
		err = tx.QueryRowContext(ctx, `WITH latest AS MATERIALIZED (
		SELECT DISTINCT ON (id) id,payload FROM "MemoryNodesObserved"
		WHERE concept='v1:cluster:node' ORDER BY id,"createdAt" DESC
	), active AS (
		SELECT regexp_replace(id,'^v1:cluster:node:','') AS id FROM latest
		WHERE COALESCE(payload->>'health','') <> 'stopped'
	)
	UPDATE memory_liveness_storage SET enabled=true
	WHERE singleton AND NOT enabled
	AND EXISTS(SELECT 1 FROM active WHERE id=?) AND NOT EXISTS(
		SELECT 1 FROM active a WHERE NOT EXISTS(
			SELECT 1 FROM memory_liveness_readers r WHERE r.node_id=a.id AND r.seen_at>clock_timestamp()-interval '30 seconds'
		)
	) RETURNING enabled`, nodeID).Scan(&enabled)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	})
	return enabled, err
}
