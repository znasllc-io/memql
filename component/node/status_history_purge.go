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
// outlived its caller would run on until the server's own timeout ended it; and
// a statement that is cancelled rolls back whole, so a batch that cannot finish
// inside the deadline removes nothing on every attempt. A purge whose batches
// cost more as the backlog grows would stall on exactly the installations that
// need it. A batch therefore has to cost what it deletes and not what the table
// holds.
//
// That is why "has a newer version" is a LATERAL probe with LIMIT 1 and not an
// EXISTS. A subquery with a LIMIT cannot be pulled up into a join, so the
// planner runs it once per candidate as a probe of the latest-row index
// (concept, id, "createdAt" DESC) that
// 20260913000000_memory_nodes_concept_id_created_at_idx.go ensures. The natural
// form, an EXISTS, is planned as a semi join, and on the incident's shape (13
// ids, 56k versions each) it hashed every version of the concept for every
// batch: 738,621 rows hashed and 16.8 million rejected by the join filter to
// delete 500. The delete then joins back on the primary key (id, "createdAt").
//
// Measured on TimescaleDB 2.29.0 with autovacuum at its defaults, over the
// incident's shape -- 738,621 versions of 13 ids in two chunks, about 0.6 KB of
// payload per row (a production row is wider: it carries its concept schema) --
// drained back to back, which is harsher than one batch per 3 s tick. The
// machine was a shared 16-core laptop that was never idle, so each form ran
// twice: as the machine was (load average 11 to 37 from other work), and with 16
// busy loops added (load average 50 to 70 by the end). Time to drain the whole
// backlog in 1,472 batches, then a batch at the median, the 95th percentile and
// the worst, against the 7 s deadline:
//
//                 as the machine was                      plus 16 busy loops
//                 drain       median  p95     worst       drain        median  p95     worst
//   LATERAL probe 40 s        16 ms   66 ms   0.18 s      1 min 26 s   46 ms   0.14 s  0.31 s
//   EXISTS        5 min 48 s  0.15 s  0.61 s  1.9 s       13 min 9 s   0.39 s  1.8 s   4.5 s
//
// The first LATERAL batch took 20 ms (86 ms with the busy loops). In steady state
// a concept holds a few thousand versions and either form takes milliseconds; the
// difference is the backlog an upgrade finds, which is the one time it matters.
//
// # One statement per tick
//
// The sweep rides the leader's tick, the goroutine that also runs reconcile, so
// whatever it does to the tick it does to the reconcile cadence. That matters
// because reconcile retires a node on liveness only after it has found it absent
// for the liveness grace (defaultReconcileGrace, 20 s): it remembers the first
// pass that found the node missing, forgets it at any pass that finds the node
// present, and retires it at the first pass at least a grace after the first.
// A tick held for longer than the grace leaves a gap no pass looks into, so a
// node that went absent, came back and went absent again reads as continuously
// absent and is retired while it is serving. A sweep that looped for a time
// budget on the tick could do exactly that.
//
// So a tick carries at most ONE statement, which statusHistoryStatementTimeout
// holds well under the grace, and a round of the purge is spread over as many
// ticks as it takes. The concepts take turns, one statement each. A concept
// whose latest batch came back full (statusHistoryBatch deleted, so there may be
// more) is asked again on its next turn; one whose batch came back short is done
// for the round, and so is one whose batch failed. When every concept is done the
// round ends and the sweep rests until statusHistoryPurgeInterval has passed
// since the round began. The cutoff is fixed for the round.
//
// In steady state that is three or four statements in as many consecutive ticks,
// every ten minutes. A backlog drains one batch per tick -- the incident's in
// about an hour and a quarter at the default 3 s tick -- and no concept waits
// behind another's backlog.
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

	// statusHistoryPurgeInterval is how long the sweep rests between rounds,
	// counted from when the last round began. Ten minutes keeps the work in the
	// chunk that took the beats (see "Why it runs continuously"), and it keeps a
	// round small: in steady state it removes what the last ten minutes of beats
	// superseded, 40 versions per connected machine and 10 per node, which is a
	// batch or two.
	statusHistoryPurgeInterval = 10 * time.Minute

	// statusHistoryBatch is the most versions one statement deletes.
	statusHistoryBatch = 500

	// statusHistoryStatementTimeout is the deadline of each call, and so the most
	// one statement can hold the leader's tick. It is held below the reconciler's
	// liveness grace (see "One statement per tick").
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
// per-node purge does not: the lateral probe is what decides a version is
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
	//
	// "Has a newer version" is a LATERAL probe with LIMIT 1 and not an EXISTS,
	// on purpose. A subquery with a LIMIT cannot be pulled up into a join, so the
	// planner has to run it once per candidate as an index probe; an EXISTS is
	// planned as a semi join, and on the incident's shape (13 ids, 56k versions
	// each) it hashed every version of the concept for each batch. See "How every
	// statement stays bounded".
	res, err := p.db.ExecContext(ctx,
		`DELETE FROM "MemoryNodes" m
		  USING (
		    SELECT n.id, n."createdAt"
		      FROM "MemoryNodes" n
		      CROSS JOIN LATERAL (SELECT 1 AS found FROM "MemoryNodes" newer
		                           WHERE newer.concept = n.concept AND newer.id = n.id
		                             AND newer."createdAt" > n."createdAt" LIMIT 1) x
		     WHERE n.concept = ? AND n."createdAt" < ?
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

// statusHistoryRound is one round of the purge, spread over as many leader ticks
// as it takes: every concept is asked once, and a concept whose batch came back
// full is asked again, one statement per tick, the concepts taking turns.
type statusHistoryRound struct {
	active bool
	// startedAt is when the round began, and paces the next one.
	startedAt time.Time
	// olderThan is the cutoff, fixed for the whole round: a version that crosses
	// it while the round runs is simply the next round's.
	olderThan time.Time
	// pending says, by position in statusHistoryConcepts, which concepts still
	// have work: not asked yet this round, or latest batch came back full.
	pending []bool
	// next is where the search for the next concept starts, one past the concept
	// just asked, so that the concepts with work take turns.
	next int
	// purged is how many versions went this round, by concept, and statements
	// how many statements it took.
	purged     []int64
	statements int
}

func newStatusHistoryRound(now time.Time) statusHistoryRound {
	pending := make([]bool, len(statusHistoryConcepts))
	for i := range pending {
		pending[i] = true
	}
	return statusHistoryRound{
		active:    true,
		startedAt: now,
		olderThan: now.Add(-statusHistoryGrace),
		pending:   pending,
		purged:    make([]int64, len(statusHistoryConcepts)),
	}
}

// take answers which concept to ask next: the first one at or after the cursor
// that still has work. False when none has, which is the end of the round.
func (s *statusHistoryRound) take() (int, bool) {
	for k := range s.pending {
		i := (s.next + k) % len(s.pending)
		if s.pending[i] {
			return i, true
		}
	}
	return 0, false
}

// record notes what the statement for concept i came back with. A full batch
// leaves the concept pending, because there may be more; a short one, or a
// failure, ends it for the round. Either way the cursor moves past it, which is
// what makes two concepts with backlogs alternate.
func (s *statusHistoryRound) record(i int, versions int64, err error) {
	s.statements++
	s.next = (i + 1) % len(s.pending)
	s.purged[i] += versions
	s.pending[i] = err == nil && versions >= statusHistoryBatch
}

// sweepStatusHistory runs the continuous purge. Called from the leader's tick,
// and it issues at most one statement per call, of one concept (see "One
// statement per tick"). Between rounds it returns at once.
func (r *TopologyReconciler) sweepStatusHistory(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	now := r.now()
	round := &r.statusRound
	if !round.active && !r.lastStatusHistoryPurge.IsZero() && now.Sub(r.lastStatusHistoryPurge) < statusHistoryPurgeInterval {
		return
	}
	p := r.historyPurger()
	if p == nil {
		return
	}
	if !round.active {
		// Stamped when the round begins, as the readiness sweep stamps its
		// own: a round that fails is retried at the next interval, not on the
		// next tick three seconds later.
		r.lastStatusHistoryPurge = now
		*round = newStatusHistoryRound(now)
	}

	i, ok := round.take()
	if !ok {
		round.active = false
		return
	}
	n, err := r.purgeStatusBatch(ctx, p, statusHistoryConcepts[i], round.olderThan)
	round.record(i, n, err)
	if _, more := round.take(); !more {
		r.finishStatusHistoryRound()
	}
}

// finishStatusHistoryRound ends the round and, when it removed anything, says so
// once: the versions removed in all and per concept, and what it took.
func (r *TopologyReconciler) finishStatusHistoryRound() {
	round := &r.statusRound
	round.active = false
	var total int64
	var perConcept []any
	for i, versions := range round.purged {
		if versions > 0 {
			total += versions
			perConcept = append(perConcept, statusHistoryConcepts[i], versions)
		}
	}
	if total == 0 {
		return
	}
	elapsed := r.now().Sub(round.startedAt)
	attrs := []any{"versions", total, "statements", round.statements, "elapsed", elapsed.Round(time.Millisecond).String()}
	r.info("topology reconciler purged superseded status history", append(attrs, perConcept...)...)
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
