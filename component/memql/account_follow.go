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
// account_follow_tasks holds one row per row whose recorded account CHANGED:
// parent_concept, parent_id, from_account ("" for a row that had none),
// to_account, created_at (inside the change's transaction), seen_at (below)
// and done_at (Settled). A record is written
//
//   - by the governed move, for each root it moves or places, and for each row
//     tied to a placed root (account_move.go);
//   - by the write path (executeWrite, through accountChangeFor), when a kind's
//     own mutation or a re-tag of a row that carries the list of accounts
//     changes a row's account from one account to another, and when a row of a
//     kind that states its own account gets its first one;
//   - by the assignment job, for each row it carries by its follow rule or puts
//     back by its re-apply rule, so the row's own followers follow in turn.
//
// Records are NEVER deleted: the follow rule and the re-apply rule may need
// one for as long as its row exists, and a record that vanished would let the
// re-apply rule undo a change it no longer knew of.
//
// The follow rule reads a parent's records to know which account it LEFT, and
// when; the account it is in now is read off the parent itself, so a parent
// moved twice carries its followers to where it is, never to where it once
// went. A record with no from_account matches nothing in the follow rule
// (which only ever moves a row that has an account): for a placement the job's
// rule 3 gives a row with no account its parent's.
//
// An explicit change of an engine-managed row's account by a PERSON also writes
// a line in account_row_moves, the ledger the re-apply rule reads: the move and
// the placement write theirs, and a re-tag writes one too (in the write's
// transaction, moved_by the person, note "re-tag").
//
// # Seen
//
// A record's created_at is when it was INSERTED, inside the transaction that
// changed the row, and that transaction may run for seconds after (a move of
// hundreds of roots, a carrying batch). Until it commits, an ordinary write
// that creates a child reads the parent in the account it is leaving and files
// the child there. So "the rows the parent had when it left" is judged against
// seen_at instead: the moment the job's fold first read the record, which it
// can only do once the change has committed (foldAccountChangeRequests). A
// record not seen yet carries nothing and settles nothing. The cost, accepted:
// a child written before the fold that the raw surface filed under its
// writer's own account (they may not write to the parent's), where that account
// is the one the parent left, is carried with the parent, as a child written
// before the change would have been. The window lasts until the job's next
// fold: about one call while the job runs, and as long as the job was stopped
// or its fold kept failing otherwise.
//
// # What a change asks of the job
//
// A record IS the request. Followers are reached by a FULL pass of their kind;
// at the start of each call the job folds the records not settled into the
// requests of their follower kinds (foldAccountChangeRequests): "The follower
// kinds of K" is every engine-managed kind whose `via` can name a row of K, the
// kinds whose `via` resolves to K and the Library's index kind, whose reference
// names any kind (followerKinds). Nothing outside the job writes the job's
// progress rows: no write, no move and no carrying batch updates one, so none of
// them waits on, or deadlocks with, a batch that holds its kind's row.
//
//   - An engine-managed row getting its FIRST account writes nothing: its
//     children were already waiting on it, and the job's own passes assign them.
//   - A row of a kind that states its own account going from none to an
//     account writes a record with no from_account: the job had left its
//     children alone.
//   - Any row going from one account to another writes a record.
//
// # Settled
//
// A record is SETTLED (done_at) once it has been seen and every follower kind
// of its row's kind has finished a full pass whose SAFE POINT is after its
// seen_at: the earlier of the pass's start and its late-commit horizon
// (account_assignment.go, Passes), which progress keeps in full_pass_at. A pass
// whose safe point is before seen_at may have read the followers before the
// record could carry them, so it does not settle the record. A requested pass
// is pending on the same comparison: the fold's request is the newest seen_at
// of the records it folds. The job settles records whenever one
// of its full passes finishes (settleAccountChangeRecords). An export waits
// while a record naming its account is not settled (AccountFollowPending).
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

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// AccountRowRef names one row: its concept and its id as stored.
type AccountRowRef struct{ Concept, ID string }

// EnqueueAccountFollow writes the change record of a row whose account changed
// from `from` ("" for none) to `to` (file header, Change records). tx is the
// change's own transaction, so the change and its record commit together.
// Nothing is written when the account did not change or the row is left with
// none. Which first accounts write a record is the caller's decision
// (accountChangeFor): an engine-managed row's first stamp writes none.
func EnqueueAccountFollow(ctx context.Context, tx bun.IDB, row AccountRowRef, from, to string) error {
	from, to = bareAccount(from), bareAccount(to)
	if to == "" || from == to || row.Concept == "" || strings.TrimSpace(row.ID) == "" {
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
// (file header): a change record, and for a person's re-tag a ledger line.
type accountChange struct {
	concept, from, to string
	// retag: a person changed an engine-managed row's account through its list;
	// by is that person.
	retag bool
	by    string
}

// accountChangeFor is what the write owes, or nil: recording is on, the row
// existed, its kind is account-scoped, and its account changes (bare compare,
// so a respelling is no change) to one. namedList says the write named the
// row's list; person is the acting person's id, "" for server code.
func accountChangeFor(e *MemQLEngine, meta *memorynodes.Concept, prior, payload map[string]any, namedList bool, person string) *accountChange {
	if !e.AccountStampingEnabled() || prior == nil || !meta.AccountScoped() {
		return nil
	}
	from, to := bareAccount(prior[accountField]), bareAccount(payload[accountField])
	if from == to || to == "" {
		return nil
	}
	if from == "" && meta.AccountEngineManaged() && !namedList {
		return nil // a first stamp: its children were waiting on it; the job assigns them
	}
	retag := meta.AccountEngineManaged() && namedList && person != ""
	return &accountChange{concept: meta.Name, from: from, to: to, retag: retag, by: person}
}

// actingPerson is the id of the person a write is by, "" for server code
// (internal origin, or an actor that is not a person).
func actingPerson(ctx context.Context) string {
	if auth.OriginFromContext(ctx).IsInternal() {
		return ""
	}
	ac, _ := auth.AccessFromContext(ctx)
	if !actorIsAPerson(ac) {
		return ""
	}
	return ac.UserId
}

// apply writes the record, and a re-tag's ledger line, through the write
// transaction's store.
func (c *accountChange) apply(ctx context.Context, store memorynodes.Store, id string) error {
	bs, ok := store.(*bunStore)
	if !ok {
		return fmt.Errorf("account follow: the write's store is not the database's")
	}
	if c.retag {
		if _, err := bs.db.ExecContext(ctx, `INSERT INTO account_row_moves (concept, row_id, from_account, to_account, moved_by, followers, note)
			VALUES (?, ?, ?, ?, ?, 0, 're-tag')`, c.concept, id, c.from, c.to, c.by); err != nil {
			return fmt.Errorf("account follow: recording the re-tag of %s: %w", id, err)
		}
	}
	return EnqueueAccountFollow(ctx, bs.db, AccountRowRef{Concept: c.concept, ID: id}, c.from, c.to)
}

// accountChangeSeenBatch bounds how many records one fold marks seen, so the
// statement stays short after the job was stopped for a while. The rest are
// marked by the next calls; a record not yet marked is still counted as
// unsettled, so the bound changes no rule.
const accountChangeSeenBatch = 5000

// foldAccountChangeRequests turns the change records not settled into requests
// for full passes of their follower kinds (file header, What a change asks of
// the job). One short transaction of the job's own:
//
//   - First it marks SEEN (seen_at = clock_timestamp()) every record not seen
//     yet. It can only read a committed record, so seen_at is never earlier
//     than the commit of the change: a row written while the change was still
//     uncommitted read the parent in the account it was leaving, and its first
//     version is older than seen_at, so the record carries it (file header,
//     Seen). Records are taken in id order with SKIP LOCKED, so two replicas
//     folding at once never wait on each other; one another replica is marking
//     is marked by that one. At most accountChangeSeenBatch a call.
//   - Then each follower kind's full_pass_requested_at is raised to the newest
//     seen_at among the records it follows, so a request is never older than a
//     record it has to read past, and a record already covered by a finished
//     pass (its safe point is after the record's seen_at) asks for nothing
//     more. One statement over the kinds in sorted order; a progress row
//     another replica holds is skipped and folded at the next call.
//
// Reads the records through their partial index on records not settled.
func foldAccountChangeRequests(ctx context.Context, db *bun.DB, managed map[string]*memorynodes.Concept) error {
	return db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE account_follow_tasks SET seen_at = clock_timestamp()
			 WHERE id IN (SELECT id FROM account_follow_tasks
			               WHERE done_at IS NULL AND seen_at IS NULL ORDER BY id LIMIT ? FOR UPDATE SKIP LOCKED)`,
			accountChangeSeenBatch); err != nil {
			return fmt.Errorf("account follow: marking the records seen: %w", err)
		}
		rows, err := tx.QueryContext(ctx, `SELECT parent_concept, max(seen_at) FROM account_follow_tasks
			WHERE done_at IS NULL AND seen_at IS NOT NULL GROUP BY parent_concept`)
		if err != nil {
			return fmt.Errorf("account follow: reading the records to fold: %w", err)
		}
		newest := map[string]time.Time{}
		for rows.Next() {
			var parent string
			var at time.Time
			if err := rows.Scan(&parent, &at); err != nil {
				rows.Close()
				return err
			}
			for _, kind := range followerKindNames(parent) {
				if managed[kind] != nil && at.After(newest[kind]) {
					newest[kind] = at
				}
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil || len(newest) == 0 {
			return err
		}
		kinds := make([]string, 0, len(newest))
		for kind := range newest {
			kinds = append(kinds, kind)
		}
		sort.Strings(kinds)
		ats := make([]time.Time, len(kinds))
		for i, kind := range kinds {
			ats[i] = newest[kind]
		}
		_, err = tx.ExecContext(ctx, `UPDATE account_assignment_progress AS p
			   SET full_pass_requested_at = k.at, next_at = LEAST(p.next_at, clock_timestamp())
			  FROM unnest(?::text[], ?::timestamptz[]) AS k(concept, at)
			 WHERE p.concept = k.concept
			   AND (p.full_pass_requested_at IS NULL OR p.full_pass_requested_at < k.at)
			   AND (p.full_pass_at IS NULL OR p.full_pass_at <= k.at)
			   AND p.concept IN (SELECT concept FROM account_assignment_progress
			                      WHERE concept = ANY(?::text[]) ORDER BY concept FOR UPDATE SKIP LOCKED)`,
			pq.Array(kinds), pq.Array(ats), pq.Array(kinds))
		if err != nil {
			return fmt.Errorf("account follow: folding the records into requests: %w", err)
		}
		return nil
	})
}

// settleAccountChangeRecords marks settled every SEEN record whose row's
// follower kinds have each finished a full pass whose safe point is after the
// record's seen_at (file header, Settled). A record not seen yet is never
// settled: no pass has carried anything by it. The job calls it when a full
// pass finishes, in that batch's transaction.
//
// It reads the kinds with records outstanding through the table's partial index
// on records not settled, and each kind's progress rows by name: no read of the
// row table.
func settleAccountChangeRecords(ctx context.Context, tx bun.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT parent_concept FROM account_follow_tasks
		WHERE done_at IS NULL ORDER BY parent_concept`)
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
		// The earliest safe point of the last finished full pass among the
		// follower kinds: a record seen before it has been read past by every one.
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
				WHERE done_at IS NULL AND seen_at IS NOT NULL AND parent_concept = ?`, parent)
		} else {
			_, err = tx.ExecContext(ctx, `UPDATE account_follow_tasks SET done_at = clock_timestamp()
				WHERE done_at IS NULL AND parent_concept = ? AND seen_at < ?`, parent, *through)
		}
		if err != nil {
			return fmt.Errorf("account follow: settling the records of %s: %w", parent, err)
		}
	}
	return nil
}
