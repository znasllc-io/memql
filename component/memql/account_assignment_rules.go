package memql

// account_assignment_rules.go -- the judgment of one row by the assignment of
// existing rows (account_assignment.go): which account it gets, or why it gets
// none. Pure decisions over the rows a batch read, plus the few reads a
// decision needs (parents, the sites binding a store, owners' accounts).
//
// # The rules, first match wins
//
//  0. A row whose newest version is a deletion marker is skipped: it is a
//     tombstone, and its schema admits no other key.
//
// Then two rules that change an account a row already has, both for rows that
// a change elsewhere left behind (account_follow.go, account_move.go):
//
//   - RE-APPLY A MOVE. The row's latest ledger line (account_row_moves), the
//     move's, a placement's or a person's re-tag's, moved it from X to Y, its
//     newest version was created after that line (less
//     assignmentLateCommitSlack, because a writer sets createdAt before it
//     commits) and names X again, and no change record of the row's own leaves
//     Y after the line: it goes back to Y. This repairs the stale append of an
//     ordinary write that read the row before the move committed and wrote a
//     new version carrying the account it read. A later explicit change of the
//     row's own writes a later line, or a record, and is not undone; records
//     are never deleted, so this holds at any age. The row gets a change
//     record, so its followers are read again (a pass of theirs may have run
//     while it was back in X).
//   - FOLLOW. The row follows a parent P through `via`, its newest version
//     names X, P's newest names another account Y, and a change record of P
//     the job has SEEN says it left X: the row goes to P's CURRENT account Y,
//     never to the account a record names as its target, so a parent moved
//     twice carries its followers to where it is. Only a row whose first
//     version is older than the moment the job first saw the record (a row
//     attached later was filed by the rules then, and is not this record's to
//     move), and whose own latest ledger line, a person's explicit change, did
//     not put it in X after the record was written (their choice is kept; the
//     job's own carry records protect nothing; loadParentChanges). The row gets
//     a change record of its own, so its followers follow it. A row attached to P from a third account is
//     untouched unless P left that account after the row existed.
//
// Both are judged on every row a pass reads, full or incremental, so a stale
// append, being a new version, is repaired by the next incremental pass. The
// ledger lines and change records are read once per batch by id arrays, a
// parent's records only when its account differs from a child's, and a
// candidate's first version time and own records only for rows that otherwise
// match. Both write in place with the list held to the new account
// (writeAssignments).
//
//  1. It already has an account (bareAccount: the empty string is none, R6):
//     KEPT.
//  2. The concept still declares the list `accountIds` and the row's list is
//     not empty: among the tags the row's owner may place a row in (every tag
//     for a cluster operator, the owner's own accounts otherwise), the owner's
//     own account when it is one of them, otherwise the first. A tag naming an
//     account the owner does not belong to files nothing there; with no tag
//     left the list decides nothing. A row whose owner cannot be asked about
//     has its tags taken as written (placeableTags).
//  3. It has a `via` parent that exists: the parent's account. A parent that
//     exists with no account gives the row none either: assigning it from its
//     owner now would disagree with the parent once the parent is assigned
//     (the stamp's R5 rule, for the same reason). The row is WAITING when the
//     parent can still get an account from the job (parentCanStillGetAccount:
//     it is engine-managed, not in conflict, and the rules would assign it or
//     it waits in turn on a parent that can). Otherwise -- a parent of a stated
//     or shared concept with no account, one in conflict or left out -- the
//     row is LEFT ALONE: it is cluster-provided like its parent, or somebody
//     has to place the parent, and nothing the job does will change that. Only
//     a waiting row is counted in `waiting`, so only it brings the daily full
//     pass (account_assignment.go, Passes); a row left alone is counted with
//     the rows kept without an account (`shared`).
//  4. A Shopify store: the account of the site whose binding or previewBinding
//     names it. Sites of two different accounts are a CONFLICT
//     (store_bound_twice): which client a store belongs to is not something to
//     guess, and everything that follows the store waits with it.
//  5. It has an owner (the @rowAuthz owner field): that person's account, by
//     the rule a person's call uses (accountOfUser in call_account.go).
//  6. Nothing decides: a `shared` concept's row stays cluster-provided
//     (SHARED); any other row gets the primary account.
//
// Between rules 1 and 2: a row whose conflict an owner LEFT OUT
// (account_assignment_conflict.left_out_at) stays in no account for good,
// whatever the rules below would now say, and its conflict is not reopened.
// That is a decision a person made about the row; a guess here would undo it.
//
// A conflict records its CANDIDATES, the accounts the row could belong to, so
// "does a conflict name this account" is a lookup (ConflictsNaming): the
// accounts of the sites binding a store, or the account the rules chose for a
// row the Library fence refused.
//
// The same judgment answers the export's question, without writing
// (AccountRowsStillAssignable in account_assignment_api.go), which is why the
// reads below go through an AccountQuerier: the export asks inside its one
// snapshot, the job inside its batch's transaction.
//
// One conversion runs before the rules, on v1:shopify:channel: rows written
// before the field was renamed hold Shopify's own account id in `accountId`.
// A value that is not one of this cluster's accounts is moved to
// `sourceAccountId` when that is absent, and the row is then judged as having
// no account. When `sourceAccountId` is already present the value is dropped
// from `accountId` and `sourceAccountId` is left as it is: that is a row
// re-synced after this release, whose insert was read-merged over the stored
// row, so the old Shopify value survived in `accountId` beside the new sync's
// `sourceAccountId`, which is the current copy of the same thing.
//
// "An account" means a row of v1:accounts:account with that bare id, or the
// primary account `self` (isAccount). Account rows are archived, never
// removed, so an account id the stamp or this job wrote always finds its row
// and is kept; a value that finds none was not written as an account.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/lib/pq"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	coreid "github.com/znasllc-io/memql/core/id"
)

const (
	conceptShopifyChannel = "v1:shopify:channel"
	channelSourceAccount  = "sourceAccountId"

	conflictStoreBoundTwice     = "store_bound_twice"
	conflictLibraryFenceRefused = "library_fence_refused"
)

// assignmentRow is the newest version of one row.
type assignmentRow struct {
	ID      string
	At      time.Time
	Payload map[string]any
}

type assignmentOutcome int

const (
	outcomeSkip assignmentOutcome = iota
	outcomeKept
	outcomeAssigned
	outcomeShared
	// outcomeWaiting: the row follows a parent with no account that can still
	// get one from the job.
	outcomeWaiting
	outcomeConflict
	// outcomeLeftOut: an owner decided the row stays in no account.
	outcomeLeftOut
	// outcomeLeftAlone: the row follows a parent with no account that will
	// never get one from the job; somebody has to place the parent.
	outcomeLeftAlone
)

// assignmentDecision is the judgment of one row.
type assignmentDecision struct {
	row     assignmentRow
	outcome assignmentOutcome
	account string // outcomeAssigned
	kind    string // outcomeConflict
	detail  string // outcomeConflict
	// candidates are the accounts the row could belong to (outcomeConflict).
	candidates []string
	// from is the account the row holds now, which the write is guarded on:
	// "" for an assignment, the account it leaves for the follow and re-apply
	// rules (assignmentWrite.From).
	from string
	// carried: the follow rule moved the row, so it gets a change record of its
	// own and its followers follow in turn.
	carried bool
	// moveChannelAccount: rewrite the old Shopify value into sourceAccountId
	// before anything else is written to the row.
	moveChannelAccount bool
}

// readNewestVersions reads the newest version of each id. DISTINCT ON with the
// id leading the sort, and the ids as an array, for the reason versionsOfOneID
// gives: TimescaleDB cannot then choose an ordered append over the time column.
//
// staged-data: MUST-NOT-GATE -- the job judges every row of the concept, staged
// or not (assignmentBatchIdsFor).
func readNewestVersions(ctx context.Context, q AccountQuerier, concept string, ids []string) ([]assignmentRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT DISTINCT ON (id) id, "createdAt", payload FROM "MemoryNodes"
		WHERE concept = $1 AND id = ANY($2::text[]) ORDER BY id, "createdAt" DESC`, concept, pq.Array(ids))
	if err != nil {
		return nil, fmt.Errorf("account assignment: reading the rows of %s: %w", concept, err)
	}
	return scanAssignmentRows(rows)
}

// scanAssignmentRows reads (id, "createdAt", payload) rows and closes them.
func scanAssignmentRows(rows *sql.Rows) ([]assignmentRow, error) {
	defer rows.Close()
	var out []assignmentRow
	for rows.Next() {
		var r assignmentRow
		var raw []byte
		if err := rows.Scan(&r.ID, &r.At, &raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &r.Payload); err != nil || r.Payload == nil {
			r.Payload = map[string]any{}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// assignmentLookups holds what a batch's judgments read, each once per batch.
type assignmentLookups struct {
	e   *MemQLEngine
	q   AccountQuerier
	ctx context.Context
	// owners memoizes accountOfUser by bare user id; the context it reads
	// under carries the request memos, so a batch of one person's rows costs
	// one role read and one membership read.
	owners map[string]string
	// parents maps parent concept -> bare id -> its account ("" = unassigned);
	// an id absent from the inner map has no live row.
	parents map[string]map[string]string
	// storeAccounts maps a bare store id to the accounts of the sites binding
	// it, loaded on the first store row.
	storeAccounts map[string][]string
	// accounts is every account's bare id, loaded on the first channel row.
	accounts map[string]bool
	// conflicts is the recorded conflicts of the batch's rows, by stored id.
	conflicts map[string]recordedConflict
	// moves is the latest ledger line (account_row_moves) of each of the
	// batch's rows, by stored id, for the re-apply rule.
	moves map[string]ledgerLine
	// changedAway is, per row with a ledger line, the change records written
	// for it after that line, by stored id: a later change of its own, which
	// the re-apply rule must not undo.
	changedAway map[string][]changeRecord
	// parentLeft maps parent concept -> bare parent id -> an account its seen
	// change records say it LEFT -> the newest such records' times, for the
	// follow rule. Read only for parents whose account differs from a child's.
	parentLeft map[string]map[string]map[string]parentLeftAt
	// followable is the batch's rows the follow rule carries, by stored id
	// (loadParentChanges).
	followable map[string]bool
}

// ledgerLine is one row of account_row_moves.
type ledgerLine struct {
	from, to string
	at       time.Time
}

// parentLeftAt is when a parent left an account, by its newest seen records
// that say so (loadParentChanges): written is the newest created_at, seen the
// newest seen_at.
type parentLeftAt struct {
	written, seen time.Time
}

// changeRecord is one row of account_follow_tasks.
type changeRecord struct {
	from, to string
	at       time.Time
}

// recordedConflict is what account_assignment_conflict holds about one row:
// whether a conflict of it is still open, and whether an owner left it out.
type recordedConflict struct {
	open, leftOut bool
}

// judgeAssignmentBatch decides every row of one batch.
func (e *MemQLEngine) judgeAssignmentBatch(ctx context.Context, q AccountQuerier, meta *memorynodes.Concept, rows []assignmentRow) ([]assignmentDecision, error) {
	_, out, err := e.judgeAssignmentRows(ctx, q, meta, rows)
	return out, err
}

// judgeAssignmentRows is judgeAssignmentBatch that also hands back the lookups
// it read, for a caller that asks more of the same rows (the export's judge
// reads the recorded conflicts).
func (e *MemQLEngine) judgeAssignmentRows(ctx context.Context, q AccountQuerier, meta *memorynodes.Concept, rows []assignmentRow) (*assignmentLookups, []assignmentDecision, error) {
	return e.judgeAssignmentRowsAt(ctx, q, meta, rows, 0, map[string]bool{})
}

// judgeAssignmentRowsAt is the judgment at a depth of the walk up waiting
// parents, sharing the walk's memo of parents' prospects. A row the rules
// leave WAITING is waiting only while its parent can still get an account
// (parentCanStillGetAccount); otherwise it is LEFT ALONE (file header, rule 3).
func (e *MemQLEngine) judgeAssignmentRowsAt(ctx context.Context, q AccountQuerier, meta *memorynodes.Concept, rows []assignmentRow,
	depth int, memo map[string]bool) (*assignmentLookups, []assignmentDecision, error) {
	lk := &assignmentLookups{
		e:      e,
		q:      q,
		ctx:    contextWithPersonRoleMemo(contextWithAccountScopeMemo(ctx, e)),
		owners: map[string]string{},
	}
	if err := lk.loadParents(meta, rows); err != nil {
		return nil, nil, err
	}
	if err := lk.loadConflicts(meta, rows); err != nil {
		return nil, nil, err
	}
	if err := lk.loadMoves(meta, rows); err != nil {
		return nil, nil, err
	}
	if err := lk.loadParentChanges(meta, rows); err != nil {
		return nil, nil, err
	}
	out := make([]assignmentDecision, 0, len(rows))
	for _, row := range rows {
		d, err := lk.judge(meta, row)
		if err != nil {
			return nil, nil, err
		}
		if d.outcome == outcomeWaiting {
			parent, parentID := assignmentParent(meta, row)
			can, err := e.parentCanStillGetAccount(ctx, q, parent, parentID, depth, memo)
			if err != nil {
				return nil, nil, err
			}
			if !can {
				d.outcome = outcomeLeftAlone
			}
		}
		out = append(out, d)
	}
	return lk, out, nil
}

func (lk *assignmentLookups) judge(meta *memorynodes.Concept, row assignmentRow) (assignmentDecision, error) {
	d := assignmentDecision{row: row}
	if deleted, _ := row.Payload["deleted"].(bool); deleted {
		return d, nil // outcomeSkip
	}

	if meta.Name == conceptShopifyChannel {
		if value := bareAccount(row.Payload[accountField]); value != "" {
			known, err := lk.isAccount(value)
			if err != nil {
				return d, err
			}
			if !known {
				d.moveChannelAccount = true
				payload := make(map[string]any, len(row.Payload))
				for k, v := range row.Payload {
					payload[k] = v
				}
				delete(payload, accountField)
				row.Payload = payload
			}
		}
	}

	current := bareAccount(row.Payload[accountField])

	// Re-apply a move (file header).
	if line, ok := lk.moves[row.ID]; ok && current == line.from && line.to != current &&
		!row.At.Before(line.at.Add(-assignmentLateCommitSlack)) && !lk.changedAwayFrom(row.ID, line) {
		d = assignTo(d, line.to)
		d.from, d.carried = current, true
		return d, nil
	}

	// Follow (file header).
	if current != "" {
		if parentConcept, parentID := assignmentParent(meta, row); parentConcept != "" && lk.followable[row.ID] {
			if account, exists := lk.parents[parentConcept][parentID]; exists && account != "" && account != current {
				d = assignTo(d, account)
				d.from, d.carried = current, true
				return d, nil
			}
		}
	}

	// Rule 1.
	if current != "" {
		d.outcome = outcomeKept
		return d, nil
	}

	// Left out by an owner (file header).
	if lk.conflicts[row.ID].leftOut {
		d.outcome = outcomeLeftOut
		return d, nil
	}

	// Rule 2. The choice is made among the tags the row's owner may place a row
	// in, as the stamp holds a person's own write to them (stampAccountList):
	// a tag naming an account the owner does not belong to files nothing there.
	// When none of the tags qualifies, the list decides nothing and the row goes
	// on to the rules below.
	if conceptDeclaresField(meta, accountListField) {
		if listed := accountList(row.Payload[accountListField]); len(listed) > 0 {
			placeable, err := lk.placeableTags(meta, row, listed)
			if err != nil {
				return d, err
			}
			if len(placeable) > 0 {
				owner, err := lk.ownerAccount(meta, row)
				if err != nil {
					return d, err
				}
				return assignTo(d, pickListedAccount(placeable, owner)), nil
			}
		}
	}

	// Rule 3.
	if parentConcept, parentID := assignmentParent(meta, row); parentConcept != "" {
		if byID, read := lk.parents[parentConcept]; read {
			if account, exists := byID[parentID]; exists {
				if account == "" {
					d.outcome = outcomeWaiting
					return d, nil
				}
				return assignTo(d, account), nil
			}
		}
	}

	// Rule 4.
	if meta.Name == conceptShopifyStore {
		accounts, err := lk.sitesBinding(BareShortId(row.ID))
		if err != nil {
			return d, err
		}
		switch len(accounts) {
		case 0:
		case 1:
			return assignTo(d, accounts[0]), nil
		default:
			d.outcome, d.kind = outcomeConflict, conflictStoreBoundTwice
			d.detail = "bound by sites of accounts " + strings.Join(accounts, ", ")
			d.candidates = accounts
			return d, nil
		}
	}

	// Rule 5.
	owner, err := lk.ownerAccount(meta, row)
	if err != nil {
		return d, err
	}
	if owner != "" {
		return assignTo(d, owner), nil
	}

	// Rule 6.
	if meta.BelongsTo.Shared {
		d.outcome = outcomeShared
		return d, nil
	}
	return assignTo(d, primaryAccountBareId()), nil
}

func assignTo(d assignmentDecision, account string) assignmentDecision {
	d.outcome, d.account = outcomeAssigned, account
	return d
}

// assignmentParent is the concept and bare id of the row a `via` field names,
// or "" when there is none to follow: no `via`, an empty field, a reference
// that names no concept, or a concept whose rows have no account.
func assignmentParent(meta *memorynodes.Concept, row assignmentRow) (string, string) {
	via := strings.TrimSpace(meta.BelongsTo.Via)
	if via == "" {
		return "", ""
	}
	raw, _ := row.Payload[via].(string)
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ""
	}
	parent := accountParentConcept(meta)
	if parent == "" {
		concept, _, err := coreid.ParseNodeId(raw)
		if err != nil || concept == "" {
			return "", ""
		}
		parent = concept
	}
	parentMeta, err := memorynodes.Get(parent)
	if err != nil || parentMeta == nil || !parentMeta.AccountScoped() {
		return "", ""
	}
	return parent, BareShortId(raw)
}

// loadParents reads, per parent concept, the newest version of every parent the
// batch names, in the batch's transaction: a parent assigned by an earlier
// batch has committed, and one assigned in this batch cannot be (a concept
// never follows itself, belongsToViaGraph.cycles).
//
// Both spellings of an id are asked for, because a parent row's id column holds
// whatever its writer stored; results are keyed by bare id.
//
// staged-data: MUST-NOT-GATE -- a parent's account is a fact about the parent
// whether it is staged or not, and the child must take the same one.
func (lk *assignmentLookups) loadParents(meta *memorynodes.Concept, rows []assignmentRow) error {
	wanted := map[string][]string{}
	for _, row := range rows {
		if concept, id := assignmentParent(meta, row); concept != "" {
			wanted[concept] = append(wanted[concept], concept+":"+id, id)
		}
	}
	lk.parents = make(map[string]map[string]string, len(wanted))
	for concept, ids := range wanted {
		result, err := lk.q.QueryContext(lk.ctx, `SELECT DISTINCT ON (id) id, payload FROM "MemoryNodes"
			WHERE concept = $1 AND id = ANY($2::text[]) ORDER BY id, "createdAt" DESC`, concept, pq.Array(distinctIds(ids)))
		if err != nil {
			return fmt.Errorf("account assignment: reading parents in %s: %w", concept, err)
		}
		byID := map[string]string{}
		for result.Next() {
			var id string
			var raw []byte
			if err := result.Scan(&id, &raw); err != nil {
				result.Close()
				return err
			}
			var payload map[string]any
			_ = json.Unmarshal(raw, &payload)
			if deleted, _ := payload["deleted"].(bool); deleted {
				continue // a deleted parent is no parent
			}
			byID[BareShortId(id)] = bareAccount(payload[accountField])
		}
		err = result.Err()
		result.Close()
		if err != nil {
			return err
		}
		lk.parents[concept] = byID
	}
	return nil
}

// loadMoves reads the latest ledger line of each of the batch's rows, and the
// change records written for the rows that have one: one statement each, by id
// array.
//
// staged-data: MUST-NOT-GATE -- reads the move ledger and the change records, not the row table.
func (lk *assignmentLookups) loadMoves(meta *memorynodes.Concept, rows []assignmentRow) error {
	lk.moves = map[string]ledgerLine{}
	lk.changedAway = map[string][]changeRecord{}
	if len(rows) == 0 {
		return nil
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	result, err := lk.q.QueryContext(lk.ctx, `SELECT DISTINCT ON (row_id) row_id, from_account, to_account, moved_at
		  FROM account_row_moves WHERE concept = $1 AND row_id = ANY($2::text[])
		 ORDER BY row_id, moved_at DESC, id DESC`, meta.Name, pq.Array(ids))
	if err != nil {
		return fmt.Errorf("account assignment: reading the moves of %s: %w", meta.Name, err)
	}
	var moved []string
	for result.Next() {
		var id string
		var line ledgerLine
		if err := result.Scan(&id, &line.from, &line.to, &line.at); err != nil {
			result.Close()
			return err
		}
		line.from, line.to = bareAccount(line.from), bareAccount(line.to)
		lk.moves[id] = line
		moved = append(moved, id)
	}
	err = result.Err()
	result.Close()
	if err != nil || len(moved) == 0 {
		return err
	}
	records, err := lk.q.QueryContext(lk.ctx, `SELECT parent_id, from_account, to_account, created_at FROM account_follow_tasks
		WHERE parent_concept = $1 AND parent_id = ANY($2::text[])`, meta.Name, pq.Array(moved))
	if err != nil {
		return fmt.Errorf("account assignment: reading the change records of %s: %w", meta.Name, err)
	}
	defer records.Close()
	for records.Next() {
		var id string
		var r changeRecord
		if err := records.Scan(&id, &r.from, &r.to, &r.at); err != nil {
			return err
		}
		lk.changedAway[id] = append(lk.changedAway[id], r)
	}
	return records.Err()
}

// changedAwayFrom reports whether the row changed account after its ledger
// line by a change of its own (a re-tag, a later move): a record that leaves
// the account the line moved it to, written after the line.
func (lk *assignmentLookups) changedAwayFrom(id string, line ledgerLine) bool {
	for _, r := range lk.changedAway[id] {
		if bareAccount(r.from) == line.to && r.at.After(line.at) {
			return true
		}
	}
	return false
}

// loadParentChanges decides which rows of the batch the follow rule carries
// (file header, Follow). A row R in account X whose parent P is in another
// account Y is carried when a change record of P that the job has SEEN says P
// left X (a record not seen yet is not used: account_follow.go, Seen), and
//
//   - R's FIRST version was created before the record's seen_at, the moment
//     the job's fold first read it, which is after the change committed. A row
//     written while the change was uncommitted read P in X and was filed there:
//     it was P's when P left, and is carried. A row attached after the record
//     was seen was filed by the rules in force when it was written (a template
//     takes the parent's account; the raw surface files a row under the
//     writer's own account when they may not write to the parent's), and is
//     not this record's to move. No slack;
//   - R's own latest LEDGER LINE (account_row_moves: a person's move,
//     placement or re-tag) did not put it in X after the record was WRITTEN
//     (created_at, not seen_at). Both times are taken the same way, by the
//     database at the insert inside the change's own transaction, so the
//     comparison orders the two decisions as they were made. A person's change
//     made while the parent's change was in flight, or before the fold saw it,
//     is newer than the parent's leaving and is kept; judged against seen_at it
//     would be undone. Only a person's explicit change protects a child. A
//     change record the job wrote by carrying or re-applying protects nothing:
//     it follows the parent and is never the child's choice, and counting it
//     would strand a child carried in the same instant as a second move of its
//     parent (the carry's record newer than the parent's). A row whose last
//     change was a carry, a stale append after it was carried included, is
//     carried again.
//
// When P left X more than once, the newest seen record that says so decides:
// its seen_at bounds which rows P had, its created_at which of a child's own
// changes came after.
//
// Per batch, each by id array: P's seen records, only for parents whose account
// differs from a child's (in the steady state nothing is read); and, for the
// rows that otherwise match, their first createdAt (narrow columns). The
// ledger lines were read by loadMoves.
//
// THE CLOCK. A row's createdAt is set by the node that wrote it, and a
// record's seen_at is the database's clock at the moment the job first saw
// the record, so within their skew the judgment of "created before the record
// was seen" can be wrong either way. A node clock AHEAD of the database's
// makes a row that did exist before look newer, and it is left where it was. A
// node clock BEHIND makes a row created just after the record was seen look
// older, and the record carries it to the parent's account. Nothing later
// repairs either: the first waits for an owner to place it, and the second for
// an owner to move it back.
//
// staged-data: MUST-NOT-GATE -- reads the change records and a row's first version time; a staged row follows its parent like any other.
func (lk *assignmentLookups) loadParentChanges(meta *memorynodes.Concept, rows []assignmentRow) error {
	lk.parentLeft = map[string]map[string]map[string]parentLeftAt{}
	lk.followable = map[string]bool{}
	wanted := map[string][]string{}
	for _, row := range rows {
		current := bareAccount(row.Payload[accountField])
		if current == "" {
			continue
		}
		concept, id := assignmentParent(meta, row)
		if concept == "" {
			continue
		}
		if account, exists := lk.parents[concept][id]; exists && account != "" && account != current {
			wanted[concept] = append(wanted[concept], concept+":"+id, id)
		}
	}
	for concept, ids := range wanted {
		result, err := lk.q.QueryContext(lk.ctx, `SELECT parent_id, from_account, created_at, seen_at FROM account_follow_tasks
			WHERE parent_concept = $1 AND parent_id = ANY($2::text[]) AND seen_at IS NOT NULL`, concept, pq.Array(distinctIds(ids)))
		if err != nil {
			return fmt.Errorf("account assignment: reading the change records in %s: %w", concept, err)
		}
		byID := map[string]map[string]parentLeftAt{}
		for result.Next() {
			var id, from string
			var written, seen time.Time
			if err := result.Scan(&id, &from, &written, &seen); err != nil {
				result.Close()
				return err
			}
			bare := BareShortId(id)
			if byID[bare] == nil {
				byID[bare] = map[string]parentLeftAt{}
			}
			from = bareAccount(from)
			left := byID[bare][from]
			if written.After(left.written) {
				left.written = written
			}
			if seen.After(left.seen) {
				left.seen = seen
			}
			byID[bare][from] = left
		}
		err = result.Err()
		result.Close()
		if err != nil {
			return err
		}
		lk.parentLeft[concept] = byID
	}

	// The rows that otherwise match, with when their parent left their account.
	leftAt := map[string]parentLeftAt{}
	current := map[string]string{}
	for _, row := range rows {
		account := bareAccount(row.Payload[accountField])
		if account == "" {
			continue
		}
		concept, id := assignmentParent(meta, row)
		if t, ok := lk.parentLeft[concept][id][account]; ok {
			leftAt[row.ID], current[row.ID] = t, account
		}
	}
	if len(leftAt) == 0 {
		return nil
	}
	ids := make([]string, 0, len(leftAt))
	for id := range leftAt {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	first := map[string]time.Time{}
	result, err := lk.q.QueryContext(lk.ctx, `SELECT id, min("createdAt") FROM "MemoryNodes"
		WHERE concept = $1 AND id = ANY($2::text[]) GROUP BY id`, meta.Name, pq.Array(ids))
	if err != nil {
		return fmt.Errorf("account assignment: reading when the rows of %s were created: %w", meta.Name, err)
	}
	for result.Next() {
		var id string
		var at time.Time
		if err := result.Scan(&id, &at); err != nil {
			result.Close()
			return err
		}
		first[id] = at
	}
	err = result.Err()
	result.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		left := leftAt[id]
		created, ok := first[id]
		if !ok || !created.Before(left.seen) {
			continue
		}
		if line, ok := lk.moves[id]; ok && line.to == current[id] && line.at.After(left.written) {
			continue // a person's own change put it here after the parent left
		}
		lk.followable[id] = true
	}
	return nil
}

// loadConflicts reads the recorded conflicts of the batch's rows: the ones still
// open and the ones an owner left out. A resolved conflict says nothing.
func (lk *assignmentLookups) loadConflicts(meta *memorynodes.Concept, rows []assignmentRow) error {
	lk.conflicts = map[string]recordedConflict{}
	if len(rows) == 0 {
		return nil
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	result, err := lk.q.QueryContext(lk.ctx, `SELECT row_id, left_out_at IS NOT NULL FROM account_assignment_conflict
		WHERE concept = $1 AND row_id = ANY($2::text[]) AND (resolved_at IS NULL OR left_out_at IS NOT NULL)`,
		meta.Name, pq.Array(ids))
	if err != nil {
		return fmt.Errorf("account assignment: reading the conflicts of %s: %w", meta.Name, err)
	}
	defer result.Close()
	for result.Next() {
		var id string
		var leftOut bool
		if err := result.Scan(&id, &leftOut); err != nil {
			return err
		}
		c := lk.conflicts[id]
		if leftOut {
			c.leftOut = true
		} else {
			c.open = true
		}
		lk.conflicts[id] = c
	}
	return result.Err()
}

// ownerAccount is the account of the row's owner, or "" when the concept names
// no owner field, the row's is empty, or the principal table does not resolve
// the person. A read that failed is an error and fails the batch: a person in
// several accounts read as in none would be filed under the primary account.
func (lk *assignmentLookups) ownerAccount(meta *memorynodes.Concept, row assignmentRow) (string, error) {
	if meta.RowAuthz == nil {
		return "", nil
	}
	field := strings.TrimSpace(meta.RowAuthz.Owner)
	if field == "" {
		return "", nil
	}
	owner, _ := row.Payload[field].(string)
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return "", nil
	}
	key := BareShortId(owner)
	if account, done := lk.owners[key]; done {
		return account, nil
	}
	account, err := lk.e.accountOfUser(lk.ctx, owner)
	if err != nil {
		return "", fmt.Errorf("account assignment: the account of owner %s: %w", key, err)
	}
	lk.owners[key] = account
	return account, nil
}

// placeableTags narrows a row's tags to the accounts its owner may place a row
// in (placeableAccountsOfUser). A row with no owner field, no owner, or an owner
// the principal table does not resolve was not tagged by a person anybody can
// ask about, so its tags are taken as written: that is a row the system wrote,
// or one whose author is gone, and the tags are all that says whose it is.
func (lk *assignmentLookups) placeableTags(meta *memorynodes.Concept, row assignmentRow, listed []string) ([]string, error) {
	if meta.RowAuthz == nil {
		return listed, nil
	}
	field := strings.TrimSpace(meta.RowAuthz.Owner)
	if field == "" {
		return listed, nil
	}
	owner, _ := row.Payload[field].(string)
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return listed, nil
	}
	placeable, resolved, err := lk.e.placeableAccountsOfUser(lk.ctx, owner, listed)
	if err != nil {
		return nil, fmt.Errorf("account assignment: the accounts of owner %s: %w", BareShortId(owner), err)
	}
	if !resolved {
		return listed, nil
	}
	return placeable, nil
}

// sitesBinding is the sorted, distinct accounts of the sites whose binding or
// previewBinding names the store. A site with no account says nothing.
//
// staged-data: MUST-NOT-GATE -- which site binds a store decides whose store it
// is, staged or not.
func (lk *assignmentLookups) sitesBinding(storeID string) ([]string, error) {
	if lk.storeAccounts == nil {
		lk.storeAccounts = map[string][]string{}
		result, err := lk.q.QueryContext(lk.ctx, `SELECT DISTINCT ON (id) payload FROM "MemoryNodes"
			WHERE concept = $1 ORDER BY id, "createdAt" DESC`, conceptPlatformSite)
		if err != nil {
			return nil, fmt.Errorf("account assignment: reading sites: %w", err)
		}
		defer result.Close()
		seen := map[string]map[string]bool{}
		for result.Next() {
			var raw []byte
			if err := result.Scan(&raw); err != nil {
				return nil, err
			}
			var site map[string]any
			if json.Unmarshal(raw, &site) != nil {
				continue
			}
			if deleted, _ := site["deleted"].(bool); deleted {
				continue
			}
			account := bareAccount(site[accountField])
			if account == "" {
				continue
			}
			for _, key := range []string{"binding", "previewBinding"} {
				binding, _ := site[key].(map[string]any)
				store := bareAccount(binding["storeId"])
				if store == "" {
					continue
				}
				if seen[store] == nil {
					seen[store] = map[string]bool{}
				}
				seen[store][account] = true
			}
		}
		if err := result.Err(); err != nil {
			return nil, err
		}
		for store, accounts := range seen {
			list := make([]string, 0, len(accounts))
			for a := range accounts {
				list = append(list, a)
			}
			sort.Strings(list)
			lk.storeAccounts[store] = list
		}
	}
	return lk.storeAccounts[storeID], nil
}

// isAccount reports whether a bare id names one of this cluster's accounts.
//
// staged-data: MUST-NOT-GATE -- account existence is an authorization fact,
// staged or not (organization_ownership.go's ruling).
func (lk *assignmentLookups) isAccount(id string) (bool, error) {
	if lk.accounts == nil {
		lk.accounts = map[string]bool{primaryAccountBareId(): true}
		result, err := lk.q.QueryContext(lk.ctx, `SELECT DISTINCT id FROM "MemoryNodes" WHERE concept = $1`, conceptAccountsAccount)
		if err != nil {
			return false, fmt.Errorf("account assignment: reading accounts: %w", err)
		}
		defer result.Close()
		for result.Next() {
			var accountID string
			if err := result.Scan(&accountID); err != nil {
				return false, err
			}
			lk.accounts[BareShortId(accountID)] = true
		}
		if err := result.Err(); err != nil {
			return false, err
		}
	}
	return lk.accounts[id], nil
}

// assignableParentDepth bounds the walk up a chain of waiting parents. `via`
// chains are acyclic (belongsToViaGraph) and a few deep; a chain longer than
// this is answered "cannot", which leaves the row alone rather than looping.
const assignableParentDepth = 16

// parentCanStillGetAccount reports whether the row a row follows will get an
// account from the job: its concept is engine-managed, it has no recorded
// conflict, and the rules would assign it now, or it waits in turn on a parent
// that can. A parent of a stated or shared concept with no account, one in
// conflict or left out, or one gone, leaves its follower for a person to
// place: that follower is LEFT ALONE, not waiting (file header, rule 3).
// Answers are memoized by concept and bare id for the judgment that asks.
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
	lk, plan, err := e.judgeAssignmentRowsAt(ctx, q, meta, []assignmentRow{row}, depth+1, memo)
	if err != nil || len(plan) == 0 {
		return false, err
	}
	if lk.conflicts[row.ID].open {
		return false, nil
	}
	// A waiting parent was judged with its own parent's prospects already
	// (judgeAssignmentRowsAt), so waiting here means it can still get one.
	switch plan[0].outcome {
	case outcomeAssigned, outcomeKept, outcomeWaiting:
		return true, nil
	}
	return false, nil
}
