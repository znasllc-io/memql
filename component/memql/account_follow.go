package memql

// account_follow.go -- FOLLOWERS FOLLOW: the change records, and what a write
// that changes a row's account asks of the assignment job (design record
// docs/superpowers/specs/2026-11-12-account-export-design.md, section I,
// "Followers follow").
//
// A row whose `via` field names another row belongs to that row's account (the
// stamp's source 4, the assignment's rule 3). When the parent's account
// CHANGES, its followers -- the rows naming it, in the account it left -- have
// to take the new one. Nothing here moves them. The assignment job does, by two
// rules of its own (account_assignment_rules.go, "Follow" and "Re-apply a
// move"), because it already walks each kind by id in bounded batches and reads
// each batch's parents by id arrays, which is the one shape of read that stays
// cheap on the row table (a hypertable whose old chunks are compressed, and a
// compressed chunk has no id index). This file holds the record those rules
// read, and the decisions about what to ask of the job.
//
// # Change records
//
// account_follow_tasks holds one row per row whose recorded account CHANGED
// from one account to another: parent_concept, parent_id, from_account,
// to_account, created_at. Its cursor columns (cursor_concept, cursor_id,
// followed, next_at) are unused: they belonged to a follow runner that walked
// follower kinds itself, and that is gone. A record is written
//
//   - by the governed move, for each root it moves from an account to another
//     (account_move.go; a placement, from no account, writes none);
//   - by the write path (executeWrite, through accountChangeFor), when a kind's
//     own mutation or a re-tag of a row that carries the list of accounts
//     changes a row's account from one account to another;
//   - by the assignment job, for each row it carries by its follow rule, so the
//     row's own followers follow in turn.
//
// The follow rule reads a parent's records to know which account it LEFT; the
// account it is in now is read off the parent itself, so a parent moved twice
// carries its followers to where it is, never to where it once went.
//
// # What a change asks of the job
//
// Followers are reached by a FULL pass of their kind, which the change asks for
// (RequestAccountFullPassIn, in the change's own transaction). "The follower
// kinds of K" is every engine-managed kind whose `via` can name a row of K: the
// kinds whose `via` resolves to K, and the Library's index kind, whose
// reference names any kind (followerKinds). A request names the whole chain
// below K (followerKindsClosure), because a grandchild can only follow once its
// parent has, and the job already orders requested passes parents first
// (assignmentCandidates).
//
//   - An engine-managed row getting its FIRST account asks nothing: its
//     children were already waiting on it, and the job's own passes assign them.
//   - A row of a kind that states its own account going from none to an
//     account asks for the passes: the job had left its children alone.
//   - Any row going from one account to another writes a change record and
//     asks for the passes.
//
// # Settled
//
// A record is SETTLED (done_at) once every follower kind of its row's kind has
// finished a full pass that STARTED after the record was written: such a pass
// read every follower, and carried each one the record applies to. The job
// settles records whenever one of its full passes finishes
// (settleAccountChangeRecords), and deletes settled records older than
// accountChangeRecordRetention, a bounded delete per finished pass. An export
// waits while a record naming its account is not settled (AccountFollowPending).
//
// Tests: account_follow_db_test.go, account_move_db_test.go.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/lib/pq"
	"github.com/uptrace/bun"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// AccountRowRef names one row: its concept and its id as stored.
type AccountRowRef struct{ Concept, ID string }

const (
	// accountChangeRecordRetention is how long a settled change record is kept.
	// The follow rule reads a parent's records to repair a stale append that
	// re-stamps a follower with the account its parent left; past this age such
	// an append is not expected any more.
	accountChangeRecordRetention = 90 * 24 * time.Hour
	// accountChangeRecordPurgeBatch bounds the delete one finished pass makes.
	accountChangeRecordPurgeBatch = 500
)

// EnqueueAccountFollow writes the change record of a row whose account changed
// from `from` to `to` (file header, Change records). tx is the change's own
// transaction, so the change and its record commit together. Nothing is
// written unless the row went from one account to ANOTHER: a first account
// (from "") has no account its followers could have been left in, and a row
// left with none has nothing to give them.
func EnqueueAccountFollow(ctx context.Context, tx bun.IDB, row AccountRowRef, from, to string) error {
	from, to = bareAccount(from), bareAccount(to)
	if from == "" || to == "" || from == to || row.Concept == "" || strings.TrimSpace(row.ID) == "" {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO account_follow_tasks (parent_concept, parent_id, from_account, to_account)
		VALUES (?, ?, ?, ?)`, row.Concept, row.ID, from, to); err != nil {
		return fmt.Errorf("account follow: recording the change of %s: %w", row.ID, err)
	}
	return nil
}

// AccountFollowPending counts the change records not yet settled that name the
// account as from or to; "" counts all (file header, Settled).
func AccountFollowPending(ctx context.Context, q AccountQuerier, account string) (int64, error) {
	bare := bareAccount(account)
	rows, err := q.QueryContext(ctx, `SELECT count(*) FROM account_follow_tasks
		WHERE done_at IS NULL AND ($1 = '' OR from_account = $1 OR to_account = $1)`, bare)
	if err != nil {
		return 0, fmt.Errorf("account follow: counting change records not settled: %w", err)
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
// concept, in name order (file header, What a change asks of the job).
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

// followerKindNames is followerKinds' names.
func followerKindNames(parent string) []string {
	kinds := followerKinds(parent)
	out := make([]string, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, k.Name)
	}
	return out
}

// followerKindsClosure is the follower kinds of every given kind, and theirs in
// turn, sorted: the chain a change below these kinds can reach.
func followerKindsClosure(concepts ...string) []string {
	seen := map[string]bool{}
	queue := append([]string{}, concepts...)
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		for _, name := range followerKindNames(next) {
			if !seen[name] {
				seen[name] = true
				queue = append(queue, name)
			}
		}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
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

// accountChange is what a write that changes a row's recorded account owes
// (file header, What a change asks of the job): a change record, a request for
// full passes of the kinds below it, or both.
type accountChange struct {
	concept, from, to string
	record            bool
	passes            []string
}

// accountChangeFor is what the write owes, or nil: recording is on, the row
// existed, its kind is account-scoped, and its account changes (bare compare,
// so a respelling is no change) to one.
func accountChangeFor(e *MemQLEngine, meta *memorynodes.Concept, prior, payload map[string]any) *accountChange {
	if !e.AccountStampingEnabled() || prior == nil || !meta.AccountScoped() {
		return nil
	}
	from, to := bareAccount(prior[accountField]), bareAccount(payload[accountField])
	if from == to || to == "" {
		return nil
	}
	if from == "" && meta.AccountEngineManaged() {
		return nil // its children were waiting on it; the job assigns them
	}
	passes := followerKindsClosure(meta.Name)
	if len(passes) == 0 && from == "" {
		return nil
	}
	return &accountChange{concept: meta.Name, from: from, to: to, record: from != "", passes: passes}
}

// apply writes the record and the requests through the write transaction's
// store.
func (c *accountChange) apply(ctx context.Context, store memorynodes.Store, id string) error {
	bs, ok := store.(*bunStore)
	if !ok {
		return fmt.Errorf("account follow: the write's store is not the database's")
	}
	if c.record {
		if err := EnqueueAccountFollow(ctx, bs.db, AccountRowRef{Concept: c.concept, ID: id}, c.from, c.to); err != nil {
			return err
		}
	}
	return RequestAccountFullPassIn(ctx, bs.db, c.passes)
}

// settleAccountChangeRecords marks settled every record whose row's follower
// kinds have each finished a full pass that started after the record was
// written, and deletes a bounded batch of settled records past their retention
// (file header, Settled). The job calls it when a full pass finishes, in that
// batch's transaction.
//
// It reads the kinds with records outstanding through the table's partial index
// on records not settled, and each kind's progress rows by name: no read of the
// row table.
func settleAccountChangeRecords(ctx context.Context, tx bun.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT parent_concept FROM account_follow_tasks WHERE done_at IS NULL`)
	if err != nil {
		return fmt.Errorf("account follow: reading the records to settle: %w", err)
	}
	var parents []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return err
		}
		parents = append(parents, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, parent := range parents {
		kinds := followerKindNames(parent)
		// The earliest start of the last finished full pass among the follower
		// kinds: a record written before it has been read past by every one.
		// A kind with no finished full pass (or no progress row) settles nothing.
		var through *time.Time
		if len(kinds) > 0 {
			var settledThrough sql.NullTime
			if err := tx.QueryRowContext(ctx, `SELECT CASE WHEN count(*) = ? AND bool_and(full_pass_at IS NOT NULL)
				   THEN min(full_pass_at) END
				  FROM account_assignment_progress WHERE concept = ANY(?::text[])`, len(kinds), pq.Array(kinds)).
				Scan(&settledThrough); err != nil {
				return fmt.Errorf("account follow: reading the passes of the followers of %s: %w", parent, err)
			}
			if settledThrough.Valid {
				through = &settledThrough.Time
			}
		}
		if len(kinds) > 0 && through == nil {
			continue
		}
		var err error
		if through == nil {
			_, err = tx.ExecContext(ctx, `UPDATE account_follow_tasks SET done_at = clock_timestamp()
				WHERE done_at IS NULL AND parent_concept = ?`, parent)
		} else {
			_, err = tx.ExecContext(ctx, `UPDATE account_follow_tasks SET done_at = clock_timestamp()
				WHERE done_at IS NULL AND parent_concept = ? AND created_at < ?`, parent, *through)
		}
		if err != nil {
			return fmt.Errorf("account follow: settling the records of %s: %w", parent, err)
		}
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM account_follow_tasks WHERE id IN (
		  SELECT id FROM account_follow_tasks WHERE done_at < clock_timestamp() - ?::interval LIMIT ?)`,
		fmt.Sprintf("%d seconds", int64(accountChangeRecordRetention.Seconds())), accountChangeRecordPurgeBatch)
	if err != nil {
		return fmt.Errorf("account follow: deleting settled records: %w", err)
	}
	return nil
}
