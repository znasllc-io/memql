package accounts

// inventory.go -- accountInventory and accountAssignmentStatus.
//
// # Who may call
//
// Both builtins declare @requiresCapability("execute", "app:accounts/inventory")
// (dsl/accounts/builtins.memql), seeded on owner and developer only. The engine
// refuses every other caller before a handler here runs
// (refuseBuiltinBelowRequiredCapability), so the capability IS the wall: the
// handlers read rows directly and return COUNTS, never a row, so there is no
// per-row admission to apply and no payload that could leave.
//
// # Why raw SQL
//
// The answer is one grouped count over every concept that declares an account
// form. The engine's read path answers one concept per query and pages its
// rows. The counts, and the status panel's per-account totals, come from
// memql.AccountRowCounts (component/memql/account_rows.go), the rule an export
// carries: a row is counted once, under the account its newest version names,
// across the row table and the observation store, and `versions` is every
// version of those rows. It is decided from narrow columns, the account
// matched by the account index, so no payload is unpacked. The file sum below
// is raw SQL of its own over the partial index idx_memorynodes_account, whose
// predicate it repeats so the planner can use it.
//
// # What counts
//
// Only concepts that DECLARE an account form (`@belongsTo(account ...)`) are
// counted. A cluster-wide concept may carry a field that happens to be called
// accountId (a mirrored vendor's own account number, say); its rows are not
// the account's, and counting them would put foreign rows in the ledger.
//
// Every read matches both spellings of an account id, bare and
// `v1:accounts:account:<bare>`, because the stamp writes the bare id while a
// concept that declares a relationship on the field stores the canonical one
// (design record, R7).
//
// Neither builtin is cached: a builtin call dispatches to its handler on every
// call and never passes through the engine's result cache, which serves only
// planned reads. The db test re-reads after a write to hold that.
//
// Tests: inventory_db_test.go, status_db_test.go, and
// component/memql/account_inventory_gate_test.go for the capability.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/uptrace/bun"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

const (
	// resultConcept is the synthetic concept every answer row rides on. Never
	// persisted.
	resultConcept = "integration:accounts:result"
	// canonicalPrefix is the account concept's id prefix. Its length is the
	// 20 the SQL's substr(..., 21) strips.
	canonicalPrefix = "v1:accounts:account:"
	accountConcept  = "v1:accounts:account"
	fileConcept     = "v1:library:file"
	// primaryAccount is the installation's own account. It is an account by
	// definition (the assignment job places rows on it before anyone has
	// written its row), so it is never refused as unknown.
	primaryAccount = "self"
	// conflictLimit bounds the review list the status returns.
	conflictLimit = 200

	// inventoryDeadline bounds the inventory's two statements together. The
	// request pool cancels a statement at 8 seconds and the node table holds
	// tens of millions of rows; the count reads the account's versions through
	// the account index and its rows' version counts, so an account holding
	// most of a large cluster can take longer than one read may. A deadline of its own, shorter than the
	// pool's, lets the handler answer in words the panel can show instead of a
	// raw timeout.
	inventoryDeadline = 6 * time.Second
	// totalsDeadline bounds the per-account totals in the status. They are a
	// convenience beside the switch, the progress and the conflicts, which
	// read tiny tables, so the status never fails because the totals were
	// slow: it answers without them.
	totalsDeadline = 4 * time.Second
)

// inventoryTooLarge is the refusal the panel shows when the inventory's
// deadline passes.
const inventoryTooLarge = "This account holds too many rows to count in one read. Try again; a narrower read is coming with export."

// Integration is the plug-in.
type Integration struct {
	db       func() *bun.DB
	concepts func() []*memorynodes.Concept
	// held is this node's MEMQL_ACCOUNT_STAMPING_ACTIVATE=false. Reported
	// beside the cluster's switch because a held node neither stamps nor
	// assigns even where the switch is on, and the operator reading the panel
	// needs to know the node that answered is one of those.
	held bool
	// logger receives the one warning a status read gives when it answers
	// without its totals.
	logger *slog.Logger
	// inventoryDeadline and totalsDeadline are the constants above; fields so
	// a test can pass a deadline that has already expired.
	inventoryDeadline time.Duration
	totalsDeadline    time.Duration
}

// IntegrationName implements memql.IntegrationProvider.
func (i *Integration) IntegrationName() string { return "accounts" }

// Capabilities implements memql.IntegrationProvider.
func (i *Integration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name:          "inventory",
			Description:   "What one account holds: rows and versions per concept that declares an account form, and the bytes of its current files.",
			Handler:       i.handleInventory,
			ArgsSchema:    map[string]string{"accountId": "string (required) -- the account, bare or canonical."},
			PreserveOrder: true,
		},
		{
			Name:          "assignmentStatus",
			Description:   "How the assignment of existing rows to accounts is going: the switch, progress per concept, rows per account and the open conflicts.",
			Handler:       i.handleAssignmentStatus,
			PreserveOrder: true,
		},
	}
}

// conceptList is the registry's concepts, read on every call so a concept
// mounted or reloaded since boot is counted.
func (i *Integration) conceptList() []*memorynodes.Concept {
	if i.concepts == nil {
		return nil
	}
	return i.concepts()
}

func (i *Integration) database() (*bun.DB, error) {
	if i.db == nil {
		return nil, errors.New("accounts: this node has no database")
	}
	db := i.db()
	if db == nil {
		return nil, errors.New("accounts: this node has no database")
	}
	return db, nil
}

// splitConcept returns the namespace (the second segment of the id) and the
// name (the last). An id of another shape keeps its whole text as the name.
func splitConcept(id string) (namespace, name string) {
	parts := strings.Split(id, ":")
	if len(parts) >= 3 {
		return parts[1], parts[len(parts)-1]
	}
	return "", id
}

// accountIdPresentSQL is the shared predicate: the partial index's own condition,
// repeated so the planner can match it.
const accountIdPresentSQL = `(payload->>'accountId') IS NOT NULL AND (payload->>'accountId') <> ''`

// inventoryFilesSQL sums the size of the account's current files: the newest
// version of each file row that ever named the account, when that version
// still names it and is not archived. Archived files are left out because an
// archive is the Library's delete; a version whose size is not a JSON number
// contributes nothing rather than failing the read.
//
// staged-data: MUST-NOT-GATE -- an inventory of what an account holds; the bytes are the account's whether or not the file concept's data is staged, and leaving them out would report an account smaller than it is.
const inventoryFilesSQL = `SELECT COALESCE(sum((f.payload->>'size')::numeric), 0)::bigint
  FROM (SELECT DISTINCT ON (id) payload
          FROM "MemoryNodes"
         WHERE concept = $3
           AND id IN (SELECT id FROM "MemoryNodes"
                       WHERE ` + accountIdPresentSQL + `
                         AND (payload->>'accountId') IN ($1, $2)
                         AND concept = $3)
         ORDER BY id, "createdAt" DESC) f
 WHERE (f.payload->>'accountId') IN ($1, $2)
   AND COALESCE(f.payload->>'archived', 'false') <> 'true'
   AND jsonb_typeof(f.payload->'size') = 'number'`

// accountExistsSQL answers whether an account row exists in either spelling.
//
// staged-data: MUST-NOT-GATE -- deciding whether the id names an account at all; a staged account is still one, and refusing it as unknown would hide rows that are already its.
const accountExistsSQL = `SELECT EXISTS (SELECT 1 FROM "MemoryNodes" WHERE concept = $1 AND id IN ($2, $3))`

// accountListSQL is every account row's id, in either spelling.
//
// staged-data: MUST-NOT-GATE -- listing the accounts the totals count; a staged account's rows are its own all the same, and leaving it out would drop its total.
const accountListSQL = `SELECT DISTINCT id FROM "MemoryNodes" WHERE concept = $1`

const switchSQL = `SELECT enabled FROM account_stamping_storage LIMIT 1`

const progressSQL = `SELECT concept, assigned, kept, shared, conflicts, pass_started_at, pass_finished_at, verified_through
  FROM account_assignment_progress
 ORDER BY concept`

const conflictsSQL = `SELECT concept, row_id, kind, detail, found_at
  FROM account_assignment_conflict
 WHERE resolved_at IS NULL
 ORDER BY found_at DESC, concept, row_id, kind
 LIMIT $1`

// normalizeAccountId returns the bare id (memql.BareAccountID), naming the
// builtin when the id is empty.
func normalizeAccountId(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", errors.New("accountInventory needs an account: accountId is empty")
	}
	return memql.BareAccountID(raw)
}

func (i *Integration) handleInventory(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	raw, _ := args["accountId"].(string)
	bare, err := normalizeAccountId(raw)
	if err != nil {
		return nil, err
	}
	db, err := i.database()
	if err != nil {
		return nil, err
	}
	canonical := canonicalPrefix + bare
	if bare != primaryAccount {
		var exists bool
		if err := db.DB.QueryRowContext(ctx, accountExistsSQL, accountConcept, canonical, bare).Scan(&exists); err != nil {
			return nil, fmt.Errorf("accountInventory: look up the account: %w", err)
		}
		if !exists {
			return nil, fmt.Errorf("there is no account %q", bare)
		}
	}

	countCtx, cancel := context.WithTimeout(ctx, i.inventoryDeadline)
	defer cancel()
	// tooLarge turns a passed deadline into the panel's sentence. It asks the
	// context rather than the error, because a cancelled statement surfaces as
	// a driver error or a closed connection depending on where it was; a
	// caller who went away keeps the plain error.
	tooLarge := func(err error, what string) error {
		if countCtx.Err() != nil && ctx.Err() == nil {
			return errors.New(inventoryTooLarge)
		}
		return fmt.Errorf("accountInventory: %s: %w", what, err)
	}
	// One REPEATABLE READ, READ ONLY transaction: the reader's plan settings
	// are transaction-local and hold for its statements
	// (memql.AccountRowCounts), and the probe, the count and the file sum all
	// read the one snapshot taken at its first statement.
	tx, err := db.DB.BeginTx(countCtx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, tooLarge(err, "begin")
	}
	defer func() { _ = tx.Rollback() }()
	counts, err := memql.AccountRowCounts(countCtx, tx, bare, memql.AccountKinds(i.conceptList(), false))
	if err != nil {
		return nil, tooLarge(err, "count rows")
	}
	type line struct {
		concept, namespace, name string
		rows, versions           int64
	}
	lines := make([]line, 0, len(counts))
	for _, c := range counts {
		l := line{concept: c.Concept, rows: c.Rows, versions: c.Versions}
		l.namespace, l.name = splitConcept(l.concept)
		lines = append(lines, l)
	}
	sort.Slice(lines, func(a, b int) bool {
		if lines[a].namespace != lines[b].namespace {
			return lines[a].namespace < lines[b].namespace
		}
		if lines[a].name != lines[b].name {
			return lines[a].name < lines[b].name
		}
		return lines[a].concept < lines[b].concept
	})

	var bytes int64
	if err := tx.QueryRowContext(countCtx, inventoryFilesSQL, bare, canonical, fileConcept).Scan(&bytes); err != nil {
		return nil, tooLarge(err, "sum file sizes")
	}

	out := make([]memorynodes.MemoryNode, 0, len(lines)+1)
	for _, l := range lines {
		n, err := node("concept:"+l.concept, map[string]any{
			"kind": "concept", "concept": l.concept, "namespace": l.namespace, "name": l.name,
			"rows": l.rows, "versions": l.versions,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	n, err := node("files", map[string]any{"kind": "files", "bytes": bytes})
	if err != nil {
		return nil, err
	}
	return append(out, n), nil
}

func (i *Integration) handleAssignmentStatus(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	db, err := i.database()
	if err != nil {
		return nil, err
	}
	var out []memorynodes.MemoryNode
	add := func(suffix string, payload map[string]any) error {
		n, err := node(suffix, payload)
		if err != nil {
			return err
		}
		out = append(out, n)
		return nil
	}

	// The cluster's switch, from its table and not this node's flag: a node
	// reports what the cluster decided, and `held` says separately whether
	// this node follows it. No row reads as off, which is what a node of this
	// release does with it too.
	var enabled bool
	if err := db.DB.QueryRowContext(ctx, switchSQL).Scan(&enabled); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("accountAssignmentStatus: read the switch: %w", err)
	}
	if err := add("switch", map[string]any{"kind": "switch", "enabled": enabled, "held": i.held}); err != nil {
		return nil, err
	}

	progress, err := db.DB.QueryContext(ctx, progressSQL)
	if err != nil {
		return nil, fmt.Errorf("accountAssignmentStatus: read progress: %w", err)
	}
	for progress.Next() {
		var concept string
		var assigned, kept, shared, conflicts int64
		var started, finished, verified sql.NullTime
		if err := progress.Scan(&concept, &assigned, &kept, &shared, &conflicts, &started, &finished, &verified); err != nil {
			progress.Close()
			return nil, fmt.Errorf("accountAssignmentStatus: read progress: %w", err)
		}
		payload := map[string]any{
			"kind": "progress", "concept": concept,
			"assigned": assigned, "kept": kept, "shared": shared, "conflicts": conflicts,
		}
		// An absent time is "not yet", left out rather than sent as a zero
		// time a client would print as a date.
		setTime(payload, "passStartedAt", started)
		setTime(payload, "passFinishedAt", finished)
		setTime(payload, "verifiedThrough", verified)
		if err := add("progress:"+concept, payload); err != nil {
			progress.Close()
			return nil, err
		}
	}
	if err := progress.Err(); err != nil {
		progress.Close()
		return nil, fmt.Errorf("accountAssignmentStatus: read progress: %w", err)
	}
	progress.Close()

	conflictRows, err := readConflicts(ctx, db)
	if err != nil {
		return nil, err
	}

	// The totals are read last and only behind the switch. Before it nothing
	// has been assigned, and the panel draws no totals. They are the one read
	// here over the node table, under a deadline of their own, and a failure
	// answers without them (the panel reads absent account rows as "totals
	// not available") rather than failing a status whose other rows came from
	// tiny tables.
	if enabled {
		accountRows, err := i.accountTotals(ctx, db)
		if err != nil {
			if i.logger != nil {
				i.logger.Warn("accountAssignmentStatus answered without the per-account totals", "error", err)
			}
		} else {
			out = append(out, accountRows...)
		}
	}
	return append(out, conflictRows...), nil
}

// readConflicts is one {kind: "conflict"} row per unresolved conflict, newest
// first, at most conflictLimit.
func readConflicts(ctx context.Context, db *bun.DB) ([]memorynodes.MemoryNode, error) {
	conflicts, err := db.DB.QueryContext(ctx, conflictsSQL, conflictLimit)
	if err != nil {
		return nil, fmt.Errorf("accountAssignmentStatus: read conflicts: %w", err)
	}
	defer conflicts.Close()
	var out []memorynodes.MemoryNode
	for conflicts.Next() {
		var concept, rowId, kind, detail string
		var found time.Time
		if err := conflicts.Scan(&concept, &rowId, &kind, &detail, &found); err != nil {
			return nil, fmt.Errorf("accountAssignmentStatus: read conflicts: %w", err)
		}
		n, err := node("conflict:"+concept+":"+rowId+":"+kind, map[string]any{
			"kind": "conflict", "concept": concept, "rowId": rowId, "conflict": kind,
			"detail": detail, "foundAt": found.UTC().Format(time.RFC3339Nano),
		})
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	if err := conflicts.Err(); err != nil {
		return nil, fmt.Errorf("accountAssignmentStatus: read conflicts: %w", err)
	}
	return out, nil
}

// accountTotals is one {kind: "account"} row per account holding rows of a
// concept that declares an account form, sorted by account id, read under
// totalsDeadline.
//
// Each account is counted with memql.AccountRowCounts, the rule decided
// from narrow columns, once per account that exists (the rows of
// v1:accounts:account, plus the primary account, which is an account before
// its row is written). One statement for every account at once would have to
// read each version's account, and only the payload holds it: on a bench with
// payloads of installation size (about 3 KB, stored out of line) that
// statement took 6.2 s, past this deadline, because evaluating the account on
// a version fetches and unpacks its whole payload (the design record,
// "Measured on a test table"). Per
// account, the account is matched by the account index instead. The loop stops
// at the deadline, and the status then answers without totals, as it does for
// any failure here.
//
// The totals list the accounts that have an account row, plus the primary
// account. A row naming an id that is no account's (no v1:accounts:account row
// carries it) is in no total; the rows of each listed account are counted
// exactly as its inventory counts them.
//
// Each account costs five round trips (the plan settings read and set, the
// probe that chooses the statement's shape, the count, the settings put back).
// Measured on the same test table: with its 22 accounts the loop does not fit
// the deadline, and the status shows no totals there.
func (i *Integration) accountTotals(ctx context.Context, db *bun.DB) ([]memorynodes.MemoryNode, error) {
	ctx, cancel := context.WithTimeout(ctx, i.totalsDeadline)
	defer cancel()
	kinds := memql.AccountKinds(i.conceptList(), false)
	// One REPEATABLE READ, READ ONLY transaction: the reader's plan settings
	// hold for its statements, and every account is counted in one snapshot.
	tx, err := db.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	accounts, err := listAccounts(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	out := make([]memorynodes.MemoryNode, 0, len(accounts))
	for _, a := range accounts {
		counts, err := memql.AccountRowCounts(ctx, tx, a, kinds)
		if err != nil {
			return nil, fmt.Errorf("count rows of account %q: %w", a, err)
		}
		// Ids are unique per concept (the concept is part of the id), so
		// summing each concept's count is the account's row count.
		var rows int64
		for _, c := range counts {
			rows += c.Rows
		}
		if rows == 0 {
			continue
		}
		n, err := node("account:"+a, map[string]any{"kind": "account", "accountId": a, "rows": rows})
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

// listAccounts is the bare id of every account row and of the primary
// account, sorted.
func listAccounts(ctx context.Context, q memql.AccountQuerier) ([]string, error) {
	rows, err := q.QueryContext(ctx, accountListSQL, accountConcept)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]bool{primaryAccount: true}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if bare, err := memql.BareAccountID(id); err == nil {
			seen[bare] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(seen))
	for a := range seen {
		out = append(out, a)
	}
	sort.Strings(out)
	return out, nil
}

func setTime(payload map[string]any, key string, t sql.NullTime) {
	if t.Valid {
		payload[key] = t.Time.UTC().Format(time.RFC3339Nano)
	}
}

// node wraps one answer row. The id is unique within one answer, because the
// engine folds a builtin's rows into an id-keyed map.
func node(suffix string, payload map[string]any) (memorynodes.MemoryNode, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return memorynodes.MemoryNode{}, fmt.Errorf("accounts: marshal result: %w", err)
	}
	return memorynodes.MemoryNode{
		ID:        "accounts:" + suffix,
		Concept:   resultConcept,
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   encoded,
	}, nil
}
