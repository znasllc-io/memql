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
//     exists with no account leaves the row WAITING: assigning it from its
//     owner now would disagree with the parent once the parent is assigned
//     (the stamp's R5 rule, for the same reason). When the parent is a stated
//     or shared concept that will never be assigned, waiting is the row's final
//     state, which is right: it is cluster-provided like its parent, or its
//     parent's owner has to place it.
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
	outcomeWaiting
	outcomeConflict
	// outcomeLeftOut: an owner decided the row stays in no account.
	outcomeLeftOut
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
// asks whether a waiting row's parent can still get an account).
func (e *MemQLEngine) judgeAssignmentRows(ctx context.Context, q AccountQuerier, meta *memorynodes.Concept, rows []assignmentRow) (*assignmentLookups, []assignmentDecision, error) {
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
	out := make([]assignmentDecision, 0, len(rows))
	for _, row := range rows {
		d, err := lk.judge(meta, row)
		if err != nil {
			return nil, nil, err
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

	// Rule 1.
	if bareAccount(row.Payload[accountField]) != "" {
		d.outcome = outcomeKept
		return d, nil
	}

	// Left out by an owner (file header).
	if lk.conflicts[row.ID].leftOut {
		d.outcome = outcomeLeftOut
		return d, nil
	}

	// Rule 2. The choice is made among the tags the row's owner may place a row
	// in, as the stamp makes it for a person's own write (pickPlaceableAccount):
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
