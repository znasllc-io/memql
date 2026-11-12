package memql

// account_move.go -- MOVING AND PLACING ROWS (design record
// docs/superpowers/specs/2026-11-12-account-export-design.md, section I, "The
// act").
//
// MoveAccountRows takes at most moveMaxRoots rows and a destination account.
// It MOVES a row of an engine-managed kind from the account it is in to
// another, and PLACES a row of any account-scoped kind that has no account. It
// refuses, per row and with a reason from a closed set:
//
//	cluster_wide        a kind declared @belongsTo(cluster): its rows are in no account
//	undeclared          a kind that declares nothing, or one the registry does not know
//	stays               a kind whose rows never leave the cluster (`stays=`)
//	follows_parent      a row whose parent is in the same account: move the parent
//	states_own_account  a `stated` kind's row that has an account: it changes in
//	                    its own app, under the organization rules
//	same_account        the row is in the destination already
//	unknown_account     the destination is not one of this cluster's active accounts
//	not_found           no row has that id
//
// # What it writes, and how
//
// A row's account is decided by its newest version (newestAccountSQL,
// account_rows.go), and a moved row's newest version is written in place by
// the assignment's own statement (writeAssignments), guarded on the account
// the move read: a row another writer changed in between is left as that
// writer left it, and is reported neither as moved nor as refused. No version
// is appended, no event is published, no automation runs, and each touched
// kind's cached reads are invalidated after the commit. Older versions keep the
// account they were written with, as the assignment leaves them.
//
// Each moved root gets one line in account_row_moves (from, to, who, a note,
// and how many rows follow it) and a follow task (account_follow.go), which
// carries its followers on the runner's next calls. The followers are counted
// before the write, by the same reads the tasks make, so the line and the
// answer say what will follow (a placement's are found by the task alone; see
// countFollowers). A `stated` root that is PLACED also places the
// rows the organization rules tie to it (organizationReferences) that have no
// account, at once and in the same transaction: those references must agree
// for the root to be edited again at all.
//
// Everything is one transaction. A preview runs every read and judgment and
// writes nothing.
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
	Moved     []MoveLine // roots, per kind
	Followers []MoveLine // rows that follow them, per kind
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

// accountMoveBeforeWrite, when set, runs between the act's reads and its
// writes. A seam for the test of a row another writer changes meanwhile;
// nothing else sets it.
var accountMoveBeforeWrite func()

// moveRoot is one root the act will write.
type moveRoot struct {
	meta      *memorynodes.Concept
	row       assignmentRow
	from      string
	followers map[string]int64
	tied      []moveRoot
}

// MoveAccountRows moves or places rows; see the file header.
func (e *MemQLEngine) MoveAccountRows(ctx context.Context, db *bun.DB, req MoveRequest) (MoveResult, error) {
	var result MoveResult
	if db == nil {
		return result, errors.New("account move: no database")
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
		moved, followers := map[string]int64{}, map[string]int64{}
		for _, root := range roots {
			if !req.Preview {
				ok, err := e.writeMoveRoot(ctx, tx, root, to, req)
				if err != nil {
					return err
				}
				if !ok {
					continue // the guard: another writer changed the row since it was read
				}
				changed[root.meta.Name] = true
				for _, tied := range root.tied {
					changed[tied.meta.Name] = true
				}
			}
			moved[root.meta.Name]++
			for kind, n := range root.followers {
				followers[kind] += n
			}
			for _, tied := range root.tied {
				followers[tied.meta.Name]++
			}
		}
		result.Moved, result.Followers = moveLines(moved), moveLines(followers)
		return nil
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
	if parentConcept, parentID := assignmentParent(meta, row); parentConcept != "" {
		parent, exists, err := newestRowOf(ctx, q, parentConcept, parentID)
		if err != nil {
			return moveRoot{}, "", err
		}
		if exists {
			parentAccount, err := newestAccountOf(ctx, q, parentConcept, parent.ID)
			if err != nil {
				return moveRoot{}, "", err
			}
			if parentAccount == from {
				return moveRoot{}, moveRefusedFollowsParent, nil
			}
		}
	}
	root := moveRoot{meta: meta, row: row, from: from, followers: map[string]int64{}}
	if err := countFollowers(ctx, q, AccountRowRef{Concept: meta.Name, ID: row.ID}, from, root.followers, 0); err != nil {
		return moveRoot{}, "", err
	}
	if from == "" && meta.BelongsTo.Stated {
		root.tied, err = tiedUnplacedRows(ctx, q, meta, row, map[string]bool{row.ID: true}, 0)
		if err != nil {
			return moveRoot{}, "", err
		}
	}
	return root, "", nil
}

// writeMoveRoot writes one root, the rows tied to it, its ledger line and its
// follow task. ok is false when the guard left the root alone; then nothing of
// it is written.
func (e *MemQLEngine) writeMoveRoot(ctx context.Context, tx bun.Tx, root moveRoot, to string, req MoveRequest) (bool, error) {
	written, err := writeInPlaceAroundFence(ctx, tx, root.meta.Name, []assignmentWrite{{
		ID: root.row.ID, At: versionKey(root.row.At), Account: accountValueFor(root.meta, to), From: root.from,
	}})
	if err != nil {
		return false, err
	}
	if len(written.stamped) == 0 {
		return false, nil
	}
	var total int64
	for _, n := range root.followers {
		total += n
	}
	for _, tied := range root.tied {
		tiedWrite, err := writeInPlaceAroundFence(ctx, tx, tied.meta.Name, []assignmentWrite{{
			ID: tied.row.ID, At: versionKey(tied.row.At), Account: accountValueFor(tied.meta, to), From: "",
		}})
		if err != nil {
			return false, err
		}
		if len(tiedWrite.stamped) > 0 {
			total++
			if err := EnqueueAccountFollow(ctx, tx, AccountRowRef{Concept: tied.meta.Name, ID: tied.row.ID}, "", to); err != nil {
				return false, err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO account_row_moves (concept, row_id, from_account, to_account, moved_by, followers, note)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, root.meta.Name, root.row.ID, root.from, to, req.By, total, req.Note); err != nil {
		return false, fmt.Errorf("account move: recording the move of %s: %w", root.row.ID, err)
	}
	if err := EnqueueAccountFollow(ctx, tx, AccountRowRef{Concept: root.meta.Name, ID: root.row.ID}, root.from, to); err != nil {
		return false, err
	}
	return true, nil
}

// countFollowers adds to `into`, per kind, every row that follows the row and
// would take its new account, generation after generation, by the reads the
// follow tasks make (account_follow.go).
//
// A row PLACED (it had no account) is not counted here. Its followers are the
// rows with no account that name it, which no index finds: the follow task
// walks the follower kinds in the background, and a person's request must not
// walk a kind of millions of rows while they wait. So the answer and the ledger
// line count, for a placement, only the rows tied to a `stated` root.
func countFollowers(ctx context.Context, q AccountQuerier, row AccountRowRef, from string, into map[string]int64, depth int) error {
	if depth >= assignableParentDepth || from == "" {
		return nil
	}
	for _, kind := range followerKinds(row.Concept) {
		after := ""
		for {
			rows, scanned, err := followersOf(ctx, q, row, from, kind, after, accountFollowBatchRows)
			if err != nil {
				return err
			}
			into[kind.Name] += int64(len(rows))
			for _, r := range rows {
				if err := countFollowers(ctx, q, AccountRowRef{Concept: kind.Name, ID: r.ID}, from, into, depth+1); err != nil {
					return err
				}
			}
			if len(scanned) < accountFollowBatchRows {
				break
			}
			after = scanned[len(scanned)-1]
		}
	}
	return nil
}

// followersOf reads one batch of a kind's candidates after afterID and keeps
// those that follow the row: their newest version names the account it left
// and names it in the via field. scanned is every candidate read, for the
// cursor. The follow task's own reads, without its write.
func followersOf(ctx context.Context, q AccountQuerier, row AccountRowRef, from string, kind *memorynodes.Concept,
	afterID string, limit int) (followers []assignmentRow, scanned []string, err error) {
	via := strings.TrimSpace(kind.BelongsTo.Via)
	parentBare := BareShortId(row.ID)
	parentCanonical := row.Concept + ":" + parentBare
	spellings := []string{parentCanonical, parentBare}
	if accountParentConcept(kind) == "" {
		spellings = []string{parentCanonical}
	}
	scanned, err = followCandidates(ctx, q, kind.Name, via, spellings, from, afterID, limit)
	if err != nil || len(scanned) == 0 {
		return nil, scanned, err
	}
	still, err := idsWithNewestAccount(ctx, q, kind.Name, scanned, from)
	if err != nil || len(still) == 0 {
		return nil, scanned, err
	}
	rows, err := readNewestVersions(ctx, q, kind.Name, still)
	if err != nil {
		return nil, scanned, err
	}
	for _, r := range rows {
		ref, _ := r.Payload[via].(string)
		ref = strings.TrimSpace(ref)
		for _, spelling := range spellings {
			if ref == spelling {
				followers = append(followers, r)
				break
			}
		}
	}
	return followers, scanned, nil
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
