package memql

// account_assignment_api.go -- what the account export asks of the assignment
// of existing rows (account_assignment.go), read-only except for the request
// for a full pass (design record
// docs/superpowers/specs/2026-11-12-account-export-design.md, section F,
// "Caught up").

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/lib/pq"
	"github.com/uptrace/bun"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// ConflictsNaming counts open conflicts whose candidates include the account.
// A conflict an owner left out (left_out_at) is a decision, not an open
// question, and holds no export back.
func ConflictsNaming(ctx context.Context, q AccountQuerier, account string) (int64, error) {
	rows, err := q.QueryContext(ctx, `SELECT count(*) FROM account_assignment_conflict
		WHERE resolved_at IS NULL AND left_out_at IS NULL AND $1 = ANY(candidates)`, bareAccount(account))
	if err != nil {
		return 0, fmt.Errorf("account assignment: counting the conflicts naming an account: %w", err)
	}
	defer rows.Close()
	var n int64
	if rows.Next() {
		if err := rows.Scan(&n); err != nil {
			return 0, err
		}
	}
	return n, rows.Err()
}

// RequestAccountFullPass asks the job for a full pass of each concept at once.
// The request is honored by the concept's next pass, or by the pass after the
// one under way, which is then full whatever the age of the last full pass
// (startAssignmentPass); the concept is made due now. A concept with no
// progress row yet is left alone: its first pass is a full pass already.
func RequestAccountFullPass(ctx context.Context, db *bun.DB, concepts []string) error {
	if len(concepts) == 0 {
		return nil
	}
	_, err := db.ExecContext(ctx, `UPDATE account_assignment_progress
		   SET full_pass_requested_at = clock_timestamp(), next_at = LEAST(next_at, clock_timestamp())
		 WHERE concept = ANY(?::text[])`, pq.Array(concepts))
	if err != nil {
		return fmt.Errorf("account assignment: requesting a full pass: %w", err)
	}
	return nil
}

// AccountAssignmentState is where the job stands on one concept.
type AccountAssignmentState struct {
	Concept           string
	FirstPassFinished bool
	VerifiedThrough   time.Time // zero until the first pass finishes
	Waiting           int64
}

// AccountAssignmentStates has one line per engine-managed concept of the
// registry, in assignment order. A concept with no progress row yet reads
// FirstPassFinished false. VerifiedThrough is the point before which every
// row's newest version was judged (account_assignment.go, Passes); Waiting is
// the rows left waiting on a parent by the last full pass plus any found since.
func (e *MemQLEngine) AccountAssignmentStates(ctx context.Context, q AccountQuerier) ([]AccountAssignmentState, error) {
	_, order, err := assignmentConcepts()
	if err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, `SELECT concept, verified_through, waiting FROM account_assignment_progress
		WHERE concept = ANY($1::text[])`, pq.Array(order))
	if err != nil {
		return nil, fmt.Errorf("account assignment: reading progress: %w", err)
	}
	defer rows.Close()
	byConcept := map[string]AccountAssignmentState{}
	for rows.Next() {
		var st AccountAssignmentState
		var verified sql.NullTime
		if err := rows.Scan(&st.Concept, &verified, &st.Waiting); err != nil {
			return nil, err
		}
		st.FirstPassFinished, st.VerifiedThrough = verified.Valid, verified.Time
		byConcept[st.Concept] = st
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]AccountAssignmentState, 0, len(order))
	for _, name := range order {
		st, ok := byConcept[name]
		if !ok {
			st = AccountAssignmentState{Concept: name}
		}
		out = append(out, st)
	}
	return out, nil
}

// assignableJudgeChunk bounds how many rows one judgment of the export's judge
// takes, as assignmentBatchIds bounds the job's: the parents and conflicts it
// reads are read per chunk.
const assignableJudgeChunk = assignmentBatchIds

// assignableParentDepth bounds the walk up a chain of waiting parents. `via`
// chains are acyclic (belongsToViaGraph) and a few deep; a chain longer than
// this is answered "cannot", which leaves the row uncounted rather than looping.
const assignableParentDepth = 16

// AccountRowsStillAssignable judges, without writing, the rows of the given
// concepts whose newest version was written at or after `since` and names no
// account. It counts, per concept, the rows the job's rules would assign, or
// would leave waiting on a parent that can still get an account. Rows the job
// leaves alone on purpose are not counted: the cluster's own, rows following a
// parent that states no account, recorded conflicts. Only concepts with a
// count are in the map, so an empty map means nothing is waiting.
//
// The judgment is the job's own (judgeAssignmentRows), so the two cannot
// disagree about a row. It reads through q, so the export can ask inside its
// snapshot. A concept that is not engine-managed is skipped: the job never
// assigns its rows, so none of them is waiting for it.
func (e *MemQLEngine) AccountRowsStillAssignable(ctx context.Context, q AccountQuerier, since time.Time, concepts []string) (map[string]int64, error) {
	out := map[string]int64{}
	prospects := map[string]bool{}
	for _, name := range concepts {
		meta, err := memorynodes.Get(name)
		if err != nil || meta == nil || !meta.AccountEngineManaged() {
			continue
		}
		rows, err := recentRowsWithoutAccount(ctx, q, name, since)
		if err != nil {
			return nil, err
		}
		var n int64
		for start := 0; start < len(rows); start += assignableJudgeChunk {
			chunk := rows[start:min(start+assignableJudgeChunk, len(rows))]
			lk, plan, err := e.judgeAssignmentRows(ctx, q, meta, chunk)
			if err != nil {
				return nil, err
			}
			for _, d := range plan {
				if lk.conflicts[d.row.ID].open {
					continue // a recorded conflict waits for a person, not for the job
				}
				switch d.outcome {
				case outcomeAssigned:
					n++
				case outcomeWaiting:
					parent, parentID := assignmentParent(meta, d.row)
					ok, err := e.parentCanStillGetAccount(ctx, q, parent, parentID, 0, prospects)
					if err != nil {
						return nil, err
					}
					if ok {
						n++
					}
				}
			}
		}
		if n > 0 {
			out[name] = n
		}
	}
	return out, nil
}

// recentRowsWithoutAccount is the newest version of every row of the concept
// written at or after since, when that version names no account. Among the
// versions since, the newest is the row's newest overall, so DISTINCT ON over
// them alone is enough; the account is tested after it, or an older version
// without one would stand in for a newer one that has one. A Shopify channel's
// rows are all returned, because its `accountId` may hold Shopify's own value,
// which the judgment reads as no account.
//
// staged-data: MUST-NOT-GATE -- the job assigns staged rows too
// (assignmentBatchIdsFor), so a staged row without an account is waiting like
// any other.
func recentRowsWithoutAccount(ctx context.Context, q AccountQuerier, concept string, since time.Time) ([]assignmentRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, "createdAt", payload FROM (
		    SELECT DISTINCT ON (id) id, "createdAt", payload FROM "MemoryNodes"
		     WHERE concept = $1 AND "createdAt" >= $2 ORDER BY id, "createdAt" DESC) n
		 WHERE $3 OR COALESCE(n.payload->>'accountId', '') = ''`, concept, since, concept == conceptShopifyChannel)
	if err != nil {
		return nil, fmt.Errorf("account assignment: reading the recent rows of %s: %w", concept, err)
	}
	return scanAssignmentRows(rows)
}

// parentCanStillGetAccount reports whether the row a waiting row follows will
// get an account from the job: its concept is engine-managed, it has no
// recorded conflict, and the rules would assign it now, or it waits in turn on
// a parent that can. A parent of a stated or shared concept with no account,
// one in conflict or left out, or one gone, leaves its follower waiting for a
// person, which is not what "caught up" waits for. Answers are memoized by
// concept and bare id for the call.
//
// staged-data: MUST-NOT-GATE -- a parent's prospects are a fact about the
// parent, staged or not (loadParents).
func (e *MemQLEngine) parentCanStillGetAccount(ctx context.Context, q AccountQuerier, concept, bareID string, depth int, memo map[string]bool) (bool, error) {
	if concept == "" || depth >= assignableParentDepth {
		return false, nil
	}
	key := concept + "\x00" + bareID
	if answer, done := memo[key]; done {
		return answer, nil
	}
	answer, err := e.judgeParentProspect(ctx, q, concept, bareID, depth, memo)
	if err != nil {
		return false, err
	}
	memo[key] = answer
	return answer, nil
}

// judgeParentProspect is parentCanStillGetAccount's judgment of one parent.
func (e *MemQLEngine) judgeParentProspect(ctx context.Context, q AccountQuerier, concept, bareID string, depth int, memo map[string]bool) (bool, error) {
	meta, err := memorynodes.Get(concept)
	if err != nil || meta == nil || !meta.AccountEngineManaged() {
		return false, nil
	}
	// Both spellings, as loadParents asks; the newest of them is the row.
	rows, err := readNewestVersions(ctx, q, concept, []string{concept + ":" + bareID, bareID})
	if err != nil || len(rows) == 0 {
		return false, err
	}
	row := rows[0]
	for _, r := range rows[1:] {
		if r.At.After(row.At) {
			row = r
		}
	}
	lk, plan, err := e.judgeAssignmentRows(ctx, q, meta, []assignmentRow{row})
	if err != nil || len(plan) == 0 {
		return false, err
	}
	if lk.conflicts[row.ID].open {
		return false, nil
	}
	switch plan[0].outcome {
	case outcomeAssigned, outcomeKept:
		return true, nil
	case outcomeWaiting:
		parent, parentID := assignmentParent(meta, row)
		return e.parentCanStillGetAccount(ctx, q, parent, parentID, depth+1, memo)
	}
	return false, nil
}
