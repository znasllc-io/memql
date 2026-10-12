package node

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/uptrace/bun"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/metrics"
)

// SUPERSEDED STATUS HISTORY IS DELETED AS IT AGES, NOT ARCHIVED
// (the 2026-10-11 storage incident; decision 1 of
// docs/superpowers/specs/2026-10-11-storage-growth-stability-design.md).
//
// # What filled the disk
//
// "MemoryNodes" is append-only: a write is a new version of a row, never an
// update in place. Three concepts are written as heartbeats, so every beat adds
// a whole new version of a row whose previous version it replaced:
//
//   - v1:worker:registration, one per connected machine every 15 s. The cadence
//     is left alone on purpose: a machine's `online` is derived from it
//     (memql#4350).
//   - v1:cluster:node, one per node every 60 s (SelfStatusWriter).
//   - v1:platform:moduleReadiness, restated at least every ten minutes
//     (readinessRewriteFloor) and at once when a verdict changes.
//
// Nothing ever removed a superseded one. On 2026-10-11 an installation with no
// user activity filled its 32Gi database volume and was down for ten hours;
// 738,626 of the rows were superseded v1:worker:registration versions for 13
// workers, and the three concepts together were about 40% of the rows written.
//
// # What is deleted, and what is never touched
//
// A version goes when BOTH hold: a newer version of the same id exists, and it
// is more than statusHistoryGrace old.
//
// The first clause is what makes the latest version of every id untouchable.
// Nothing is newer than it, so however old it is -- a worker that has been
// offline for a month -- it stays, and it is that id's whole current state. The
// second clause leaves the last hour of beats in place for anything still
// reading the version before the latest, and for an operator working out why a
// machine flapped.
//
// # Why it deletes directly, and why there is no archive
//
// MemQL has no delete mutation (readiness_row_purge.go records the parser fact),
// so this is a statement against the node table, on the pattern that file and
// component/identity/authactivity/prune.go established. Nothing is notified: a
// subscriber to these concepts wants the newest beat, and the newest beat is
// exactly what stays.
//
// There is no archive. The work journal and the log store archive before they
// delete because their rows are a record someone may ask for; a superseded
// heartbeat records only that a machine was alive at an instant the next
// heartbeat also covers. Writing them somewhere else first would move the
// growth, not stop it. It is the ruling the moduleReadiness collapse
// (component/database/20260921000000_module_readiness_history_collapse.go)
// already made for one of these three concepts.
//
// # Why it runs continuously
//
// A deleted row does not give disk back by itself. It is dead space inside the
// chunk that holds it, and PostgreSQL reuses that space only for rows inserted
// into the SAME chunk. "MemoryNodes" is a hypertable and every insert lands in
// its newest chunk, so:
//
//   - a version removed while it is still in the current chunk is space the
//     heartbeats being written there reuse, and the chunk stops growing;
//   - a version removed after its chunk has closed is space nothing will ever
//     be written into again. The delete is correct and the volume does not
//     shrink.
//
// A nightly job, or a one-off purge of history, is therefore the wrong shape for
// these concepts: by the time it ran, a day of beats would already sit in
// closed chunks. Every ten minutes with an hour's grace removes nearly every
// version while it is still in the chunk that took it. The backlog an upgrade
// finds is the one exception: it already lives in closed chunks, so clearing it
// stops the growth but does not by itself return their disk.
//
// # How every statement stays bounded
//
// One statement deletes at most statusHistoryBatch versions of one concept, and
// each call gets statusHistoryStatementTimeout -- inside the request pool's 8 s
// statement_timeout (the direct handle the reconciler uses takes the same
// session parameters) and pgdriver's 10 s read deadline. A client that gives up
// at the read deadline sends no cancel (memql#5604), so a statement that
// outlived its caller would run on until the server's own timeout ended it; the
// batch is sized so that is never the usual case. The candidate scan and the
// newer-version check both read the latest-row index (concept, id, "createdAt"
// DESC) that 20260913000000_memory_nodes_concept_id_created_at_idx.go ensures,
// and the delete joins back on the primary key (id, "createdAt").
//
// Measured on TimescaleDB 2.29.0 over the incident's shape -- 738,621 versions of
// 13 ids in two chunks, about 0.6 KB of payload per row (a production row is
// wider: it carries its concept schema), autovacuum at its defaults, on a
// development laptop: draining the whole backlog took 1,472 batches and six
// minutes of statement time, each batch 0.2 s at the median, 0.6 s at the 95th
// percentile and 1.0 s at the worst, against the 7 s deadline. The planner
// answers "is there a newer version" by hashing the concept's rows rather than
// probing per candidate, so a batch costs the size of the concept and not of the
// batch: the backlog is the expensive case, and once it is gone a concept holds a
// few thousand versions and a batch takes milliseconds. Should a backlog ever
// push a batch toward the deadline, a LATERAL probe per candidate in place of the
// EXISTS plans as a nested loop instead (24 ms for the first batch on the same
// data, three and a half minutes for the whole drain).
//
// A sweep asks a concept again for as long as a batch comes back full, and moves
// on once a batch comes back short, so the steady state is one cheap statement
// per concept. A backlog drains over several intervals: the sweep stops after
// the call that crosses statusHistoryRunBudget rather than holding the leader's
// tick for as long as the backlog lasts, since the tick is also what retires
// dead nodes.
//
// Run only by the reconciler's leader (one cluster-wide advisory lock), so a
// batch costs one statement for the cluster rather than one per replica. Deletes
// are idempotent anyway: a version a sibling already removed is simply not there.

// statusHistoryConcepts is the closed list this file ever deletes from. Closed
// on purpose: the statement removes every superseded version of whatever concept
// it is given, and these are the three heartbeat concepts whose superseded
// versions the design record rules nobody needs. A fourth is a decision to be
// made in review, not a string passed by a caller, so the purger refuses
// anything outside it.
var statusHistoryConcepts = []string{
	memqlengine.WorkerRegistrationConcept, // v1:worker:registration, the largest by far
	memorynodes.ConceptClusterNode,        // v1:cluster:node
	memqlengine.ModuleReadinessConcept,    // v1:platform:moduleReadiness
}

const (
	// statusHistoryGrace is how old a superseded version must be before it may
	// go. See "What is deleted" above.
	statusHistoryGrace = time.Hour

	// statusHistoryPurgeInterval is how often a sweep runs. Ten minutes keeps
	// the work in the chunk that took the beats (see "Why it runs
	// continuously"), and it keeps a sweep small: in steady state it removes
	// what the last ten minutes of beats superseded, 40 versions per connected
	// machine and 10 per node, which is a batch or two.
	statusHistoryPurgeInterval = 10 * time.Minute

	// statusHistoryBatch is the most versions one statement deletes.
	statusHistoryBatch = 500

	// statusHistoryRunBudget is how long one sweep keeps going. It is checked
	// after each call, so a sweep can overshoot it by the call in flight, at most
	// statusHistoryStatementTimeout.
	statusHistoryRunBudget = 25 * time.Second

	// statusHistoryStatementTimeout is the deadline of each call.
	statusHistoryStatementTimeout = 7 * time.Second
)

// statusHistoryPurger is the one storage operation this file performs. An
// interface so the sweep's loop is testable without Postgres; what the statement
// deletes is covered by status_history_purge_db_test.go against a real database.
type statusHistoryPurger interface {
	// purgeSuperseded removes up to limit versions of concept that are older
	// than olderThan and have a newer version of the same id, and answers how
	// many went.
	purgeSuperseded(ctx context.Context, concept string, olderThan time.Time, limit int) (int64, error)
}

// sqlStatusHistoryPurger is the production purger, over the direct
// (non-pooled) handle the reconciler already holds for its lease.
type sqlStatusHistoryPurger struct {
	db *bun.DB
}

// staged-data: MUST-NOT-GATE -- a staged row omitted here becomes PERMANENTLY
// UNPRUNABLE, exactly as readiness_row_purge.go's deletes are. This statement is
// the ONLY thing that ever removes a superseded version of these three concepts
// -- the DSL has no delete mutation -- so a version the gate hides is one nothing
// removes again, and the growth this file exists to stop resumes silently while
// the counter reports work being done. It has a second failure the readiness
// per-node purge does not: the EXISTS sub-select is what decides a version is
// SUPERSEDED. Gated, a hidden newer version leaves the older ones standing as if
// they were current, and the sweep reports a clean pass over a table that is not.
func (p sqlStatusHistoryPurger) purgeSuperseded(ctx context.Context, concept string, olderThan time.Time, limit int) (int64, error) {
	// The refusals come FIRST, before the nil-db shortcut, so they are refusals
	// on every build rather than only on one with a database. A concept outside
	// the list would be somebody's history -- an identity's past credentials, a
	// work run's past states -- deleted by a typo.
	if !slices.Contains(statusHistoryConcepts, concept) {
		return 0, fmt.Errorf("status history purge: %q is not a status history concept", concept)
	}
	if limit <= 0 {
		return 0, fmt.Errorf("status history purge: batch limit %d is not positive", limit)
	}
	if p.db == nil {
		return 0, nil
	}
	// The sub-select is the bound: it names at most `limit` victims, and the
	// delete is a join on the primary key back to exactly those versions. The
	// concept is repeated on the outer table although (id, "createdAt") already
	// names one row: whatever the sub-select returned, this statement can never
	// delete a version of another concept.
	res, err := p.db.ExecContext(ctx,
		`DELETE FROM "MemoryNodes" m
		  USING (
		    SELECT n.id, n."createdAt"
		      FROM "MemoryNodes" n
		     WHERE n.concept = ? AND n."createdAt" < ?
		       AND EXISTS (SELECT 1 FROM "MemoryNodes" newer
		                    WHERE newer.concept = n.concept AND newer.id = n.id
		                      AND newer."createdAt" > n."createdAt")
		     LIMIT ?
		  ) d
		 WHERE m.concept = ? AND m.id = d.id AND m."createdAt" = d."createdAt"`,
		concept, olderThan, limit, concept)
	if err != nil {
		return 0, fmt.Errorf("status history purge: %s: %w", concept, err)
	}
	return rowsAffected(res), nil
}

// historyPurger builds the purger from the reconciler's own db handle, or nil
// when there is none (a DB-less binary or a test's no-op loop). The same handle
// purger() builds the readiness purger from.
func (r *TopologyReconciler) historyPurger() statusHistoryPurger {
	if r.statusPurger != nil {
		return r.statusPurger
	}
	if r.dbGetter == nil {
		return nil
	}
	db := r.dbGetter()
	if db == nil {
		return nil
	}
	return sqlStatusHistoryPurger{db: db}
}

// sweepStatusHistory runs the continuous purge, at most once per
// statusHistoryPurgeInterval. Called from the leader's tick.
func (r *TopologyReconciler) sweepStatusHistory(ctx context.Context) {
	now := r.now()
	if !r.lastStatusHistoryPurge.IsZero() && now.Sub(r.lastStatusHistoryPurge) < statusHistoryPurgeInterval {
		return
	}
	p := r.historyPurger()
	if p == nil {
		return
	}
	// Stamped before the work, as the readiness sweep does: a sweep that fails
	// or runs out of budget is retried at the next interval, not on the next
	// tick three seconds later.
	r.lastStatusHistoryPurge = now

	// One cutoff for the whole sweep. A version that crosses it while the sweep
	// runs is simply the next sweep's.
	olderThan := now.Add(-statusHistoryGrace)

	var total int64
	var perConcept []any
	for _, concept := range statusHistoryConcepts {
		versions, more := r.purgeStatusConcept(ctx, p, concept, olderThan, now)
		if versions > 0 {
			total += versions
			perConcept = append(perConcept, concept, versions)
		}
		if !more {
			break
		}
	}
	if total > 0 {
		elapsed := r.now().Sub(now)
		attrs := []any{"versions", total, "elapsed", elapsed.Round(time.Millisecond).String(), "budget_spent", elapsed >= statusHistoryRunBudget}
		r.info("topology reconciler purged superseded status history", append(attrs, perConcept...)...)
	}
}

// purgeStatusConcept runs one concept's batches for as long as they come back
// full, and answers how many versions went and whether the sweep may go on to
// the next concept. It may not once the run budget is spent -- checked after
// every call, a failing one included, so the call that crosses it is the last --
// or the context is done.
//
// A batch that fails ends only this concept: the others are independent, and the
// sweep is the only thing keeping the table from growing again.
func (r *TopologyReconciler) purgeStatusConcept(ctx context.Context, p statusHistoryPurger, concept string, olderThan, started time.Time) (versions int64, more bool) {
	for ctx.Err() == nil {
		n, err := r.purgeStatusBatch(ctx, p, concept, olderThan)
		versions += n
		if r.now().Sub(started) >= statusHistoryRunBudget {
			return versions, false
		}
		if err != nil || n < statusHistoryBatch {
			return versions, true
		}
	}
	return versions, false
}

// purgeStatusBatch makes one call. A failure is logged and dropped; a success
// is counted at once, so a batch that committed is on the counter even when the
// next one fails.
func (r *TopologyReconciler) purgeStatusBatch(ctx context.Context, p statusHistoryPurger, concept string, olderThan time.Time) (int64, error) {
	pctx, cancel := context.WithTimeout(ctx, statusHistoryStatementTimeout)
	defer cancel()
	n, err := p.purgeSuperseded(pctx, concept, olderThan, statusHistoryBatch)
	if err != nil {
		r.warn("topology reconciler: status history purge failed; retrying next interval",
			"concept", concept, "error", err)
		return 0, err
	}
	metrics.StatusHistoryVersionsPurged(concept, n)
	return n, nil
}
