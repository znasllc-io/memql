package node

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/uptrace/bun"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/metrics"
)

// READINESS ROWS OF A STOPPED NODE ARE DELETED, NOT MERELY UNCOUNTED
// (epic memql#5316 ruling D4, issue memql#5325).
//
// # What the liveness window already does, and what it leaves
//
// readiness.Fold keeps a report only from a node whose v1:cluster:node row
// carries a live health word inside NodeLiveWindow, so a stopped node's
// verdict stops counting within a minute of its last heartbeat and no screen
// ever reads it. That is the VERDICT half, and it shipped with the fold.
//
// What it leaves is the row. v1:platform:moduleReadiness is append-only and
// nothing ever removed one, so every pod that has ever booted keeps seven rows
// (one per module) in "MemoryNodes" forever, and a cluster that rolls daily
// adds seven per pod per rollout in perpetuity. Nobody reads them and nobody
// can: the fold cannot see them and no query filters on them. They are a table
// nobody pruned, which is the same thing memql#5199 says about a retired field
// -- invisible on a fresh database, and only ever measured on a real
// installation. One was measured: 772k versions for a few dozen live ids
// (2026-09-13).
//
// # Two paths, because there are two writers of health="stopped"
//
// TopologyReconciler.retire is the fast path and owns the transition within
// seconds; the pruneStaleClusterNodes cron is the lazy backstop for whatever
// the fast path missed. A purge hung on retire alone would leave the
// backstop's nodes behind, so this file carries both:
//
//   - purgeReadinessRowsForNode, called from retire the moment this process
//     has recorded the node as stopped. No inference: the node was declared
//     gone by this very statement.
//   - purgeReadinessRowsForStoppedNodes, a slow sweep keyed on cluster:node
//     rows whose LATEST version says stopped. It collects the backstop's
//     nodes and every row left behind by an engine that predates this change,
//     and it guesses nothing -- a row is removed because some writer recorded
//     its node as stopped, never because a node has not been seen lately.
//
// A node with NO cluster:node row at all is deliberately left alone. "No row"
// is also what a pod looks like in the seconds between its first readiness
// pass and its registration, and deleting on absence would race a booting
// node. A node that never registers is a different defect from the one this
// file closes.
//
// # Why this deletes directly, and what is not notified
//
// MemQL has no delete mutation -- the parser accepts insert and update and
// nothing else (component/language/parser/body_clauses.go) -- so a retention
// delete is a statement against the node table, on the pattern
// component/identity/authactivity/prune.go and
// component/node/delivery_store_pg.go established. Two consequences, stated
// rather than left to be rediscovered:
//
//   - EVERY VERSION of a row goes, not only the latest. "MemoryNodes" is keyed
//     (id, createdAt), and deleting the newest alone would leave the rest as
//     rows no read returns and no sweep finds again.
//   - Nothing is notified. There is no graph event and no subscriber fan-out,
//     which is correct here for the reason RoutingExclusions() records against
//     graph.node.deleted.v1:platform:moduleReadiness: the fold had already
//     stopped counting the row, so a delete event would change no word on any
//     screen.
//
// Run only by the reconciler's leader (one cluster-wide advisory lock), so a
// stop costs one statement rather than one per replica. Deletes are idempotent
// anyway -- a row a sibling already removed simply is not there.

const (
	// readinessPurgeInterval is how often the stopped-node sweep runs. Ten
	// minutes matches component/memql's readinessRewriteFloor, the longest a
	// live node's own row may go unrestated: below it the sweep would be
	// looking for rows the cluster has not finished writing, and above it a
	// backstop-retired node's rows would outlive every other trace of the pod.
	// The retire path is what makes the fast case fast; this bounds the rest.
	readinessPurgeInterval = 10 * time.Minute

	// The two metric paths. Named constants rather than literals at the call
	// sites, because the counter's cardinality is closed at exactly these two
	// and a third spelling would be a silent new series.
	readinessPurgePathRetired = "retired"
	readinessPurgePathSwept   = "swept"

	// clusterNodeIDPrefix composes a bare MEMQL_NODE_ID into the
	// v1:cluster:node row id that holds its health. The join goes in this
	// direction on purpose: adding the prefix to the readiness row's nodeId is
	// exact, where stripping it off the node id would have to assume a node id
	// contains no colon of its own.
	clusterNodeIDPrefix = memorynodes.ConceptClusterNode + ":"
)

// readinessRowPurger is the one storage operation this file performs. An
// interface so the reconciler's CALL of it is testable without Postgres; what
// the statements do is covered by readiness_row_purge_db_test.go against a
// real database.
type readinessRowPurger interface {
	// purgeForNode removes every version of every readiness row reported by
	// one node, and answers how many versions went.
	purgeForNode(ctx context.Context, nodeId string) (int64, error)
	// purgeForStoppedNodes removes the rows of every node whose latest
	// cluster:node row says stopped, and answers how many versions went.
	purgeForStoppedNodes(ctx context.Context) (int64, error)
}

// sqlReadinessRowPurger is the production purger, over the direct
// (non-pooled) handle the reconciler already holds for its lease.
type sqlReadinessRowPurger struct {
	db *bun.DB
}

// staged-data: MUST-NOT-GATE -- a staged row omitted here becomes PERMANENTLY
// UNPRUNABLE, exactly as component/identity/authactivity/prune.go's delete is.
// This purge and the sweep below are the ONLY things that ever remove a
// v1:platform:moduleReadiness row -- the DSL has no delete mutation -- so a row
// the gate hides is a row nothing will remove again: it belongs to a pod that
// no longer exists, no read will ever return it, and the growth this file
// exists to stop resumes silently while the counter reports work being done.
func (p sqlReadinessRowPurger) purgeForNode(ctx context.Context, nodeId string) (int64, error) {
	// The blank guard comes FIRST, before the nil-db shortcut, so it is a
	// refusal on every build rather than only on one with a database: a blank
	// nodeId matches every row whose payload carries no nodeId at all, which
	// is a purge of the whole concept dressed as one node's.
	if nodeId == "" {
		return 0, fmt.Errorf("readiness purge: blank node id")
	}
	if p.db == nil {
		return 0, nil
	}
	res, err := p.db.ExecContext(ctx,
		`DELETE FROM "MemoryNodes" WHERE concept = ? AND payload->>'nodeId' = ?`,
		memqlengine.ModuleReadinessConcept, nodeId)
	if err != nil {
		return 0, fmt.Errorf("readiness purge: node %s: %w", nodeId, err)
	}
	return rowsAffected(res), nil
}

// staged-data: MUST-NOT-GATE -- the unprunable-row bug above, and a second one
// this statement has and the per-node purge does not. Its sub-select takes the
// LATEST version of each v1:cluster:node row to read that node's current
// health. Gated, "latest" would mean latest-among-visible: a hidden newer
// version reading `healthy` leaves an older `stopped` one deciding, and the
// sweep deletes the readiness rows of a node that is serving right now. That
// heals on the node's next pass (at most one safety-net period) but it is a
// live node's rows removed on evidence the gate manufactured, which is a
// different and worse failure than leaving a dead pod's rows behind.
func (p sqlReadinessRowPurger) purgeForStoppedNodes(ctx context.Context) (int64, error) {
	if p.db == nil {
		return 0, nil
	}
	// DISTINCT ON (id) ... ORDER BY id, "createdAt" DESC is the latest version
	// of each cluster:node row, which is the only one whose health is the
	// node's current answer. Reading any version would purge a node that was
	// briefly stopped and has since come back under the same pod name.
	res, err := p.db.ExecContext(ctx,
		`DELETE FROM "MemoryNodes" r
		  WHERE r.concept = ?
		    AND (? || (r.payload->>'nodeId')) IN (
		      SELECT n.id FROM (
		        SELECT DISTINCT ON (id) id, payload
		          FROM "MemoryNodes"
		         WHERE concept = ?
		         ORDER BY id, "createdAt" DESC
		      ) AS n
		      WHERE n.payload->>'health' = 'stopped'
		    )`,
		memqlengine.ModuleReadinessConcept, clusterNodeIDPrefix, memorynodes.ConceptClusterNode)
	if err != nil {
		return 0, fmt.Errorf("readiness purge: stopped nodes: %w", err)
	}
	return rowsAffected(res), nil
}

func rowsAffected(res sql.Result) int64 {
	n, err := res.RowsAffected()
	if err != nil {
		return 0
	}
	return n
}

// purger builds the purger from the reconciler's own db handle, or nil when
// there is none (a DB-less binary or a test's no-op loop).
func (r *TopologyReconciler) purger() readinessRowPurger {
	if r.readinessPurger != nil {
		return r.readinessPurger
	}
	if r.dbGetter == nil {
		return nil
	}
	db := r.dbGetter()
	if db == nil {
		return nil
	}
	return sqlReadinessRowPurger{db: db}
}

// purgeReadinessRowsForNode removes one retired node's readiness rows. Called
// from retire after the terminal health row landed, so the delete never runs
// against a node this process failed to record as stopped.
//
// A failure is logged and dropped: the node IS stopped, the fold has already
// stopped counting it, and the sweep below will collect the rows on its next
// pass. Failing the retire over its cleanup would leave a live-looking node
// row behind, which is the worse of the two.
func (r *TopologyReconciler) purgeReadinessRowsForNode(ctx context.Context, nodeId string) {
	p := r.purger()
	if p == nil {
		return
	}
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	versions, err := p.purgeForNode(pctx, nodeId)
	if err != nil {
		r.warn("topology reconciler: readiness row purge failed; the sweep will collect them",
			"node_id", nodeId, "error", err)
		return
	}
	if versions > 0 {
		metrics.ModuleReadinessRowsPurged(readinessPurgePathRetired, versions)
		r.info("topology reconciler purged a stopped node's readiness rows",
			"node_id", nodeId, "versions", versions)
	}
}

// sweepReadinessRowsOfStoppedNodes runs the slow sweep, at most once per
// readinessPurgeInterval. Called from the leader's tick.
func (r *TopologyReconciler) sweepReadinessRowsOfStoppedNodes(ctx context.Context) {
	now := r.now()
	if !r.lastReadinessPurge.IsZero() && now.Sub(r.lastReadinessPurge) < readinessPurgeInterval {
		return
	}
	p := r.purger()
	if p == nil {
		return
	}
	r.lastReadinessPurge = now
	pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	versions, err := p.purgeForStoppedNodes(pctx)
	if err != nil {
		r.warn("topology reconciler: readiness row sweep failed; retrying next interval", "error", err)
		return
	}
	if versions > 0 {
		metrics.ModuleReadinessRowsPurged(readinessPurgePathSwept, versions)
		r.info("topology reconciler swept readiness rows of stopped nodes", "versions", versions)
	}
}
