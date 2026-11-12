package memql

// account_follow.go -- FOLLOWERS FOLLOW (design record
// docs/superpowers/specs/2026-11-12-account-export-design.md, section I,
// "Followers follow").
//
// A row whose `via` field names another row belongs to that row's account (the
// stamp's source 4, the assignment's rule 3). Until this file nothing carried
// those rows when the parent's account CHANGED: a goal moved and its runs stayed
// behind. Now every write that changes a row's recorded account records a
// FOLLOW TASK (EnqueueAccountFollow): the governed move (account_move.go), a
// re-tag of a row that carries the list of accounts, and a `stated` kind's own
// mutation (executeWrite). AccountOwnershipRunner works the tasks off after its
// assignment batch, one bounded batch per tick.
//
// # Who follows
//
// A FOLLOWER of row R is a row of an engine-managed kind whose `via` field names
// R and whose own account, decided by its newest version (newestAccountSQL,
// account_rows.go), is the account R LEFT. A row attached to R from another
// account is not a follower and stays where it is: row ownership allows a row
// to point at a parent of another account on purpose, so what marks a follower
// is the account it shares with its parent, not the link alone. A follower that
// has followers of its own records a task in turn, so a goal carries its runs,
// and each run its steps, one generation per task.
//
// The kinds that may follow R are those whose `via` resolves to R's concept,
// and those whose `via` resolves to no single concept (a Library index row's
// reference names its target's concept in the id), in name order.
//
// # How a task reads, and why so
//
// The row table is a hypertable whose old chunks are compressed by concept, so
// a compressed chunk has no id index and a probe per row would read whole
// chunks. So a task never probes row by row:
//
//   - When R left an account, the candidates come from the account index,
//     idx_memorynodes_account, for (the account R left, the follower kind),
//     filtered by the via field, at most accountFollowBatchRows ids after the
//     task's cursor. The observation store is asked the same.
//   - When R had no account (a placement), the index has nothing to offer:
//     "names no account" is what it leaves out. The task then walks the
//     follower kind's ids in order, a bounded batch at a time, which costs a
//     read of the kind once per placement and is why placements are rare acts.
//
// Each candidate is decided by its newest version: its account through
// newestAccountSQL, the one rule every account reader uses, and its via field
// through readNewestVersions, the shape every newest-version read takes. Then
// it is written in place by the assignment's own statement (writeAssignments),
// guarded on the account it was read with, so a row that changed since is left
// alone: no version is appended, no event is published, no automation runs, and
// the kind's cached reads are invalidated after the commit.
//
// # The batch
//
// One call claims the oldest due task FOR UPDATE SKIP LOCKED, works at most
// accountFollowBatchRows candidate ids of it, and advances its cursor
// (cursor_concept, cursor_id) in the same transaction, so a crash rolls both
// back and any replica resumes from the last committed batch. A task is done
// when every follower kind has been walked to its end. A call goes on to further
// tasks while accountFollowCallBudget allows. A Library row the retirement fence
// refuses is left in the account it was in and logged; nothing else can move
// it either.
//
// Tests: account_follow_db_test.go.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/lib/pq"
	"github.com/uptrace/bun"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/metrics"
)

// AccountRowRef names one row: its concept and its id as stored.
type AccountRowRef struct{ Concept, ID string }

// accountFollowBatchRows bounds one batch: the candidate ids a call examines
// for one task. A variable only so a test can cut a task into batches of one;
// nothing else changes it.
var accountFollowBatchRows = 200

// accountFollowCallBudget stops a call from starting another batch once it has
// run this long, as assignmentCallBudget does for the assignment. Shorter,
// because the call runs after the assignment batch in the same tick deadline
// (AccountOwnershipRunner.tick).
const accountFollowCallBudget = time.Second

// EnqueueAccountFollow records that the row's account changed from `from` ("" =
// none) to `to`. tx is the write's own transaction where there is one, so the
// change and the obligation to carry its followers commit together. Nothing is
// recorded when the account did not change, when the row was left with none
// (nothing to give its followers), or when no kind can follow the row's.
func EnqueueAccountFollow(ctx context.Context, tx bun.IDB, row AccountRowRef, from, to string) error {
	from, to = bareAccount(from), bareAccount(to)
	if from == to || to == "" || row.Concept == "" || strings.TrimSpace(row.ID) == "" {
		return nil
	}
	if len(followerKinds(row.Concept)) == 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO account_follow_tasks (parent_concept, parent_id, from_account, to_account)
		VALUES (?, ?, ?, ?)`, row.Concept, row.ID, from, to); err != nil {
		return fmt.Errorf("account follow: recording the followers of %s: %w", row.ID, err)
	}
	return nil
}

// AccountFollowPending counts tasks not done that name the account as from or
// to; "" counts all.
func AccountFollowPending(ctx context.Context, q AccountQuerier, account string) (int64, error) {
	bare := bareAccount(account)
	rows, err := q.QueryContext(ctx, `SELECT count(*) FROM account_follow_tasks
		WHERE done_at IS NULL AND ($1 = '' OR from_account = $1 OR to_account = $1)`, bare)
	if err != nil {
		return 0, fmt.Errorf("account follow: counting pending tasks: %w", err)
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

// followerKinds is the engine-managed kinds whose rows may follow a row of the
// concept, in name order (file header, Who follows).
func followerKinds(parent string) []*memorynodes.Concept {
	var out []*memorynodes.Concept
	for _, c := range memorynodes.All() {
		if !c.AccountEngineManaged() || strings.TrimSpace(c.BelongsTo.Via) == "" {
			continue
		}
		if p := accountParentConcept(c); p == parent || p == "" {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// followTask is one row of account_follow_tasks, read under its lock.
type followTask struct {
	id                     int64
	parentConcept          string
	parentID               string
	from, to               string
	cursorConcept, cursorID string
}

// runAccountFollowBatch is one call of the follow tasks; see the file header.
// It returns nil when nothing is due.
func (e *MemQLEngine) runAccountFollowBatch(ctx context.Context) error {
	db := e.database()
	if db == nil {
		return nil
	}
	started := time.Now()
	for time.Since(started) < accountFollowCallBudget && ctx.Err() == nil {
		worked, changed, err := e.followOneBatch(ctx, db)
		if err != nil {
			return err
		}
		for concept := range changed {
			e.InvalidateCacheForConcept(concept)
		}
		if !worked {
			break
		}
	}
	pending, err := AccountFollowPending(ctx, db.DB, "")
	if err == nil {
		metrics.SetAccountFollowTasks(pending)
	}
	return nil
}

// followOneBatch claims one due task and works one batch of it. worked is
// false when nothing was claimable; changed is the kinds whose rows moved.
func (e *MemQLEngine) followOneBatch(ctx context.Context, db *bun.DB) (bool, map[string]bool, error) {
	worked := false
	changed := map[string]bool{}
	err := db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		worked = false
		clear(changed)
		var t followTask
		err := tx.QueryRowContext(ctx, `SELECT id, parent_concept, parent_id, from_account, to_account, cursor_concept, cursor_id
			  FROM account_follow_tasks
			 WHERE done_at IS NULL AND next_at <= clock_timestamp()
			 ORDER BY next_at, id LIMIT 1 FOR UPDATE SKIP LOCKED`).
			Scan(&t.id, &t.parentConcept, &t.parentID, &t.from, &t.to, &t.cursorConcept, &t.cursorID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("account follow: claiming a task: %w", err)
		}
		worked = true

		budget := accountFollowBatchRows
		followed := 0
		done := true
		cursorConcept, cursorID := t.cursorConcept, t.cursorID
		for _, kind := range followerKinds(t.parentConcept) {
			if kind.Name < cursorConcept {
				continue
			}
			if kind.Name != cursorConcept {
				cursorID = ""
			}
			cursorConcept = kind.Name
			if budget <= 0 {
				done = false
				break
			}
			scanned, moved, err := e.followKindBatch(ctx, tx, t, kind, cursorID, budget)
			if err != nil {
				return err
			}
			followed += moved
			if moved > 0 {
				changed[kind.Name] = true
			}
			if len(scanned) == budget {
				// The batch filled on this kind: more of it may be left.
				cursorID = scanned[len(scanned)-1]
				done = false
				break
			}
			budget -= len(scanned)
			cursorID = ""
		}
		if done {
			_, err = tx.ExecContext(ctx, `UPDATE account_follow_tasks
				   SET cursor_concept = '', cursor_id = '', followed = followed + ?, done_at = clock_timestamp()
				 WHERE id = ?`, followed, t.id)
		} else {
			_, err = tx.ExecContext(ctx, `UPDATE account_follow_tasks
				   SET cursor_concept = ?, cursor_id = ?, followed = followed + ?, next_at = clock_timestamp()
				 WHERE id = ?`, cursorConcept, cursorID, followed, t.id)
		}
		if err != nil {
			return fmt.Errorf("account follow: advancing task %d: %w", t.id, err)
		}
		return nil
	})
	if err != nil {
		return false, nil, err
	}
	return worked, changed, nil
}

// followKindBatch reads at most limit candidate ids of one follower kind after
// afterID, moves those that follow, and records a task for each moved row that
// has followers of its own. scanned is every candidate id read, in order, for
// the cursor; moved is how many rows took the new account.
func (e *MemQLEngine) followKindBatch(ctx context.Context, tx bun.Tx, t followTask, kind *memorynodes.Concept,
	afterID string, limit int) (scanned []string, moved int, err error) {
	rows, scanned, err := followersOf(ctx, tx.Tx, AccountRowRef{Concept: t.parentConcept, ID: t.parentID}, t.from, kind, afterID, limit)
	if err != nil || len(rows) == 0 {
		return scanned, 0, err
	}
	account := accountValueFor(kind, t.to)
	writes := make([]assignmentWrite, 0, len(rows))
	for _, row := range rows {
		writes = append(writes, assignmentWrite{ID: row.ID, At: versionKey(row.At), Account: account, From: t.from})
	}
	written, err := writeInPlaceAroundFence(ctx, tx, kind.Name, writes)
	if err != nil {
		return scanned, 0, err
	}
	for _, w := range written.refused {
		e.Logger.Warn("account follow: the Library retirement fence refused a follower; it stays in the account it was in",
			"concept", kind.Name, "id", w.ID, "account", t.from)
	}
	grandchildren := len(followerKinds(kind.Name)) > 0
	for _, id := range written.stamped {
		if grandchildren {
			if err := EnqueueAccountFollow(ctx, tx, AccountRowRef{Concept: kind.Name, ID: id}, t.from, t.to); err != nil {
				return scanned, 0, err
			}
		}
	}
	return scanned, len(written.stamped), nil
}

// accountValueFor is how a kind stores an account: a `stated` kind's own
// writers store the canonical id (validateOrganizationOwnership), and the
// engine stores the bare id everywhere else (account_stamp.go, Spelling).
func accountValueFor(kind *memorynodes.Concept, bare string) string {
	if kind.BelongsTo.Stated {
		return accountCanonicalPrefix + bare
	}
	return bare
}

// followCandidatesFromIndexSQL is the ids of a follower kind naming the parent
// in its via field ($4) on a version that names the account the parent left,
// through the account index, and the observation store asked the same.
//
// staged-data: MUST-NOT-GATE -- carrying a row to the account its parent moved to; a staged follower is the account's all the same.
const followCandidatesFromIndexSQL = `SELECT id FROM (
    SELECT id FROM "MemoryNodes"
     WHERE ` + accountNamedSQL + `
       AND (payload->>'accountId') IN ($1, $2) AND concept = $3
       AND (payload->>$4) = ANY($5::text[]) AND id > $6
    UNION
    SELECT id FROM memory_liveness
     WHERE concept = $3 AND (payload->>'accountId') IN ($1, $2)
       AND (payload->>$4) = ANY($5::text[]) AND id > $6
  ) k ORDER BY id LIMIT $7`

// followCandidatesWalkSQL is the next ids of a follower kind in id order, for a
// parent that had no account (file header, How a task reads).
//
// staged-data: MUST-NOT-GATE -- the followCandidatesFromIndexSQL reason.
const followCandidatesWalkSQL = `SELECT DISTINCT id FROM "MemoryNodes"
 WHERE concept = $1 AND id > $2 ORDER BY id LIMIT $3`

func followCandidates(ctx context.Context, q AccountQuerier, concept, via string, parent []string, from, afterID string, limit int) ([]string, error) {
	if from == "" {
		return scanIDs(ctx, q, followCandidatesWalkSQL, concept, afterID, limit)
	}
	return scanIDs(ctx, q, followCandidatesFromIndexSQL, from, accountCanonicalPrefix+from, concept, via, pq.Array(parent), afterID, limit)
}

// idsWithNewestAccountSQL keeps the ids whose newest version names the account
// (either spelling), or names none when the account is "".
//
// staged-data: MUST-NOT-GATE -- deciding whose a row is (newestAccountSQL).
const idsWithNewestAccountSQL = `SELECT c.id FROM (SELECT $1::text AS concept, unnest($2::text[]) AS id) c
 WHERE COALESCE(` + newestAccountSQL + `, '') IN ($3, $4)
 ORDER BY c.id`

func idsWithNewestAccount(ctx context.Context, q AccountQuerier, concept string, ids []string, account string) ([]string, error) {
	canonical := ""
	if account != "" {
		canonical = accountCanonicalPrefix + account
	}
	return scanIDs(ctx, q, idsWithNewestAccountSQL, concept, pq.Array(ids), account, canonical)
}

// accountFollowChange is a write's change of a row's recorded account, to be
// recorded in the write's transaction (executeWrite).
type accountFollowChange struct {
	concept, from, to string
}

// accountFollowFor is the follow task a write owes, or nil: recording is on,
// the row existed, its kind is account-scoped, and its account changes to one
// (bare compare, so a respelling is no change) that a kind can follow.
func accountFollowFor(e *MemQLEngine, meta *memorynodes.Concept, prior, payload map[string]any) *accountFollowChange {
	if !e.AccountStampingEnabled() || prior == nil || !meta.AccountScoped() {
		return nil
	}
	from, to := bareAccount(prior[accountField]), bareAccount(payload[accountField])
	if from == to || to == "" || len(followerKinds(meta.Name)) == 0 {
		return nil
	}
	return &accountFollowChange{concept: meta.Name, from: from, to: to}
}

// enqueue records the task through the write transaction's store.
func (c *accountFollowChange) enqueue(ctx context.Context, store memorynodes.Store, id string) error {
	bs, ok := store.(*bunStore)
	if !ok {
		return fmt.Errorf("account follow: the write's store is not the database's")
	}
	return EnqueueAccountFollow(ctx, bs.db, AccountRowRef{Concept: c.concept, ID: id}, c.from, c.to)
}
