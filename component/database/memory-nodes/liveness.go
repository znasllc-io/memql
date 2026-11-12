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
//
// A concept enters this set together with its own reader handshake (an
// Enable...WhenReady below and the storage switch it guards), because this
// list is what a reader knows: a reader without the concept here enumerates
// its latest keys from MemoryNodes alone (latestScanKeys, latestScanWalk) and
// reports every observation-only write to a held journal as a business write
// (executeWrite's observationOnly). Adding a FIELD to a listed concept's
// observation list in memory_node_store_liveness needs no handshake.
//
// v1:pipelines:run is listed for its lease renewal alone (driverHeartbeatAt,
// migration 20261108190000_pipeline_run_lease_observations): the lease is the
// run's liveness, every 30 s, and nothing reads a superseded renewal. No
// payload lookup or walk index serves the concept (payloadLookups,
// payloadWalks in component/memql), so listing it changes only which table its
// latest keys come from, not an index path.
func HasLiveness(concept string) bool {
	switch concept {
	case "v1:worker:registration", "v1:cluster:node", "v1:platform:moduleReadiness", "v1:platform:customDomain", "v1:platform:syncState", "v1:pipelines:run":
		return true
	default:
		return false
	}
}

// EnableLivenessStorageWhenReady waits for every non-stopped engine reader.
// An offline old binary may still serve reads, so it blocks this rollout.
func EnableLivenessStorageWhenReady(ctx context.Context, db *bun.DB, nodeID string) (bool, error) {
	return enableObservationsWhenReady(ctx, db, nodeID, "memory_liveness_storage", "memory_liveness_readers", true)
}

// EnableReconcileObservationsWhenReady has a separate handshake because a
// reader of the first liveness release does not enumerate these concepts from
// the observation table. Such a reader must block this second rollout.
func EnableReconcileObservationsWhenReady(ctx context.Context, db *bun.DB, nodeID string) (bool, error) {
	return enableObservationsWhenReady(ctx, db, nodeID, "memory_reconcile_observation_storage", "memory_reconcile_observation_readers", true)
}

// EnablePipelineRunObservationsWhenReady is the third handshake, for the
// pipeline run's lease renewal. A reader of the first two releases does not
// list v1:pipelines:run in HasLiveness and must block it; once every
// non-stopped node advertises here the renewal is an observation on the whole
// cluster. Like the other two it never turns itself off: observations already
// stored must stay readable.
//
// It runs only where schema references are enabled (SchemaCompactor.run), so
// on a cluster that never set MEMQL_DATABASE_SCHEMA_REFERENCES_ACTIVATE every
// renewal stays a version. Where references are already enabled (production
// since 2026-10-14) it activates once every replica runs an engine that
// advertises it: deploying that engine is the decision. Regression tests:
// TestPipelineRunObservationsWaitForTheirOwnReaderHandshake (component/memql)
// and TestLeaseRenewalsAreObservationsAcrossTwoReplicas
// (component/pipelinerun).
func EnablePipelineRunObservationsWhenReady(ctx context.Context, db *bun.DB, nodeID string) (bool, error) {
	return enableObservationsWhenReady(ctx, db, nodeID, "memory_pipeline_run_observation_storage", "memory_pipeline_run_observation_readers", true)
}

// enableObservationsWhenReady is the reader handshake behind every one-way
// switch that has a storage table and a readers table of this shape: the three
// observation stores above, and account stamping (account_stamping.go), which
// stores no observation and waits on the fleet for the same reason. It answers
// with the switch as the cluster has it. Once on, that is all it does.
//
// While the switch is off, a node with an id says it supports what the switch
// guards, on every call, and then tries to turn it on. It turns on when the
// caller is itself a node that has not stopped and every such node has said so
// within 30 seconds; the fleet is the latest version of each v1:cluster:node
// row, observations included, and is scanned at most once per five seconds for
// the whole cluster (next_check_at), not once per replica.
//
// allowActivation=false is the caller's hold, with the rule
// EnableSchemaReferencesWhenReady has: a held node still advertises, so that
// releasing the hold on one replica takes effect at its next check without the
// others being rolled first, and returns before trying to turn anything on. It
// is a hold on turning the switch on, never a way to turn it off. The three
// observation stores pass true: deploying an engine that advertises them is
// the decision.
func enableObservationsWhenReady(ctx context.Context, db *bun.DB, nodeID, storage, readers string, allowActivation bool) (bool, error) {
	var enabled bool
	if err := db.QueryRowContext(ctx, `SELECT enabled FROM ? WHERE singleton`, bun.Ident(storage)).Scan(&enabled); err != nil || enabled {
		return enabled, err
	}
	if strings.TrimSpace(nodeID) == "" {
		return false, nil
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO ? (node_id,seen_at) VALUES (?,clock_timestamp()) ON CONFLICT (node_id) DO UPDATE SET seen_at=excluded.seen_at`, bun.Ident(readers), nodeID); err != nil {
		return false, err
	}
	if !allowActivation {
		return false, nil
	}
	// staged-data: MUST-NOT-GATE -- storage rollout must include all engine
	// readers, irrespective of authored concept publication or caller identity.
	err := db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		// One topology scan per cluster per interval, not one per replica.
		var singleton bool
		err := tx.QueryRowContext(ctx, `SELECT singleton FROM ? WHERE singleton AND NOT enabled AND next_check_at<=clock_timestamp() FOR UPDATE SKIP LOCKED`, bun.Ident(storage)).Scan(&singleton)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE ? SET next_check_at=clock_timestamp()+interval '5 seconds' WHERE singleton`, bun.Ident(storage)); err != nil {
			return err
		}
		err = tx.QueryRowContext(ctx, `WITH latest AS MATERIALIZED (
		SELECT DISTINCT ON (id) id,payload FROM "MemoryNodesObserved"
		WHERE concept='v1:cluster:node' ORDER BY id,"createdAt" DESC
	), active AS (
		SELECT regexp_replace(id,'^v1:cluster:node:','') AS id FROM latest
		WHERE COALESCE(payload->>'health','') <> 'stopped'
	)
	UPDATE ? SET enabled=true
	WHERE singleton AND NOT enabled
	AND EXISTS(SELECT 1 FROM active WHERE id=?) AND NOT EXISTS(
		SELECT 1 FROM active a WHERE NOT EXISTS(
			SELECT 1 FROM ? r WHERE r.node_id=a.id AND r.seen_at>clock_timestamp()-interval '30 seconds'
		)
	) RETURNING enabled`, bun.Ident(storage), nodeID, bun.Ident(readers)).Scan(&enabled)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	})
	return enabled, err
}
