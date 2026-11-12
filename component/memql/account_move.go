package memql

// account_move.go -- MOVING AND PLACING ROWS (design record
// docs/superpowers/specs/2026-11-12-account-export-design.md, section I, "The
// act").
//
// MoveAccountRows takes at most moveMaxRoots rows and a destination account.
// It MOVES a row of an engine-managed kind from the account it is in to
// another, and PLACES a row of any account-scoped kind that has no account. It
// refuses outright, with an error, while this node does not record accounts
// (AccountStampingEnabled): the field is not written before the switch, by
// anyone. Otherwise it refuses per row, with a reason from a closed set:
//
//	cluster_wide        a kind declared @belongsTo(cluster): its rows are in no account
//	undeclared          a kind that declares nothing, or one the registry does not know
//	stays               a kind whose rows never leave the cluster (`stays=`)
//	follows_parent      a row whose parent is in the same account: move the parent.
//	                    For a placement "the same account" is none: a row whose
//	                    parent has no account either is placed by placing the parent
//	states_own_account  a `stated` kind's row that has an account: it changes in
//	                    its own app, under the organization rules
//	same_account        the row is in the destination already
//	unknown_account     the destination is not one of this cluster's active accounts
//	not_found           no row has that id
//
// The follows_parent rule is exported for the lists that mark such rows
// (RowsFollowingTheirParent), and judgeMoveRoot asks the same function.
//
// # What it writes, and how
//
// A row's account is decided by its newest version (newestAccountSQL,
// account_rows.go). The move writes that version in place, through the
// assignment's own statement (writeAssignments), guarded on the account the
// move read. An ordinary write in flight may meanwhile APPEND a newer version
// naming the account the row is leaving (it read the row before the move
// committed; goals, runs and steps take no per-row lock). So after its write
// the move reads the row's newest version again, and writes that one too while
// it still names the account being left, three rounds at most
// (moveRecheckRounds). What a later append still re-stamps, the assignment
// job's re-apply rule repairs on its next pass, from the ledger line
// (account_assignment_rules.go). A row another writer moved to a third account
// is left as that writer left it, and is reported neither as moved nor as
// refused. No version is appended, no event is published, no automation runs,
// and each touched kind's cached reads are invalidated after the commit.
//
// For each root that ends in the destination: one line in account_row_moves
// (from, to, who, a note, and how many tied rows were placed with it); a change
// record when it left an account (account_follow.go); and its open conflicts in
// account_assignment_conflict are resolved, as the job resolves those of a row
// it assigns, so an export of the accounts a conflict named does not wait on a
// row that has been decided. A `stated` root that is PLACED also places, at
// once and in the same transaction, the rows the organization rules tie to it
// (organizationReferences) that have no account: those references must agree
// for the root to be edited again at all. Those are counted.
//
// Followers are NOT counted. They are carried in the background by the
// assignment job, and the move asks it, once per call and in its transaction,
// for full passes of every kind below the kinds it moved or placed a root of
// (RequestAccountFullPassIn, followerKindsClosure). MoveResult.Followers names
// the kinds that will follow, each with Rows zero.
//
// Everything is one transaction. A preview runs every read and judgment, and
// writes and asks for nothing.
//
// Who may call is not decided here: the builtin that calls this holds
// `execute app:accounts/move` and a person's own session (design record,
// section H).
//
// Tests: account_move_db_test.go.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/lib/pq"
	"github.com/uptrace/bun"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// MoveRequest is one act of moving or placing rows.
type MoveRequest struct {
	Rows    []AccountRowRef
	To      string // bare account id
	By      string // user id, for the record
	Note    string
	Preview bool // answer what would happen and write nothing
}

// MoveLine is a count of rows of one kind.
type MoveLine struct {
	Concept string
	Rows    int64
}

// MoveRefusal is a row the act would not move, and why.
type MoveRefusal struct{ Concept, ID, Reason string }

// MoveResult is what the act did, or, for a preview, would do.
type MoveResult struct {
	Moved []MoveLine // roots, per kind
	// Followers is the kinds whose rows follow the roots. The rows the
	// organization rules tie to a placed `stated` root are placed with it and
	// counted here. Every other line is a kind the assignment job carries in
	// the background, and its Rows is ZERO: followers are named, never counted
	// (account_follow.go).
	Followers []MoveLine
	Refused   []MoveRefusal
}

// The refusal reasons, a closed set (file header).
const (
	moveRefusedClusterWide      = "cluster_wide"
	moveRefusedUndeclared       = "undeclared"
	moveRefusedStays            = "stays"
	moveRefusedFollowsParent    = "follows_parent"
	moveRefusedStatesOwnAccount = "states_own_account"
	moveRefusedSameAccount      = "same_account"
	moveRefusedUnknownAccount   = "unknown_account"
	moveRefusedNotFound         = "not_found"
)

// moveMaxRoots bounds one act: every root is read by id, and a compressed
// chunk has no id index, so the act is for a selection, never a whole account.
const moveMaxRoots = 500

// moveTieDepth bounds the walk through the rows the organization rules tie to a
// placed root: a campaign names an audience, which names nothing further.
const moveTieDepth = 4

// moveRecheckRounds bounds how many newer versions the move chases after its
// write (file header, What it writes); past that, the job's re-apply rule is
// the net.
const moveRecheckRounds = 3

// ErrAccountRecordingOff refuses the move while this node does not record
// accounts (file header).
var ErrAccountRecordingOff = errors.New("account move: this node does not record accounts yet, so no row can be moved or placed")

// accountMoveBeforeWrite, when set, runs between the act's reads and its
// writes; accountMoveAfterWrite after the writes, before the commit. Seams for
// the tests of a row another writer changes meanwhile; nothing else sets them.
var (
	accountMoveBeforeWrite func()
	accountMoveAfterWrite  func()
)

// moveRoot is one root the act will write.
type moveRoot struct {
	meta *memorynodes.Concept
	row  assignmentRow
	from string
	tied []moveRoot
}

// MoveAccountRows moves or places rows; see the file header.
func (e *MemQLEngine) MoveAccountRows(ctx context.Context, db *bun.DB, req MoveRequest) (MoveResult, error) {
	var result MoveResult
	if db == nil {
		return result, errors.New("account move: no database")
	}
	if !e.AccountStampingEnabled() {
		return result, ErrAccountRecordingOff
	}
	if len(req.Rows) > moveMaxRoots {
		return result, fmt.Errorf("account move: %d rows asked for; one act moves at most %d", len(req.Rows), moveMaxRoots)
	}
	to, err := BareAccountID(req.To)
	known := err == nil
	if known {
		known, err = activeAccountExists(ctx, db.DB, to)
		if err != nil {
			return result, err
		}
	}
	if !known {
		for _, ref := range req.Rows {
			result.Refused = append(result.Refused, MoveRefusal{Concept: ref.Concept, ID: ref.ID, Reason: moveRefusedUnknownAccount})
		}
		return result, nil
	}

	changed := map[string]bool{}
	err = db.RunInTx(ctx, &sql.TxOptions{}, func(ctx context.Context, tx bun.Tx) error {
		result = MoveResult{}
		clear(changed)
		var roots []moveRoot
		for _, ref := range req.Rows {
			root, reason, err := e.judgeMoveRoot(ctx, tx.Tx, ref, to)
			if err != nil {
				return err
			}
			if reason != "" {
				result.Refused = append(result.Refused, MoveRefusal{Concept: ref.Concept, ID: ref.ID, Reason: reason})
				continue
			}
			roots = append(roots, root)
		}
		if accountMoveBeforeWrite != nil {
			accountMoveBeforeWrite()
		}
		moved, tied := map[string]int64{}, map[string]int64{}
		var rootKinds []string
		for _, root := range roots {
			if !req.Preview {
				ok, err := e.writeMoveRoot(ctx, tx, root, to, req)
				if err != nil {
					return err
				}
				if !ok {
					continue // another writer moved the row elsewhere meanwhile
				}
				changed[root.meta.Name] = true
				for _, t := range root.tied {
					changed[t.meta.Name] = true
				}
			}
			moved[root.meta.Name]++
			rootKinds = append(rootKinds, root.meta.Name)
			for _, t := range root.tied {
				tied[t.meta.Name]++
				rootKinds = append(rootKinds, t.meta.Name)
			}
		}
		followers := map[string]int64{}
		for _, kind := range followerKindsClosure(rootKinds...) {
			followers[kind] = 0
		}
		for kind, n := range tied {
			followers[kind] = n
		}
		result.Moved, result.Followers = moveLines(moved), followerLines(followers)
		if req.Preview || len(rootKinds) == 0 {
			return nil
		}
		if accountMoveAfterWrite != nil {
			accountMoveAfterWrite()
		}
		return RequestAccountFullPassIn(ctx, tx, followerKindsClosure(rootKinds...))
	})
	if err != nil {
		return MoveResult{}, err
	}
	for concept := range changed {
		e.InvalidateCacheForConcept(concept)
	}
	return result, nil
}

// judgeMoveRoot reads one selected row and decides whether the act may move it
// to the destination; a refusal is returned as its reason.
func (e *MemQLEngine) judgeMoveRoot(ctx context.Context, q AccountQuerier, ref AccountRowRef, to string) (moveRoot, string, error) {
	meta, err := memorynodes.Get(ref.Concept)
	if err != nil || meta == nil || meta.BelongsTo == nil {
		return moveRoot{}, moveRefusedUndeclared, nil
	}
	if meta.ClusterWide() {
		return moveRoot{}, moveRefusedClusterWide, nil
	}
	if !meta.AccountScoped() {
		return moveRoot{}, moveRefusedUndeclared, nil
	}
	if meta.Stays() != "" {
		return moveRoot{}, moveRefusedStays, nil
	}
	row, found, err := newestRowOf(ctx, q, meta.Name, ref.ID)
	if err != nil {
		return moveRoot{}, "", err
	}
	if !found {
		return moveRoot{}, moveRefusedNotFound, nil
	}
	from, err := newestAccountOf(ctx, q, meta.Name, row.ID)
	if err != nil {
		return moveRoot{}, "", err
	}
	if from == to {
		return moveRoot{}, moveRefusedSameAccount, nil
	}
	if from != "" && meta.BelongsTo.Stated {
		return moveRoot{}, moveRefusedStatesOwnAccount, nil
	}
	links, err := parentLinksOf(ctx, q, meta, []assignmentRow{row})
	if err != nil {
		return moveRoot{}, "", err
	}
	if links[0].FollowsParent {
		return moveRoot{}, moveRefusedFollowsParent, nil
	}
	root := moveRoot{meta: meta, row: row, from: from}
	if from == "" && meta.BelongsTo.Stated {
		root.tied, err = tiedUnplacedRows(ctx, q, meta, row, map[string]bool{row.ID: true}, 0)
		if err != nil {
			return moveRoot{}, "", err
		}
	}
	return root, "", nil
}

// writeMoveRoot writes one root, chases a newer version an ordinary write
// appended meanwhile (file header), and, when the root's newest version names
// the destination at the end of that, writes the rows tied to it, its ledger
// line and change record, and resolves its open conflicts. ok is false when
// the root did not end in the destination; then none of that is written.
func (e *MemQLEngine) writeMoveRoot(ctx context.Context, tx bun.Tx, root moveRoot, to string, req MoveRequest) (bool, error) {
	account := accountValueFor(root.meta, to)
	target := root.row
	for round := 0; ; round++ {
		written, err := writeInPlaceAroundFence(ctx, tx, root.meta.Name, []assignmentWrite{{
			ID: target.ID, At: versionKey(target.At), Account: account, From: root.from,
		}})
		if err != nil {
			return false, err
		}
		if len(written.stamped) == 0 || round+1 >= moveRecheckRounds {
			break
		}
		newest, found, err := newestRowOf(ctx, tx.Tx, root.meta.Name, root.row.ID)
		if err != nil {
			return false, err
		}
		if !found || !newest.At.After(target.At) || bareAccount(newest.Payload[accountField]) != root.from {
			break
		}
		target = newest
	}
	final, err := newestAccountOf(ctx, tx.Tx, root.meta.Name, root.row.ID)
	if err != nil {
		return false, err
	}
	if final != to {
		return false, nil
	}
	var placed int64
	for _, tied := range root.tied {
		tiedWrite, err := writeInPlaceAroundFence(ctx, tx, tied.meta.Name, []assignmentWrite{{
			ID: tied.row.ID, At: versionKey(tied.row.At), Account: accountValueFor(tied.meta, to), From: "",
		}})
		if err != nil {
			return false, err
		}
		if len(tiedWrite.stamped) > 0 {
			placed++
			if err := resolveAssignmentConflicts(ctx, tx, tied.meta.Name, []string{tied.row.ID}); err != nil {
				return false, err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO account_row_moves (concept, row_id, from_account, to_account, moved_by, followers, note)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, root.meta.Name, root.row.ID, root.from, to, req.By, placed, req.Note); err != nil {
		return false, fmt.Errorf("account move: recording the move of %s: %w", root.row.ID, err)
	}
	if err := EnqueueAccountFollow(ctx, tx, AccountRowRef{Concept: root.meta.Name, ID: root.row.ID}, root.from, to); err != nil {
		return false, err
	}
	if err := resolveAssignmentConflicts(ctx, tx, root.meta.Name, []string{root.row.ID}); err != nil {
		return false, err
	}
	return true, nil
}

// RowPayload is one row of a kind, by its stored id, with its newest payload.
type RowPayload struct {
	ID      string
	Payload map[string]any
}

// ParentLink says, for one row, whether it follows a parent in the same
// account: the rule the move refuses a root by (follows_parent).
type ParentLink struct {
	ID string
	// ParentConcept is the kind of the row its via field names, "" when it
	// names none (no via, an empty field, a reference to no account-scoped
	// kind).
	ParentConcept string
	// FollowsParent: that parent exists and its newest version names the same
	// account as the row's newest version, "none" included.
	FollowsParent bool
}

// RowsFollowingTheirParent answers ParentLink for a page of rows of one kind,
// in their order, by the code judgeMoveRoot refuses follows_parent by: the
// rows' accounts and their parents' are read by newest version
// (newestAccountSQL), a few statements for the page. A kind the registry does
// not know answers no parent for every row.
func RowsFollowingTheirParent(ctx context.Context, q AccountQuerier, concept string, rows []RowPayload) ([]ParentLink, error) {
	meta, err := memorynodes.Get(concept)
	if err != nil || meta == nil {
		out := make([]ParentLink, len(rows))
		for i, r := range rows {
			out[i] = ParentLink{ID: r.ID}
		}
		return out, nil
	}
	in := make([]assignmentRow, len(rows))
	for i, r := range rows {
		in[i] = assignmentRow{ID: r.ID, Payload: r.Payload}
	}
	return parentLinksOf(ctx, q, meta, in)
}

// parentLinksOf is RowsFollowingTheirParent over rows already read.
//
// staged-data: MUST-NOT-GATE -- deciding whose a row is (newestAccountSQL); the move and the lists treat a staged row like any other.
func parentLinksOf(ctx context.Context, q AccountQuerier, meta *memorynodes.Concept, rows []assignmentRow) ([]ParentLink, error) {
	out := make([]ParentLink, len(rows))
	parents := map[string][]string{}
	ids := make([]string, 0, len(rows))
	for i, r := range rows {
		out[i].ID = r.ID
		ids = append(ids, r.ID)
		if pc, pid := assignmentParent(meta, r); pc != "" {
			out[i].ParentConcept = pc
			parents[pc] = append(parents[pc], pc+":"+pid, pid)
		}
	}
	if len(parents) == 0 {
		return out, nil
	}
	own, err := newestAccountsOf(ctx, q, meta.Name, ids)
	if err != nil {
		return nil, err
	}
	parentAccounts := map[string]map[string]string{} // concept -> bare id -> account
	for pc, spellings := range parents {
		accounts, err := newestAccountsOf(ctx, q, pc, distinctIds(spellings))
		if err != nil {
			return nil, err
		}
		byBare := map[string]string{}
		for id, account := range accounts {
			byBare[BareShortId(id)] = account
		}
		parentAccounts[pc] = byBare
	}
	for i, r := range rows {
		pc := out[i].ParentConcept
		if pc == "" {
			continue
		}
		_, pid := assignmentParent(meta, r)
		if account, exists := parentAccounts[pc][pid]; exists && account == own[r.ID] {
			out[i].FollowsParent = true
		}
	}
	return out, nil
}

// newestAccountsOfSQL is, for each id that has a row, the account its newest
// version names ("" for none). An id with no row is not in the answer.
//
// staged-data: MUST-NOT-GATE -- deciding whose a row is (newestAccountSQL).
const newestAccountsOfSQL = `SELECT c.id, COALESCE(` + newestAccountSQL + `, '')
  FROM (SELECT $1::text AS concept, unnest($2::text[]) AS id) c
 WHERE EXISTS (SELECT 1 FROM "MemoryNodes" n WHERE n.concept = c.concept AND n.id = c.id)`

// newestAccountsOf is newestAccountsOfSQL as a map of stored id to bare account.
func newestAccountsOf(ctx context.Context, q AccountQuerier, concept string, ids []string) (map[string]string, error) {
	out := map[string]string{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.QueryContext(ctx, newestAccountsOfSQL, concept, pq.Array(ids))
	if err != nil {
		return nil, fmt.Errorf("account move: reading the accounts of %s: %w", concept, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, account string
		if err := rows.Scan(&id, &account); err != nil {
			return nil, err
		}
		out[id] = bareAccount(account)
	}
	return out, rows.Err()
}

// followerLines is the Followers answer: a line per kind, sorted, zero rows
// included (MoveResult.Followers).
func followerLines(counts map[string]int64) []MoveLine {
	if len(counts) == 0 {
		return nil
	}
	out := make([]MoveLine, 0, len(counts))
	for concept, n := range counts {
		out = append(out, MoveLine{Concept: concept, Rows: n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Concept < out[j].Concept })
	return out
}

// tiedUnplacedRows is the rows the organization rules tie to a placed `stated`
// root (organizationReferences: an audience, a template, a sender) that have no
// account, and the rows tied to those in turn.
func tiedUnplacedRows(ctx context.Context, q AccountQuerier, meta *memorynodes.Concept, row assignmentRow, seen map[string]bool, depth int) ([]moveRoot, error) {
	if depth >= moveTieDepth {
		return nil, nil
	}
	fields := organizationReferences(meta.Name)
	names := make([]string, 0, len(fields))
	for field := range fields {
		names = append(names, field)
	}
	sort.Strings(names)
	var out []moveRoot
	for _, field := range names {
		ref, _ := row.Payload[field].(string)
		if strings.TrimSpace(ref) == "" {
			continue
		}
		tiedMeta, err := memorynodes.Get(fields[field])
		if err != nil || tiedMeta == nil || !tiedMeta.AccountScoped() {
			continue
		}
		tied, found, err := newestRowOf(ctx, q, tiedMeta.Name, ref)
		if err != nil {
			return nil, err
		}
		if !found || seen[tied.ID] {
			continue
		}
		seen[tied.ID] = true
		account, err := newestAccountOf(ctx, q, tiedMeta.Name, tied.ID)
		if err != nil {
			return nil, err
		}
		if account != "" {
			continue
		}
		out = append(out, moveRoot{meta: tiedMeta, row: tied})
		more, err := tiedUnplacedRows(ctx, q, tiedMeta, tied, seen, depth+1)
		if err != nil {
			return nil, err
		}
		out = append(out, more...)
	}
	return out, nil
}

// newestRowOf reads the newest version of one row, asking for both spellings
// of its id (a row's id column holds whatever its writer stored) and keeping
// the newer when both exist.
//
// staged-data: MUST-NOT-GATE -- the act moves a staged row like any other; the caller's capability is the wall.
func newestRowOf(ctx context.Context, q AccountQuerier, concept, id string) (assignmentRow, bool, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return assignmentRow{}, false, nil
	}
	bare := BareShortId(id)
	rows, err := readNewestVersions(ctx, q, concept, distinctIds([]string{id, concept + ":" + bare, bare}))
	if err != nil || len(rows) == 0 {
		return assignmentRow{}, false, err
	}
	row := rows[0]
	for _, r := range rows[1:] {
		if r.At.After(row.At) {
			row = r
		}
	}
	return row, true, nil
}

// newestAccountOfSQL is the account the newest version of one row names.
//
// staged-data: MUST-NOT-GATE -- deciding whose a row is (newestAccountSQL).
const newestAccountOfSQL = `SELECT COALESCE(` + newestAccountSQL + `, '') FROM (SELECT $1::text AS concept, $2::text AS id) c`

// newestAccountOf is the bare account the newest version of the row names, or
// "" for none.
func newestAccountOf(ctx context.Context, q AccountQuerier, concept, id string) (string, error) {
	rows, err := q.QueryContext(ctx, newestAccountOfSQL, concept, id)
	if err != nil {
		return "", fmt.Errorf("account move: reading the account of %s: %w", id, err)
	}
	defer rows.Close()
	account := ""
	if rows.Next() {
		if err := rows.Scan(&account); err != nil {
			return "", err
		}
	}
	return bareAccount(account), rows.Err()
}

// activeAccountExistsSQL is the newest version of an account row, either
// spelling.
//
// staged-data: MUST-NOT-GATE -- account existence is an authorization fact, staged or not (organization_ownership.go's ruling).
const activeAccountExistsSQL = `SELECT DISTINCT ON (id) payload FROM "MemoryNodes"
 WHERE concept = $1 AND id = ANY($2::text[]) ORDER BY id, "createdAt" DESC`

// activeAccountExists reports whether the bare id names one of this cluster's
// accounts that is not archived or deleted. The primary account always exists.
func activeAccountExists(ctx context.Context, q AccountQuerier, bare string) (bool, error) {
	if bare == primaryAccountBareId() {
		return true, nil
	}
	rows, err := q.QueryContext(ctx, activeAccountExistsSQL, conceptAccountsAccount,
		pq.Array([]string{bare, accountCanonicalPrefix + bare}))
	if err != nil {
		return false, fmt.Errorf("account move: reading the destination account: %w", err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return false, err
		}
		var payload map[string]any
		_ = json.Unmarshal(raw, &payload)
		if deleted, _ := payload["deleted"].(bool); deleted {
			continue
		}
		if status, _ := payload["status"].(string); status == "archived" || status == "deleted" {
			continue
		}
		found = true
	}
	return found, rows.Err()
}

// moveLines is a count per kind, sorted by kind.
func moveLines(counts map[string]int64) []MoveLine {
	if len(counts) == 0 {
		return nil
	}
	out := make([]MoveLine, 0, len(counts))
	for concept, n := range counts {
		if n > 0 {
			out = append(out, MoveLine{Concept: concept, Rows: n})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Concept < out[j].Concept })
	return out
}
