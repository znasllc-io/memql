package memql

// account_assignment.go -- THE ASSIGNMENT OF EXISTING ROWS TO ACCOUNTS (design
// record docs/superpowers/specs/2026-11-12-account-row-ownership-design.md,
// section F and refinement R10; the late commits, the full pass on request and
// the conflicts' candidates are the account export's, design record
// docs/superpowers/specs/2026-11-12-account-export-design.md, section F,
// "Caught up").
//
// Once the rollout switch is on, every new row of an engine-managed concept is
// stamped with its account as it is written (account_stamp.go). This file gives
// the rows written BEFORE that, and the rows a write path that bypasses the
// stamp still produces, their account. The rules a row is judged by are in
// account_assignment_rules.go; the statements that write it are in
// account_assignment_write.go. This file is the scheduling: which concept is
// worked on, how far, and what is remembered between calls.
//
// # What it touches, and what it does not
//
//   - Only ENGINE-MANAGED concepts (BelongsTo.EngineManaged): a `stated`
//     concept's own writers name its account, so a guess here would contradict
//     them, and a cluster-wide concept has no account at all.
//   - Only the NEWEST version of a row, in place, plus its copy in
//     memory_liveness when it has one. Older versions keep the account they
//     were written with. Rewriting every version would roughly double the
//     touched chunks until they are vacuumed (R10), and no reader takes an
//     account off an old version: a read takes a row at its newest. The one
//     exception is the list of accounts on the five kinds that carry one: on a
//     row that has an account, every version's list is held to that account
//     (trimAccountLists, account_assignment_write.go), because every version
//     travels with the row and none may carry another account's id. That is a
//     FULL pass's work, once per row, for the rows one array-shaped read per
//     batch finds holding a stray list; the newest version's list is held by
//     the stamp on every write.
//   - Mostly rows with NO account. The two rules that change an account a row
//     already has (account_assignment_rules.go: re-apply a move, follow) carry
//     rows a change elsewhere left behind; this is how followers follow a
//     parent whose account changed, and why a change asks for a full pass of
//     its follower kinds (account_follow.go). A full pass that finishes
//     settles the change records whose follower kinds have all been read since.
//   - It is raw SQL on the node table, so no version is appended, no event is
//     published and no automation runs. A row's assignment is not news.
//
// # One call, a bounded amount of work
//
// AccountOwnershipRunner calls runAccountAssignmentBatch every five seconds
// with a five-second deadline. A call walks at most assignmentBatchIds ids in
// all, one concept per transaction: claim the concept's progress row FOR UPDATE
// SKIP LOCKED, judge and write at most that many of its rows, and advance the
// cursor in the same transaction, so a crash or a lost deadline rolls back both
// and any replica resumes from the last committed batch (CompactSchemaBatch's
// shape). A call may go on to further concepts while the id budget and
// assignmentCallBudget allow, because most concepts have few rows and a call
// per concept would leave the once-a-minute later passes of a hundred concepts
// unable to keep up with one call per five seconds.
//
// A BATCH THAT CANNOT FINISH IN TIME MUST NOT BLOCK THE QUEUE. jsonb_set
// rewrites a row's whole payload, and for a concept whose payloads are large
// (a model call, an observation, a document version) 500 rows can be more
// TOAST than five seconds rewrite. Rolled back and claimed again first with the
// same 500 rows, such a concept would never advance, and every concept that
// waits on it would wait for good. So each concept keeps its own batch size
// (account_assignment_progress.batch_size, at most assignmentBatchIds): a
// batch that fails for any reason, a deadline or a statement timeout included,
// is recorded OUTSIDE the rolled-back transaction, under a fresh short context
// because the caller's is usually the thing that expired, by halving that
// size (floor 1) and moving the concept's next_at assignmentFailureDelay out,
// so the next call works on another concept. A batch that succeeds grows the
// size back by a quarter. The error still reaches the runner, which logs it
// and backs off as before.
//
// # Passes
//
//   - A FIRST pass walks every id of the concept in id order. A concept's first
//     pass is not started while a concept it takes its account from has not
//     finished its own (firstPassBlocked), so a goal is assigned before its runs
//     and a run before its steps, on every replica.
//   - A LATER pass walks only the versions written since verified_through, less
//     assignmentLateCommitSlack for a row whose writer set its createdAt a
//     little before its transaction began. It runs at most once a minute per
//     concept.
//   - verified_through is the START of the finished pass (never its finish,
//     which would skip a row written while the pass ran), or, when it is
//     earlier, the pass's HORIZON: the start of the oldest transaction any
//     batch of the pass saw open. Each batch reads that start from
//     pg_stat_activity before it reads a row (oldestOpenWriteStart: every node
//     connects as one role, so its sessions are every writer) and keeps the
//     earliest in account_assignment_progress.pass_horizon_at, which a pass
//     resets when it starts. A row inserted in a transaction that is open
//     while some batch reads past it is invisible to that batch; held back to
//     that transaction's start, the cursor is still behind the row once it
//     commits, and the next pass reads it -- whether the transaction is still
//     open at the pass's last batch or committed between two batches, and on a
//     full pass as on a later one. Every open transaction counts, one that has
//     not written yet included, except the account export's snapshot, which
//     names itself (AccountExportApplicationName) and writes nothing. If the
//     view cannot be read the batch fails and nothing advances. A pass that
//     finishes with its horizon more than assignmentHorizonWarnAge before its
//     start logs one WARN naming the oldest open session (logHorizonHeldBack).
//   - A later pass is a FULL pass again when a full pass was REQUESTED after the
//     last one began (RequestAccountFullPass, account_assignment_api.go: the
//     export asks for one of a kind with rows waiting, instead of waiting a day),
//     or when the concept's last full pass left rows WAITING on a parent
//     (`waiting` > 0) and began at least assignmentFullPassInterval ago. A
//     waiting row is not written again when its parent is assigned, so no later
//     pass would see it; a re-walk is what reaches it. A concept with nothing
//     waiting and no request never re-walks: walking every id of a table of tens
//     of millions of rows once a day to find nothing would be hours of reads a
//     day.
//   - `waiting` counts the rows that follow a parent with no account which can
//     still get one from the job (account_assignment_rules.go, rule 3), left by
//     the last full pass PLUS any found since; it is reset only when a full
//     pass starts, where the other counts reset every pass. A row following a
//     parent that will never get one from the job (of a stated or shared
//     concept with no account, in conflict, left out) is LEFT ALONE: counted in
//     `shared` with the other rows kept without an account on purpose, and it
//     brings no full pass by itself. So "nothing waiting" means nothing the
//     job will still assign is held up behind a parent.
//   - A requested full pass waits for the requested full pass of the concept it
//     takes its account from, as a first pass does (assignmentCandidates):
//     before it, a child would find its parent unassigned and be left waiting.
//   - The first batch of a later pass also judges again the rows of the
//     concept's open conflicts that name no candidate (conflictsWithoutCandidates),
//     so a conflict recorded before candidates existed gains them on the next
//     pass rather than at a full pass that may never come.
//
// WHAT THIS DOES NOT CATCH, accepted: a row whose writer set its createdAt more
// than assignmentLateCommitSlack before its transaction began, and that a pass
// read past before the transaction began: the horizon starts at the
// transaction, the slack covers the minute before it, and nothing records the
// time a writer took before it opened one. A writer that holds a timestamp for
// over a minute before beginning its transaction is a stalled process, and it
// is a write path that bypasses the stamp; the row stays unassigned until it
// is written again or a full pass of its concept, requested or due, reaches
// it. Also, the export's session is trusted to write nothing while it carries
// AccountExportApplicationName.
//
// Running it twice changes nothing: a row that has an account is kept, and the
// write itself is guarded on the row still having none.
//
// Tests: account_assignment_db_test.go.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lib/pq"
	"github.com/uptrace/bun"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/metrics"
)

const (
	// assignmentBatchIds bounds one call: the ids a call examines across all
	// the concepts it works on. 500 newest versions read, judged and written in
	// place fit well inside the request pool's 8 s statement timeout and the
	// runner's 5 s deadline, with room for the reads a judgment makes.
	assignmentBatchIds = 500
	// assignmentCallBudget stops a call from starting another concept once it
	// has run this long, so the last transaction it opens still has most of
	// the runner's deadline to finish in.
	assignmentCallBudget = 2 * time.Second
	// assignmentPassInterval is how soon a concept whose pass finished is
	// looked at again.
	assignmentPassInterval = time.Minute
	// assignmentFullPassInterval is how long after a full pass that left rows
	// waiting on a parent the concept is walked in full again. A concept with
	// nothing waiting is never walked in full again (file header, Passes).
	assignmentFullPassInterval = 24 * time.Hour
	// assignmentFailureDelay is how far a concept whose batch failed is pushed
	// back, so the next call works on another concept.
	assignmentFailureDelay = 30 * time.Second
	// assignmentFailureRecordTimeout bounds the write that records a failed
	// batch, made under a context of its own.
	assignmentFailureRecordTimeout = 2 * time.Second
)

// assignmentLateCommitSlack widens a later pass back past verified_through: a
// row's createdAt is set by its writer before the row is inserted, so a row
// stamped a little before its transaction began, or before the horizon was
// read, would otherwise fall just behind the cursor (file header, Passes). A
// variable only so a test can watch a late commit without holding a
// transaction open for longer than a minute; nothing else changes it. That is
// safe as a package variable because no test of this package runs the job
// beside the tests that change it: none of them calls t.Parallel, and the
// runner's tests drive tick synchronously rather than starting the runner
// (checked when the variable was introduced).
var assignmentLateCommitSlack = time.Minute

// assignmentHorizonWarnAge is how far behind its pass's start a horizon may be
// before the finishing batch logs the session holding it (logHorizonHeldBack).
// A variable only so a test need not hold a transaction open that long.
var assignmentHorizonWarnAge = 10 * time.Minute

// AccountExportApplicationName is the application_name the account export
// sets on its snapshot session, so the assignment's late-commit horizon does
// not count that long transaction (oldestOpenWriteStart). A session that names
// itself so must write no row of the node table while it does.
const AccountExportApplicationName = "memql-account-export"

// runAccountAssignmentBatch is one call of the job; see the file header. It
// returns nil when there is nothing due, and an error only for a transaction
// that failed, which the runner logs and backs off from.
func (e *MemQLEngine) runAccountAssignmentBatch(ctx context.Context) error {
	db := e.database()
	if db == nil {
		return nil
	}
	managed, order, err := assignmentConcepts()
	if err != nil || len(order) == 0 {
		return err
	}
	if err := ensureAssignmentProgressRows(ctx, db, order); err != nil {
		return err
	}
	candidates, err := assignmentCandidates(ctx, db, managed, order)
	if err != nil {
		return err
	}

	// The gauge of change records not settled, once per call, read through the
	// table's partial index; a failed read leaves the gauge as it was.
	defer func() {
		if n, err := AccountFollowPending(ctx, db.DB, ""); err == nil {
			metrics.SetAccountFollowTasks(n)
		}
	}()

	started := time.Now()
	budget := assignmentBatchIds
	for len(candidates) > 0 && budget > 0 && time.Since(started) < assignmentCallBudget && ctx.Err() == nil {
		result, err := e.assignOneBatch(ctx, db, candidates, budget)
		if err != nil {
			return err
		}
		if result.concept == "" {
			return nil // every candidate is held by another replica or not due
		}
		result.record()
		if result.changed {
			// After the commit, so a reader that misses the cache reads the
			// stamped row. Per batch rather than per pass: a pass over a large
			// concept takes hours, and its readers should not wait for the end.
			e.InvalidateCacheForConcept(result.concept)
		}
		budget -= result.examined
		candidates = removeCandidate(candidates, result.concept)
	}
	return nil
}

// assignmentConcepts returns every engine-managed concept by name and the order
// they are worked in (engineManagedOrder: a concept after the concept it takes
// its account from).
func assignmentConcepts() (map[string]*memorynodes.Concept, []string, error) {
	all := memorynodes.All()
	list := make([]*memorynodes.Concept, 0, len(all))
	for _, c := range all {
		list = append(list, c)
	}
	order, err := engineManagedOrder(list)
	if err != nil {
		return nil, nil, err
	}
	managed := make(map[string]*memorynodes.Concept, len(order))
	for _, name := range order {
		managed[name] = all[name]
	}
	return managed, order, nil
}

// ensureAssignmentProgressRows gives every engine-managed concept its progress
// row. A concept a later release or bundle declares gets one here and starts
// with a first pass; a row of a concept no longer declared is left alone, so
// its history stays readable and it is never claimed.
func ensureAssignmentProgressRows(ctx context.Context, db bun.IDB, order []string) error {
	_, err := db.ExecContext(ctx, `INSERT INTO account_assignment_progress (concept)
		SELECT unnest(?::text[]) ON CONFLICT (concept) DO NOTHING`, pq.Array(order))
	if err != nil {
		return fmt.Errorf("account assignment: recording the concepts to assign: %w", err)
	}
	return nil
}

// assignmentCandidates is the order filtered to the concepts a call may claim:
// those with no full pass owed, and those whose owed full pass may start or go
// on because every concept they take their account from has finished its own.
// A full pass is owed while the first pass has not finished, and while a full
// pass was requested after the last one began (fullPassRequested): a requested
// pass of a child that ran before its parent's would find the parent still
// unassigned and leave the child waiting, which is what the request was for.
// Whether a row is due (next_at) is left to the claim, which reads it under the
// lock.
func assignmentCandidates(ctx context.Context, db bun.IDB, managed map[string]*memorynodes.Concept, order []string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT concept FROM account_assignment_progress
		WHERE concept = ANY(?::text[])
		  AND (verified_through IS NULL
		       OR (full_pass_requested_at IS NOT NULL
		           AND (full_pass_at IS NULL OR full_pass_requested_at > full_pass_at)))`, pq.Array(order))
	if err != nil {
		return nil, fmt.Errorf("account assignment: reading progress: %w", err)
	}
	defer rows.Close()
	incomplete := map[string]bool{}
	for rows.Next() {
		var concept string
		if err := rows.Scan(&concept); err != nil {
			return nil, err
		}
		incomplete[concept] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(order))
	for _, name := range order {
		if incomplete[name] && firstPassBlocked(name, managed, incomplete) {
			continue
		}
		out = append(out, name)
	}
	return out, nil
}

// firstPassBlocked reports whether a concept's owed full pass, its first or a
// requested one, has to wait: some concept its rows take their account from
// still owes its own (`incomplete`, assignmentCandidates), so a child judged
// now would find its parent unassigned and be left waiting.
// engineManagedOrder already lists parents first; this is
// what keeps a second replica, which skips the parent a first replica holds,
// from starting the child meanwhile.
//
// A concept whose `via` resolves to no single concept (the id names it, as a
// Library index row's reference does) can follow a row of any concept, so it
// waits for every engine-managed concept that is not itself placed after
// everything -- the inherited flag engineManagedOrder sorts by. Waiting on a
// concept downstream of such a reference instead would deadlock: that concept
// waits on the reference's concept in turn.
func firstPassBlocked(name string, managed map[string]*memorynodes.Concept, incomplete map[string]bool) bool {
	seen := map[string]bool{}
	for current := name; !seen[current]; {
		seen[current] = true
		meta := managed[current]
		if meta == nil || strings.TrimSpace(meta.BelongsTo.Via) == "" {
			return false
		}
		parent := accountParentConcept(meta)
		if parent == "" {
			for other, m := range managed {
				if other != current && incomplete[other] && !assignmentAfterEverything(m, managed) {
					return true
				}
			}
			return false
		}
		if managed[parent] == nil {
			return false // a stated or undeclared parent is not assigned by this job
		}
		if incomplete[parent] {
			return true
		}
		current = parent
	}
	return false
}

// assignmentAfterEverything reports whether a concept, or an engine-managed
// concept up its `via` chain, follows a parent whose concept only the row's
// own reference names.
func assignmentAfterEverything(c *memorynodes.Concept, managed map[string]*memorynodes.Concept) bool {
	seen := map[string]bool{}
	for c != nil && !seen[c.Name] {
		seen[c.Name] = true
		if strings.TrimSpace(c.BelongsTo.Via) == "" {
			return false
		}
		parent := accountParentConcept(c)
		if parent == "" {
			return true
		}
		c = managed[parent]
	}
	return false
}

func removeCandidate(list []string, name string) []string {
	out := list[:0]
	for _, c := range list {
		if c != name {
			out = append(out, c)
		}
	}
	return out
}

// assignmentProgress is one concept's progress row, read under its lock.
type assignmentProgress struct {
	concept         string
	cursorID        sql.NullString
	cursorAt        sql.NullTime
	verifiedThrough sql.NullTime
	passFull        bool
	fullPassAt      sql.NullTime
	passStartedAt   sql.NullTime
	passFinishedAt  sql.NullTime
	// waiting is the rows left waiting by the last full pass plus any found
	// waiting since (file header, Passes).
	waiting int
	// fullPassRequestedAt is the last RequestAccountFullPass for the concept.
	fullPassRequestedAt sql.NullTime
	// passHorizon is the start of the oldest transaction any batch of the
	// current pass saw open (file header, Passes).
	passHorizon sql.NullTime
	// batchSize is this concept's own bound on a batch (file header, One call).
	batchSize int
}

// fullPassRequested reports whether a full pass was asked for after the last
// full pass began. A request made while a full pass is under way is newer than
// that pass's start, so the pass after it is full too: the rows the request was
// about may have been walked before they were ready.
func (p assignmentProgress) fullPassRequested() bool {
	return p.fullPassRequestedAt.Valid && (!p.fullPassAt.Valid || p.fullPassRequestedAt.Time.After(p.fullPassAt.Time))
}

// inPass reports whether a pass is under way: one has started and not finished
// since.
func (p assignmentProgress) inPass() bool {
	return p.passStartedAt.Valid && (!p.passFinishedAt.Valid || p.passFinishedAt.Time.Before(p.passStartedAt.Time))
}

// assignmentBatchResult is what one transaction did, for the metric and the
// cache.
type assignmentBatchResult struct {
	concept  string
	examined int
	changed  bool
	counts   assignmentCounts
}

// assignmentCounts is the judgments of one batch.
type assignmentCounts struct {
	assigned, kept, shared, conflicts, waiting int
}

func (r assignmentBatchResult) record() {
	metrics.AccountAssignmentRows(metrics.AccountAssignmentAssigned, r.counts.assigned)
	metrics.AccountAssignmentRows(metrics.AccountAssignmentKept, r.counts.kept)
	metrics.AccountAssignmentRows(metrics.AccountAssignmentShared, r.counts.shared)
	metrics.AccountAssignmentRows(metrics.AccountAssignmentConflict, r.counts.conflicts)
}

// assignOneBatch claims the first due candidate and works one batch of it, all
// in one transaction. A zero result (no concept) means nothing was claimable.
//
// A batch that fails after its concept was claimed is recorded by
// recordAssignmentFailure once the transaction has rolled back; the claimed
// concept is held outside the closure for that reason.
func (e *MemQLEngine) assignOneBatch(ctx context.Context, db *bun.DB, candidates []string, budget int) (assignmentBatchResult, error) {
	var result assignmentBatchResult
	claimed := ""
	err := db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		result = assignmentBatchResult{}
		var p assignmentProgress
		err := tx.QueryRowContext(ctx, `SELECT concept, cursor_id, cursor_at, verified_through, pass_full,
			       full_pass_at, pass_started_at, pass_finished_at, waiting, batch_size, full_pass_requested_at, pass_horizon_at
			  FROM account_assignment_progress
			 WHERE concept = ANY(?::text[]) AND next_at <= clock_timestamp()
			 ORDER BY array_position(?::text[], concept)
			 LIMIT 1 FOR UPDATE SKIP LOCKED`, pq.Array(candidates), pq.Array(candidates)).
			Scan(&p.concept, &p.cursorID, &p.cursorAt, &p.verifiedThrough, &p.passFull,
				&p.fullPassAt, &p.passStartedAt, &p.passFinishedAt, &p.waiting, &p.batchSize, &p.fullPassRequestedAt, &p.passHorizon)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("account assignment: claiming a concept: %w", err)
		}
		claimed = p.concept
		limit := budget
		if p.batchSize >= 1 && p.batchSize < limit {
			limit = p.batchSize
		}
		meta, err := memorynodes.Get(p.concept)
		if err != nil || meta == nil || !meta.BelongsTo.EngineManaged() {
			return fmt.Errorf("account assignment: concept %q is no longer engine-managed", p.concept)
		}

		var now time.Time
		if err := tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
			return err
		}
		// Read before the batch reads any row, so a transaction that commits
		// between the two is still counted as open (file header, Passes).
		horizon, err := oldestOpenWriteStart(ctx, tx)
		if err != nil {
			return err
		}
		if !p.inPass() {
			p = startAssignmentPass(p, now)
		}
		p.passHorizon = earlierTime(p.passHorizon, horizon)

		ids, nextID, nextAt, err := assignmentBatchIdsFor(ctx, tx, p, limit)
		if err != nil {
			return err
		}
		finished := len(ids) < limit
		if !p.passFull && p.passStartedAt.Time.Equal(now) {
			// After `finished` is decided: these rows are not the pass's own.
			unnamed, err := conflictsWithoutCandidates(ctx, tx, p.concept, limit)
			if err != nil {
				return err
			}
			ids = append(ids, unnamed...)
		}
		var counts assignmentCounts
		changed := false
		if len(ids) > 0 {
			// The judgment reads through the raw transaction (*sql.Tx): its
			// statements are written for an AccountQuerier, with $n
			// placeholders, so the export can ask the same judgment inside its
			// snapshot (account_assignment_rules.go).
			rows, err := readNewestVersions(ctx, tx.Tx, p.concept, distinctIds(ids))
			if err != nil {
				return err
			}
			plan, err := e.judgeAssignmentBatch(ctx, tx.Tx, meta, rows)
			if err != nil {
				return err
			}
			if fault := assignmentWriteFault; fault != nil {
				if err := fault(p.concept); err != nil {
					return err
				}
			}
			counts, changed, err = applyAssignmentPlan(ctx, tx, meta, plan, p.passFull)
			if err != nil {
				return err
			}
		}
		if err := advanceAssignmentProgress(ctx, tx, p, now, finished, nextID, nextAt, counts); err != nil {
			return err
		}
		if finished {
			e.logHorizonHeldBack(ctx, tx, p)
			if p.passFull {
				// A finished full pass may settle change records whose
				// followers it was the last kind to read (account_follow.go).
				if err := settleAccountChangeRecords(ctx, tx); err != nil {
					return err
				}
			}
		}
		result = assignmentBatchResult{concept: p.concept, examined: len(ids), changed: changed, counts: counts}
		return nil
	})
	if err != nil && claimed != "" {
		recordAssignmentFailure(ctx, db, claimed)
		err = fmt.Errorf("account assignment: a batch of %s failed and its batch size was halved: %w", claimed, err)
	}
	return result, err
}

// conflictsWithoutCandidates is the rows of open conflicts of the concept that
// name no candidate: conflicts recorded before candidates were (migration
// 20261112130000_account_export), or a fence refusal of a row the rules chose
// no account for. A later pass judges them again on its first batch, so their
// candidates are filled in on the job's next pass instead of at the next full
// pass, which a concept with nothing waiting never has. A conflict whose
// judgment now finds no candidate stays as it is and is asked about again at
// the next pass's start; there are few of them, and the read is bounded.
func conflictsWithoutCandidates(ctx context.Context, tx bun.Tx, concept string, limit int) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT row_id FROM account_assignment_conflict
		WHERE concept = ? AND resolved_at IS NULL AND left_out_at IS NULL AND cardinality(candidates) = 0
		ORDER BY row_id LIMIT ?`, concept, limit)
	if err != nil {
		return nil, fmt.Errorf("account assignment: reading the conflicts of %s without candidates: %w", concept, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// assignmentWriteFault, when set, is asked before a batch writes and fails the
// batch with what it returns. A seam for the test of a batch that cannot finish
// (account_assignment_db_test.go); nothing else sets it.
var assignmentWriteFault func(concept string) error

// recordAssignmentFailure halves the batch size of a concept whose batch just
// failed (floor 1) and pushes it assignmentFailureDelay back, so the next call
// works on another concept and the next attempt at this one is smaller (file
// header, One call). It runs after the rollback, under a context of its own:
// the caller's is usually the deadline that failed the batch. A failure to
// record is only logged by the caller's error path; the concept is then retried
// at its old size, which is where it would have been without this.
func recordAssignmentFailure(ctx context.Context, db *bun.DB, concept string) {
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), assignmentFailureRecordTimeout)
	defer cancel()
	_, _ = db.ExecContext(recordCtx, `UPDATE account_assignment_progress
		   SET batch_size = GREATEST(1, batch_size / 2), next_at = clock_timestamp() + ?::interval
		 WHERE concept = ?`, fmt.Sprintf("%d milliseconds", assignmentFailureDelay.Milliseconds()), concept)
}

// startAssignmentPass begins a pass. It is a full pass when the concept has
// never finished one, when a full pass was requested after the last one began,
// or when its last full pass left rows waiting on a parent and began at least
// assignmentFullPassInterval ago; otherwise a later pass over what was written
// since (file header, Passes).
func startAssignmentPass(p assignmentProgress, now time.Time) assignmentProgress {
	neverFull := !p.verifiedThrough.Valid || !p.fullPassAt.Valid
	waitingDue := p.waiting > 0 && p.fullPassAt.Valid && now.Sub(p.fullPassAt.Time) >= assignmentFullPassInterval
	p.passFull = neverFull || waitingDue || p.fullPassRequested()
	p.passStartedAt = sql.NullTime{Time: now, Valid: true}
	p.cursorID, p.cursorAt = sql.NullString{}, sql.NullTime{}
	p.passHorizon = sql.NullTime{}
	return p
}

// earlierTime is the earlier of two times, either of which may be absent.
func earlierTime(a, b sql.NullTime) sql.NullTime {
	if !a.Valid || (b.Valid && b.Time.Before(a.Time)) {
		return b
	}
	return a
}

// assignmentBatchIdsFor reads the next ids of the pass and where the cursor
// goes after them.
//
// A full pass walks distinct ids in id order. The order leads with the id, so
// TimescaleDB cannot plan it as an ordered append over the time column, which
// would scan whole chunks for a concept with few rows in them (the newest-
// version read's measured failure, versionsOfOneID).
//
// A later pass walks VERSIONS written since verified_through in ("createdAt",
// id) order: the concept's time index, and chunk exclusion, bound it to the
// recent chunks. An id with several recent versions comes back more than once;
// the batch judges it once.
//
// staged-data: MUST-NOT-GATE -- the job assigns every row of the concept,
// staged or not; a staged row is still a row of some account, and skipping it
// would leave it out of the account's inventory and export for good.
func assignmentBatchIdsFor(ctx context.Context, tx bun.Tx, p assignmentProgress, limit int) (ids []string, nextID sql.NullString, nextAt sql.NullTime, err error) {
	if p.passFull {
		rows, err := tx.QueryContext(ctx, `SELECT DISTINCT id FROM "MemoryNodes"
			WHERE concept = ? AND id > ? ORDER BY id LIMIT ?`, p.concept, p.cursorID.String, limit)
		if err != nil {
			return nil, nextID, nextAt, fmt.Errorf("account assignment: walking %s: %w", p.concept, err)
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return nil, nextID, nextAt, err
			}
			ids = append(ids, id)
		}
		if len(ids) > 0 {
			nextID = sql.NullString{String: ids[len(ids)-1], Valid: true}
		}
		return ids, nextID, nextAt, rows.Err()
	}

	since := p.verifiedThrough.Time.Add(-assignmentLateCommitSlack)
	afterAt, afterID := since, ""
	if p.cursorAt.Valid {
		afterAt, afterID = p.cursorAt.Time, p.cursorID.String
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, "createdAt" FROM "MemoryNodes"
		WHERE concept = ? AND "createdAt" > ? AND ("createdAt", id) > (?, ?)
		ORDER BY "createdAt", id LIMIT ?`, p.concept, since, afterAt, afterID, limit)
	if err != nil {
		return nil, nextID, nextAt, fmt.Errorf("account assignment: walking recent rows of %s: %w", p.concept, err)
	}
	defer rows.Close()
	var lastAt time.Time
	for rows.Next() {
		var id string
		if err := rows.Scan(&id, &lastAt); err != nil {
			return nil, nextID, nextAt, err
		}
		ids = append(ids, id)
	}
	if len(ids) > 0 {
		nextID = sql.NullString{String: ids[len(ids)-1], Valid: true}
		nextAt = sql.NullTime{Time: lastAt, Valid: true}
	}
	return ids, nextID, nextAt, rows.Err()
}

func distinctIds(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// oldestOpenWriteStart is the start of the oldest transaction still open on
// this database among this role's sessions, other than this one and the
// account export's snapshot; invalid when there is none. Every engine node
// connects as the one role, so this sees every writer of the node table.
//
// Every open transaction counts, not only one that has written already: a
// transaction that has not written yet may still insert a row whose createdAt
// it took earlier, and would then commit it behind the cursor. The one long
// transaction that never writes a row is the export's snapshot, open for up to
// an hour; counting it would hold every concept's cursor back for that hour,
// so it names itself (AccountExportApplicationName) and is left out. Another
// long reader costs re-reads while it is open, never a missed row.
//
// An error here fails the batch and nothing advances: a pass that could not
// tell whether a write is still open must not move verified_through past it,
// and holding the job is the safe side of not knowing.
func oldestOpenWriteStart(ctx context.Context, tx bun.Tx) (sql.NullTime, error) {
	var oldest sql.NullTime
	err := tx.QueryRowContext(ctx, `SELECT min(xact_start) FROM pg_stat_activity
		WHERE usename = current_user AND datname = current_database() AND pid <> pg_backend_pid()
		  AND application_name <> ? AND xact_start IS NOT NULL`, AccountExportApplicationName).Scan(&oldest)
	if err != nil {
		return oldest, fmt.Errorf("account assignment: reading the open transactions: %w", err)
	}
	return oldest, nil
}

// logHorizonHeldBack logs one WARN when the pass that just finished had its
// horizon more than assignmentHorizonWarnAge before its start: some session has
// held a transaction open that long, every later pass of every concept re-reads
// everything written since it began, and an operator wants to know which
// session. It names the session that is oldest NOW (the one that held the
// horizon may have finished since), at most one line per pass, and no metric.
// A failure to read the view is not the batch's failure; it is only logged.
func (e *MemQLEngine) logHorizonHeldBack(ctx context.Context, tx bun.Tx, p assignmentProgress) {
	if !p.passHorizon.Valid || p.passStartedAt.Time.Sub(p.passHorizon.Time) <= assignmentHorizonWarnAge || e.Component == nil || e.Logger == nil {
		return
	}
	var pid sql.NullInt64
	var app sql.NullString
	var started sql.NullTime
	err := tx.QueryRowContext(ctx, `SELECT pid, application_name, xact_start FROM pg_stat_activity
		WHERE usename = current_user AND datname = current_database() AND pid <> pg_backend_pid()
		  AND application_name <> ? AND xact_start IS NOT NULL
		ORDER BY xact_start LIMIT 1`, AccountExportApplicationName).Scan(&pid, &app, &started)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		e.Logger.Warn("account assignment: a transaction held the pass's horizon back, and the session could not be read",
			"concept", p.concept, "horizon", p.passHorizon.Time, "passStarted", p.passStartedAt.Time, "error", err)
		return
	}
	e.Logger.Warn("account assignment: a long-open transaction is holding the late-commit horizon back; every later pass re-reads what was written since it began",
		"concept", p.concept, "horizon", p.passHorizon.Time, "passStarted", p.passStartedAt.Time,
		"pid", pid.Int64, "application_name", app.String, "xactStart", started.Time)
}

// advanceAssignmentProgress writes the cursor, the pass's horizon and the
// batch's counts, and on the last batch of a pass closes it: verified_through
// becomes the pass's START, or the pass's horizon if that is earlier (file
// header, Passes), the cursor and the horizon are cleared, and the concept is
// next due a minute later.
func advanceAssignmentProgress(ctx context.Context, tx bun.Tx, p assignmentProgress, now time.Time, finished bool,
	nextID sql.NullString, nextAt sql.NullTime, c assignmentCounts) error {
	// A batch that starts the pass resets the counts, so the row reads as the
	// current pass -- except `waiting`, which only a FULL pass resets: it is
	// what decides whether the concept is walked in full again
	// (startAssignmentPass), and a later pass sees only recent rows, so its own
	// count would forget the rows still waiting from the last full pass.
	fresh := p.passStartedAt.Time.Equal(now)
	freshWaiting := fresh && p.passFull
	cursorID, cursorAt := p.cursorID, p.cursorAt
	if nextID.Valid {
		cursorID, cursorAt = nextID, nextAt
	}
	verified, fullAt, finishedAt, passHorizon := p.verifiedThrough, p.fullPassAt, p.passFinishedAt, p.passHorizon
	nextDue := now
	if finished {
		verified = earlierTime(p.passStartedAt, p.passHorizon)
		passHorizon = sql.NullTime{}
		if p.passFull {
			fullAt = p.passStartedAt
		}
		finishedAt = sql.NullTime{Time: now, Valid: true}
		cursorID, cursorAt = sql.NullString{}, sql.NullTime{}
		nextDue = now.Add(assignmentPassInterval)
	}
	_, err := tx.ExecContext(ctx, `UPDATE account_assignment_progress SET
		    cursor_id = ?, cursor_at = ?, verified_through = ?, pass_full = ?, full_pass_at = ?,
		    pass_started_at = ?, pass_finished_at = ?, next_at = ?, pass_horizon_at = ?,
		    assigned  = CASE WHEN ? THEN 0 ELSE assigned END + ?,
		    kept      = CASE WHEN ? THEN 0 ELSE kept END + ?,
		    shared    = CASE WHEN ? THEN 0 ELSE shared END + ?,
		    conflicts = CASE WHEN ? THEN 0 ELSE conflicts END + ?,
		    waiting   = CASE WHEN ? THEN 0 ELSE waiting END + ?,
		    batch_size = LEAST(?, batch_size + GREATEST(1, batch_size / 4))
		  WHERE concept = ?`,
		cursorID, cursorAt, verified, p.passFull, fullAt, p.passStartedAt, finishedAt, nextDue, passHorizon,
		fresh, c.assigned, fresh, c.kept, fresh, c.shared, fresh, c.conflicts, freshWaiting, c.waiting,
		assignmentBatchIds, p.concept)
	if err != nil {
		return fmt.Errorf("account assignment: advancing %s: %w", p.concept, err)
	}
	return nil
}
