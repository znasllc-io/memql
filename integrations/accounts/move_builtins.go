package accounts

// move_builtins.go -- the five calls behind the Data tab's rows, the move
// wizard and the rows awaiting a decision (design record
// docs/superpowers/specs/2026-11-12-account-export-design.md, sections H, I
// and K): accountRows, accountUnplaced, accountRowsMove,
// accountConflictLeaveOut and accountMoves.
//
// # Who may call
//
// Every call declares @requiresCapability("execute", "app:accounts/move")
// (dsl/accounts/builtins.memql), seeded on owner and developer only; the
// engine refuses every other caller before a handler here runs, and that is
// the wall (move_db_test.go holds it through the engine). The two acts that
// write -- a move that is not a preview, and leaving a conflict out -- are a
// person's decision, so they also ask auth.RequirePersonSession FIRST, before
// they read anything. The three reads and a preview change nothing and need
// only the part.
//
// # Rows are read directly
//
// accountRows reads a page of ids through memql.AccountRowIDs or
// memql.UnplacedRowIDs, each row's newest version and first createdAt through
// memql.AccountRowHeads, and whether it follows its parent through
// memql.RowsFollowingTheirParent (the rule the move refuses follows_parent by),
// with no per-row admission: the capability is the wall, and a staged kind's
// rows are the account's all the same. What leaves is narrow on purpose --
// the id, the values of the kind's @displayCard fields, the owner field the
// kind's @rowAuthz names, two dates and whether the row follows its parent --
// never a payload, and never a field the kind declares @secret, which is
// blanked even when @displayCard names it.
//
// # The move
//
// accountRowsMove is memql.MoveAccountRows (component/memql/account_move.go),
// which judges every row, writes in place, records one line per moved root in
// account_row_moves and a follow task for its followers. This file adds what a
// person's request needs around it: the 500-row bound in words, the
// destination refused in words when it is not an account, recording off
// refused in words, and the audit event, written only when the call moved or
// placed a row. The act closes a placed row's open conflicts itself, in its own
// transaction. The rows that hang off a moved row are carried afterwards by a
// background pass, so the answer names the kinds that follow and counts none of
// their rows.
//
// Tests: move_db_test.go.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/lib/pq"
	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	coreid "github.com/znasllc-io/memql/core/id"
)

const (
	// rowsPageDefault and rowsPageMax bound a page of accountRows and of
	// accountMoves.
	rowsPageDefault = 50
	rowsPageMax     = 200
	// moveMaxRows is the engine's own bound on one act (account_move.go's
	// moveMaxRoots), refused here first so the screen gets a sentence.
	moveMaxRows = 500
	// moveNoteMax bounds the note, in characters: it is stored once with every
	// row a move changes and once in the audit event.
	moveNoteMax = 500

	auditTargetAccount     = "account"
	auditRowsMoved         = "account_rows_moved"
	auditConflictLeftOut   = "account_conflict_left_out"
	moveRefusedUnknownAcct = "unknown_account"
)

// AccountMover is the engine's act (memql.MemQLEngine.MoveAccountRows).
type AccountMover interface {
	MoveAccountRows(ctx context.Context, db *bun.DB, req memql.MoveRequest) (memql.MoveResult, error)
}

// Moves is the move half of the accounts plug-in. Its fields are named apart
// from the other halves' because Provider embeds all three.
type Moves struct {
	rowsInv  *Integration
	moveExec Executor
	mover    AccountMover
	moveLog  *slog.Logger
}

// NewMoves builds the move half over the inventory's integration. exec writes
// the audit events; mover is the engine's act.
func NewMoves(inv *Integration, exec Executor, mover AccountMover, logger *slog.Logger) *Moves {
	if logger == nil {
		logger = slog.Default()
	}
	return &Moves{rowsInv: inv, moveExec: exec, mover: mover, moveLog: logger}
}

// wireMoves connects the move half to the engine the plug-in was handed. A
// node whose engine is not the concrete one has no act, and a move answers
// that in words rather than pretending.
func wireMoves(inv *Integration, pctx memql.PluginContext) *Moves {
	var exec Executor
	var mover AccountMover
	if pctx.Engine != nil {
		exec = pctx.Engine
	}
	if e, ok := pctx.Engine.(*memql.MemQLEngine); ok && e != nil {
		mover = e
	}
	return NewMoves(inv, exec, mover, pctx.Logger)
}

// moveCapabilities are the move half's builtins.
func (m *Moves) moveCapabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{Name: "rows", Description: "One page of a kind's rows in an account, or in no account: the id, the kind's display fields, owner and dates.", Handler: m.handleRows,
			ArgsSchema: map[string]string{"accountId": "string -- the account; empty for rows in no account.", "concept": "string (required) -- the kind.",
				"after": "string -- the cursor of the previous page.", "limit": "int -- at most 200, default 50.", "search": "string -- filters the page by its display values."}, PreserveOrder: true},
		{Name: "unplaced", Description: "Rows in no account, per kind, and the open conflicts awaiting a decision.", Handler: m.handleUnplaced, PreserveOrder: true},
		{Name: "rowsMove", Description: "Move rows to an account or place rows that have none; a preview writes nothing.", Handler: m.handleRowsMove,
			ArgsSchema: map[string]string{"rows": "[]object (required) -- {concept, id}, at most 500.", "toAccountId": "string (required) -- the destination.",
				"note": "string -- kept with the record.", "preview": "bool -- answer what would move and write nothing."}, PreserveOrder: true},
		{Name: "conflictLeaveOut", Description: "Record that a row awaiting a decision stays in no account.", Handler: m.handleConflictLeaveOut,
			ArgsSchema: map[string]string{"concept": "string (required)", "rowId": "string (required)", "conflict": "string (required) -- the conflict's code."}, PreserveOrder: true},
		{Name: "moves", Description: "What was moved, newest first, paged.", Handler: m.handleMoves,
			ArgsSchema: map[string]string{"accountId": "string -- the account as source or destination; empty for every move.",
				"after": "string -- the cursor of the previous page.", "limit": "int -- at most 200, default 50."}, PreserveOrder: true},
	}
}

// pageLimit reads `limit`: the default when absent, refused below one, and at
// most rowsPageMax.
func pageLimit(args map[string]any) (int, error) {
	limit, err := intArg(args, "limit", rowsPageDefault)
	if err != nil {
		return 0, err
	}
	if limit < 1 {
		return 0, errors.New("limit must be at least 1")
	}
	return min(limit, rowsPageMax), nil
}

// kind finds a loaded concept by name in the inventory's lister, which is the
// registry in production and a test's fixture otherwise.
func (m *Moves) kind(name string) *memorynodes.Concept {
	for _, c := range m.rowsInv.conceptList() {
		if c != nil && c.Name == name {
			return c
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// accountRows

// rowHead is what the page needs of one row: its newest version and when
// it was first written.
type rowHead struct {
	created, updated time.Time
	payload          map[string]any
}

func (m *Moves) handleRows(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	concept := stringArg(args, "concept")
	if concept == "" {
		return nil, errors.New("accountRows needs a kind: concept is empty")
	}
	meta := m.kind(concept)
	if meta == nil || !meta.AccountScoped() {
		return nil, fmt.Errorf("%s is not a kind whose rows belong to accounts", concept)
	}
	limit, err := pageLimit(args)
	if err != nil {
		return nil, err
	}
	db, err := m.rowsInv.database()
	if err != nil {
		return nil, err
	}
	account := ""
	if raw := stringArg(args, "accountId"); raw != "" {
		if account, err = memql.BareAccountID(raw); err != nil {
			return nil, err
		}
		if err := requireAccount(ctx, db, account); err != nil {
			return nil, err
		}
	}
	after := stringArg(args, "after")
	var ids []string
	if account == "" {
		ids, err = memql.UnplacedRowIDs(ctx, db.DB, concept, after, limit)
	} else {
		ids, err = memql.AccountRowIDs(ctx, db.DB, account, concept, after, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("accountRows: %w", err)
	}
	// The cursor is the last id READ, not the last id answered: search drops
	// rows from the page, and a cursor that stopped at the last kept row would
	// read the dropped ones again, so paging would never end.
	next := ""
	if len(ids) == limit {
		next = ids[len(ids)-1]
	}

	heads := map[string]*rowHead{}
	if err := memql.AccountRowHeads(ctx, db.DB, concept, ids, func(n memorynodes.MemoryNode, _ bool, first time.Time) error {
		heads[n.ID] = &rowHead{created: first, updated: n.CreatedAt, payload: decodePayload(n.Payload)}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("accountRows: %w", err)
	}
	// Whether each row follows its parent is the engine's own rule, the one
	// the move refuses follows_parent by, asked for the page at once.
	page := make([]memql.RowPayload, 0, len(ids))
	for _, id := range ids {
		if h := heads[id]; h != nil {
			page = append(page, memql.RowPayload{ID: id, Payload: h.payload})
		}
	}
	links, err := memql.RowsFollowingTheirParent(ctx, db.DB, concept, page)
	if err != nil {
		return nil, fmt.Errorf("accountRows: %w", err)
	}
	follows := map[string]string{}
	for _, l := range links {
		if l.FollowsParent {
			follows[l.ID] = l.ParentConcept
		}
	}

	search := strings.ToLower(stringArg(args, "search"))
	secret := map[string]bool{}
	for _, f := range meta.SecretFields() {
		secret[f] = true
	}
	out := make([]memorynodes.MemoryNode, 0, len(ids)+1)
	for _, id := range ids {
		v := heads[id]
		if v == nil {
			continue // removed between the two reads
		}
		p := v.payload
		primary, secondary, tertiary, status := displayValues(meta, id, p, secret)
		// Search filters what this page read; it is not a search of the
		// kind (a kind of millions of rows has no index on its display
		// fields), which is why the cursor above moves past what it drops.
		if search != "" && !containsFold(search, primary, secondary, tertiary, status) {
			continue
		}
		row := map[string]any{
			"kind": "row", "id": id, "concept": concept,
			"primary": primary, "secondary": secondary, "tertiary": tertiary, "status": status,
			"ownerUserId": ownerOf(meta, p, secret),
			"createdAt":   v.created.UTC().Format(time.RFC3339Nano), "updatedAt": v.updated.UTC().Format(time.RFC3339Nano),
			"followsParent": follows[id] != "",
		}
		if follows[id] != "" {
			row["parentConcept"] = follows[id]
		}
		n, err := node("rows:"+id, row)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	n, err := node("rows:page", map[string]any{"kind": "page", "nextCursor": next})
	if err != nil {
		return nil, err
	}
	return append(out, n), nil
}

// requireAccount refuses a bare id that names no account row. The primary
// account is an account before its row exists.
func requireAccount(ctx context.Context, db *bun.DB, bare string) error {
	if bare == primaryAccount {
		return nil
	}
	var exists bool
	if err := db.DB.QueryRowContext(ctx, accountExistsSQL, accountConcept, canonicalPrefix+bare, bare).Scan(&exists); err != nil {
		return fmt.Errorf("look up the account: %w", err)
	}
	if !exists {
		return fmt.Errorf("there is no account %q", bare)
	}
	return nil
}

func decodePayload(raw []byte) map[string]any {
	var p map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &p)
	}
	if p == nil {
		p = map[string]any{}
	}
	return p
}

// displayValues are the values of the fields the kind's @displayCard names,
// the id standing in for primary when it names none or the value is empty. A
// @secret field is never read, whatever the card names.
func displayValues(meta *memorynodes.Concept, id string, p map[string]any, secret map[string]bool) (primary, secondary, tertiary, status string) {
	field := func(name string) string {
		name = strings.TrimSpace(name)
		if name == "" || secret[name] {
			return ""
		}
		return displayText(p[name])
	}
	if card := meta.DisplayCard; card != nil {
		primary, secondary, tertiary, status = field(card.Primary), field(card.Secondary), field(card.Tertiary), field(card.Status)
	}
	if primary == "" {
		primary = id
	}
	return primary, secondary, tertiary, status
}

// displayText is a scalar as text. An object or a list is not a display value
// and reads as nothing, so no structure leaves through a display slot.
func displayText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return ""
	}
}

// ownerOf is the value of the owner field the kind's @rowAuthz names, if any.
func ownerOf(meta *memorynodes.Concept, p map[string]any, secret map[string]bool) string {
	if meta.RowAuthz == nil {
		return ""
	}
	f := strings.TrimSpace(meta.RowAuthz.Owner)
	if f == "" || secret[f] {
		return ""
	}
	s, _ := p[f].(string)
	return s
}

func containsFold(needle string, values ...string) bool {
	for _, v := range values {
		if strings.Contains(strings.ToLower(v), needle) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// accountUnplaced

// unplacedCountersSQL is the assignment job's own counters, read as the
// export's readiness reads them (export_readiness.go's unplacedSQL, kept as a
// copy so the two files change apart): `shared` is the rows the last pass left
// without an account on purpose (left alone, or left out by an owner) and
// `waiting` the rows held up behind a parent, exclusive outcomes of one
// judgment. Open conflicts are not counted here; they are listed below, one
// row each, and counting them twice would show the same row in both places.
const unplacedCountersSQL = `SELECT concept, shared, waiting FROM account_assignment_progress`

// openConflictsSQL is every conflict still open: not resolved by a later
// pass or a placement, and not left out by somebody.
const openConflictsSQL = `SELECT concept, row_id, kind, detail, candidates, found_at
  FROM account_assignment_conflict
 WHERE resolved_at IS NULL AND left_out_at IS NULL
 ORDER BY found_at DESC, concept, row_id, kind
 LIMIT $1`

func (m *Moves) handleUnplaced(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	db, err := m.rowsInv.database()
	if err != nil {
		return nil, err
	}
	// The same counters and the same kinds the export's readiness counts
	// (export_readiness.go): an account-scoped kind not declared `shared`
	// (where a row with no account is a statement, not a gap), as of the
	// job's last pass. No row is walked at request time.
	counted := map[string]bool{}
	for _, c := range m.rowsInv.conceptList() {
		if c != nil && c.AccountScoped() && !c.SharedRows() {
			counted[c.Name] = true
		}
	}
	rows, err := db.DB.QueryContext(ctx, unplacedCountersSQL)
	if err != nil {
		return nil, fmt.Errorf("accountUnplaced: read the assignment counters: %w", err)
	}
	type line struct {
		concept string
		rows    int64
	}
	var lines []line
	for rows.Next() {
		var concept string
		var shared, waiting int64
		if err := rows.Scan(&concept, &shared, &waiting); err != nil {
			rows.Close()
			return nil, fmt.Errorf("accountUnplaced: read the assignment counters: %w", err)
		}
		if n := shared + waiting; counted[concept] && n > 0 {
			lines = append(lines, line{concept, n})
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("accountUnplaced: read the assignment counters: %w", err)
	}
	rows.Close()
	sort.Slice(lines, func(a, b int) bool { return lines[a].concept < lines[b].concept })

	var out []memorynodes.MemoryNode
	for _, l := range lines {
		ns, name := splitConcept(l.concept)
		n, err := node("unplaced:"+l.concept, map[string]any{"kind": "unplaced", "concept": l.concept, "namespace": ns, "name": name, "rows": l.rows})
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}

	conflicts, err := db.DB.QueryContext(ctx, openConflictsSQL, conflictLimit)
	if err != nil {
		return nil, fmt.Errorf("accountUnplaced: read the conflicts: %w", err)
	}
	defer conflicts.Close()
	for conflicts.Next() {
		var concept, rowID, code, detail string
		var candidates pq.StringArray
		var found time.Time
		if err := conflicts.Scan(&concept, &rowID, &code, &detail, &candidates, &found); err != nil {
			return nil, fmt.Errorf("accountUnplaced: read the conflicts: %w", err)
		}
		n, err := node("conflict:"+concept+":"+rowID+":"+code, conflictPayload(concept, rowID, code, detail, candidates, found))
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	if err := conflicts.Err(); err != nil {
		return nil, fmt.Errorf("accountUnplaced: read the conflicts: %w", err)
	}
	return out, nil
}

func conflictPayload(concept, rowID, code, detail string, candidates []string, found time.Time) map[string]any {
	if candidates == nil {
		candidates = []string{}
	}
	return map[string]any{
		"kind": "conflict", "concept": concept, "rowId": rowID, "conflict": code,
		"detail": detail, "candidates": candidates, "foundAt": found.UTC().Format(time.RFC3339Nano),
	}
}

// ---------------------------------------------------------------------------
// accountRowsMove

func (m *Moves) handleRowsMove(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	preview := boolArg(args, "preview")
	by := ""
	if !preview {
		userID, err := auth.RequirePersonSession(ctx)
		if err != nil {
			return nil, err
		}
		by = userID
	} else if ac, ok := auth.AccessFromContext(ctx); ok && ac != nil {
		by = ac.UserId
	}

	refs, err := moveRefs(args["rows"])
	if err != nil {
		return nil, err
	}
	raw := stringArg(args, "toAccountId")
	if raw == "" {
		return nil, errors.New("Choose the account to move the rows to.")
	}
	to, err := memql.BareAccountID(raw)
	if err != nil {
		return nil, fmt.Errorf("%q is not an account.", raw)
	}
	if m.mover == nil {
		return nil, errors.New("This node cannot move rows. Try again.")
	}
	db, err := m.rowsInv.database()
	if err != nil {
		return nil, err
	}
	note := stringArg(args, "note")
	if utf8.RuneCountInString(note) > moveNoteMax {
		return nil, fmt.Errorf("A note is at most %d characters.", moveNoteMax)
	}
	result, err := m.mover.MoveAccountRows(ctx, db, memql.MoveRequest{Rows: refs, To: to, By: by, Note: note, Preview: preview})
	if errors.Is(err, memql.ErrAccountRecordingOff) {
		// The act refuses outright until this node records which account a
		// row belongs to: a move then would be undone by nothing and checked
		// by nothing. The engine's error names itself; the screen gets a
		// sentence.
		return nil, errors.New("Rows cannot be moved yet: this cluster does not record which account a row belongs to. Try again once recording is on.")
	}
	if err != nil {
		return nil, err
	}
	// The act refuses every row, one by one, when the destination is not an
	// active account. The screen asked for one destination, so it is told so
	// once, in words, instead of a list of identical refusals.
	if allRefusedAs(result, moveRefusedUnknownAcct) {
		return nil, fmt.Errorf("There is no active account %q to move the rows to.", to)
	}

	var moved int64
	for _, l := range result.Moved {
		moved += l.Rows
	}
	// An audit event records a change: a real call that moved or placed
	// nothing (every row refused, or changed by another writer meanwhile)
	// wrote nothing, and writes no event. A placed row's open conflicts are
	// closed by the act itself, in its own transaction.
	if !preview && moved > 0 {
		m.auditMove(ctx, to, note, result, moved)
	}
	return moveAnswer(result, preview, moved)
}

// moveRefs reads the `rows` argument: a list of {concept, id}, at least one
// and at most moveMaxRows.
func moveRefs(raw any) ([]memql.AccountRowRef, error) {
	list, ok := raw.([]any)
	if !ok {
		if raw == nil {
			return nil, errors.New("Choose at least one row to move.")
		}
		return nil, errors.New("rows must be a list of {concept, id}")
	}
	if len(list) == 0 {
		return nil, errors.New("Choose at least one row to move.")
	}
	if len(list) > moveMaxRows {
		return nil, fmt.Errorf("One move takes at most %d rows, and %d were chosen. Move them in smaller groups.", moveMaxRows, len(list))
	}
	refs := make([]memql.AccountRowRef, 0, len(list))
	for i, item := range list {
		obj, isObject := item.(map[string]any)
		if !isObject {
			return nil, fmt.Errorf("Row %d is not a row: each row is {concept, id}.", i+1)
		}
		concept, _ := obj["concept"].(string)
		id, _ := obj["id"].(string)
		concept, id = strings.TrimSpace(concept), strings.TrimSpace(id)
		if concept == "" || id == "" {
			return nil, fmt.Errorf("row %d names no kind or no id; each row is {concept, id}", i+1)
		}
		refs = append(refs, memql.AccountRowRef{Concept: concept, ID: id})
	}
	return refs, nil
}

func allRefusedAs(r memql.MoveResult, reason string) bool {
	if len(r.Moved) > 0 || len(r.Refused) == 0 {
		return false
	}
	for _, f := range r.Refused {
		if f.Reason != reason {
			return false
		}
	}
	return true
}

// moveAnswer is the call's rows. A follows line names a KIND and carries no
// count: the rows that follow a moved row are found by a background pass
// (account_follow.go), not counted while the person waits, so the engine
// answers which kinds follow and the summary says how many kinds. Rows tied
// to a placed `stated` root are placed in the same act and counted, so they
// arrive among the moved lines.
func moveAnswer(r memql.MoveResult, preview bool, moved int64) ([]memorynodes.MemoryNode, error) {
	kind := "moved"
	if preview {
		kind = "wouldMove"
	}
	var out []memorynodes.MemoryNode
	add := func(suffix string, p map[string]any) error {
		n, err := node(suffix, p)
		if err == nil {
			out = append(out, n)
		}
		return err
	}
	for _, l := range r.Moved {
		if err := add("move:"+kind+":"+l.Concept, map[string]any{"kind": kind, "concept": l.Concept, "rows": l.Rows}); err != nil {
			return nil, err
		}
	}
	for _, l := range r.Followers {
		if err := add("move:follows:"+l.Concept, map[string]any{"kind": "follows", "concept": l.Concept}); err != nil {
			return nil, err
		}
	}
	for i, f := range r.Refused {
		// The index keeps two refusals of one id apart (a row chosen twice).
		if err := add(fmt.Sprintf("move:refused:%d:%s:%s", i, f.Concept, f.ID),
			map[string]any{"kind": "refused", "concept": f.Concept, "id": f.ID, "reason": f.Reason}); err != nil {
			return nil, err
		}
	}
	if err := add("move:summary", map[string]any{"kind": "summary", "moved": moved, "followerKinds": len(r.Followers), "refused": len(r.Refused)}); err != nil {
		return nil, err
	}
	return out, nil
}

func (m *Moves) auditMove(ctx context.Context, to, note string, r memql.MoveResult, moved int64) {
	roots := map[string]any{}
	for _, l := range r.Moved {
		roots[l.Concept] = l.Rows
	}
	// The kinds that follow, not a count of their rows (moveAnswer says why).
	followerKinds := make([]string, 0, len(r.Followers))
	for _, l := range r.Followers {
		followerKinds = append(followerKinds, l.Concept)
	}
	m.auditAct(ctx, auditRowsMoved, to, "success", map[string]any{
		"toAccountId": to, "roots": roots, "moved": moved,
		"followerKinds": followerKinds, "refused": len(r.Refused), "note": note,
	})
}

// ---------------------------------------------------------------------------
// accountConflictLeaveOut

const leaveOutSQL = `UPDATE account_assignment_conflict
   SET left_out_at = clock_timestamp(), left_out_by = $4
 WHERE concept = $1 AND row_id = $2 AND kind = $3 AND resolved_at IS NULL AND left_out_at IS NULL
RETURNING detail, candidates, found_at, left_out_at`

const conflictStateSQL = `SELECT resolved_at IS NOT NULL, left_out_at FROM account_assignment_conflict
 WHERE concept = $1 AND row_id = $2 AND kind = $3`

func (m *Moves) handleConflictLeaveOut(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	userID, err := auth.RequirePersonSession(ctx)
	if err != nil {
		return nil, err
	}
	concept, rowID, code := stringArg(args, "concept"), stringArg(args, "rowId"), stringArg(args, "conflict")
	if concept == "" || rowID == "" || code == "" {
		return nil, errors.New("accountConflictLeaveOut needs the kind, the row and the conflict")
	}
	db, err := m.rowsInv.database()
	if err != nil {
		return nil, err
	}
	var detail string
	var candidates pq.StringArray
	var found, leftOut time.Time
	err = db.DB.QueryRowContext(ctx, leaveOutSQL, concept, rowID, code, userID).Scan(&detail, &candidates, &found, &leftOut)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, m.whyNotOpen(ctx, db, concept, rowID, code)
	}
	if err != nil {
		return nil, fmt.Errorf("accountConflictLeaveOut: %w", err)
	}
	m.auditAct(ctx, auditConflictLeftOut, "", "success", map[string]any{
		"concept": concept, "rowId": rowID, "conflict": code, "candidates": []string(candidates),
	})
	p := conflictPayload(concept, rowID, code, detail, candidates, found)
	p["leftOutAt"] = leftOut.UTC().Format(time.RFC3339Nano)
	n, err := node("conflict:"+concept+":"+rowID+":"+code, p)
	if err != nil {
		return nil, err
	}
	return []memorynodes.MemoryNode{n}, nil
}

// whyNotOpen says, in a sentence, why a conflict could not be left out.
func (m *Moves) whyNotOpen(ctx context.Context, db *bun.DB, concept, rowID, code string) error {
	var resolved bool
	var leftOut sql.NullTime
	err := db.DB.QueryRowContext(ctx, conflictStateSQL, concept, rowID, code).Scan(&resolved, &leftOut)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return errors.New("This row is not awaiting a decision.")
	case err != nil:
		return fmt.Errorf("accountConflictLeaveOut: %w", err)
	case leftOut.Valid:
		return fmt.Errorf("This row was already left out of every account, on %s.", leftOut.Time.UTC().Format("2 January 2006"))
	default:
		return errors.New("This row is no longer awaiting a decision: it has been placed in an account.")
	}
}

// ---------------------------------------------------------------------------
// accountMoves

// movesSQL pages the ledger newest first, one line per moved or placed root.
// The ledger's `followers` column is not answered: the rows that follow a
// root are carried by a background pass after the line is written, so the
// number it holds is not the rows that followed. $1 empty is every move; otherwise
// the account as source or destination, which each of the table's two indexes
// serves. The cursor is (moved_at, id) of the last row of the previous page.
const movesSQL = `SELECT id, concept, row_id, from_account, to_account, moved_by, moved_at, note
  FROM account_row_moves
 WHERE ($1 = '' OR to_account = $1 OR from_account = $1)
   AND ($2::timestamptz IS NULL OR (moved_at, id) < ($2::timestamptz, $3::bigint))
 ORDER BY moved_at DESC, id DESC
 LIMIT $4`

func (m *Moves) handleMoves(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	limit, err := pageLimit(args)
	if err != nil {
		return nil, err
	}
	account := ""
	if raw := stringArg(args, "accountId"); raw != "" {
		if account, err = memql.BareAccountID(raw); err != nil {
			return nil, err
		}
	}
	var cursorAt sql.NullTime
	var cursorID int64
	if after := stringArg(args, "after"); after != "" {
		at, id, ok := strings.Cut(after, "|")
		t, terr := time.Parse(time.RFC3339Nano, at)
		n, nerr := strconv.ParseInt(id, 10, 64)
		if !ok || terr != nil || nerr != nil {
			return nil, errors.New("after is not a cursor this call answered")
		}
		cursorAt, cursorID = sql.NullTime{Time: t, Valid: true}, n
	}
	db, err := m.rowsInv.database()
	if err != nil {
		return nil, err
	}
	rows, err := db.DB.QueryContext(ctx, movesSQL, account, cursorAt, cursorID, limit+1)
	if err != nil {
		return nil, fmt.Errorf("accountMoves: %w", err)
	}
	defer rows.Close()
	var out []memorynodes.MemoryNode
	next, count := "", 0
	for rows.Next() {
		var id int64
		var concept, rowID, from, to, by, note string
		var at time.Time
		if err := rows.Scan(&id, &concept, &rowID, &from, &to, &by, &at, &note); err != nil {
			return nil, fmt.Errorf("accountMoves: %w", err)
		}
		count++
		if count > limit {
			break // the extra row says there is another page
		}
		movedAt := at.UTC().Format(time.RFC3339Nano)
		next = movedAt + "|" + strconv.FormatInt(id, 10)
		n, err := node("move:"+strconv.FormatInt(id, 10), map[string]any{
			"kind": "move", "id": strconv.FormatInt(id, 10), "concept": concept, "rowId": rowID,
			"fromAccount": from, "toAccount": to, "movedBy": by, "movedAt": movedAt, "note": note,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("accountMoves: %w", err)
	}
	if count <= limit {
		next = ""
	}
	n, err := node("moves:page", map[string]any{"kind": "page", "nextCursor": next})
	if err != nil {
		return nil, err
	}
	return append(out, n), nil
}

// ---------------------------------------------------------------------------
// The audit trail

// auditAct writes one event, category data, target type account. A FAILED
// AUDIT WRITE DOES NOT FAIL THE ACT, which has committed (export_builtins.go's
// audit says why); it is logged.
func (m *Moves) auditAct(ctx context.Context, action, target, outcome string, detail map[string]any) {
	detached, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditWriteTimeout)
	defer cancel()
	if err := m.writeMoveAudit(detached, action, target, outcome, detail); err != nil {
		m.moveLog.Warn("account move: the audit event did not land", "action", action, "error", err)
	}
}

func (m *Moves) writeMoveAudit(ctx context.Context, action, target, outcome string, detail map[string]any) error {
	if m.moveExec == nil {
		return errors.New("account move: no engine to write the audit event")
	}
	actorID, actorEmail, actorRole := "", "", ""
	if ac, ok := auth.AccessFromContext(ctx); ok && ac != nil {
		actorID, actorEmail, actorRole = ac.UserId, ac.PrimaryEmail, string(ac.Role)
	}
	call, err := langparser.RenderCall("createAuditEvent", map[string]any{
		"eventId":     "v1:identity:auditEvent:" + coreid.NewShortId(),
		"occurredAt":  time.Now().UTC().Format(time.RFC3339Nano),
		"category":    auditCategoryData,
		"action":      action,
		"actorUserId": actorID,
		"actorEmail":  actorEmail,
		"actorRole":   actorRole,
		"targetType":  auditTargetAccount,
		"targetId":    target,
		"outcome":     outcome,
		"detail":      detail,
	})
	if err != nil {
		return err
	}
	// Internal origin for this one write, as the export's: auditEvent's create
	// admits a cluster owner or server code, and a developer moving rows is
	// neither. The actor it records is still the caller.
	_, err = m.moveExec.Execute(auth.ContextWithInternalOrigin(ctx), "mutation "+call)
	return err
}
