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
// rows; a count of distinct ids and of versions per concept, for every concept
// at once, is the one statement below over the partial index
// idx_memorynodes_account. Each statement repeats that index's predicate so the
// planner can use it, and none depends on it: without the index the same
// statement is a sequential scan with the same answer.
//
// # What counts
//
// Only concepts that DECLARE an account form (`@belongsTo(account ...)`) are
// counted. A cluster-wide concept may carry a field that happens to be called
// accountId (a mirrored vendor's own account number, say); its rows are not
// the account's, and counting them would put foreign rows in the ledger.
//
// Readers match both spellings of an account id, bare and
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
	// tens of millions of rows; `versions` comes from the partial index alone
	// but `rows` (distinct ids) visits the heap, so an account holding most of
	// a large cluster can take longer than one read may. A deadline of its own,
	// shorter than the pool's, lets the handler answer in words the panel can
	// show instead of a raw timeout. No second index: the export piece brings
	// a narrower read.
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

// accountScopedConcepts is the set of concept ids whose rows belong to an
// account, read from the registry on every call so a concept mounted or
// reloaded since boot is counted.
func (i *Integration) accountScopedConcepts() map[string]bool {
	out := map[string]bool{}
	if i.concepts == nil {
		return out
	}
	for _, c := range i.concepts() {
		if c != nil && c.BelongsTo != nil && c.BelongsTo.AccountScoped() {
			out[c.Name] = true
		}
	}
	return out
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

// inventoryCountsSQL counts, per concept, the distinct rows and the versions
// that name the account in either spelling.
//
// staged-data: MUST-NOT-GATE -- an inventory of what an account holds; a staged concept's rows are the account's all the same, and leaving them out would report an account smaller than it is (the export that follows this piece reads the same ledger).
const inventoryCountsSQL = `SELECT concept, count(DISTINCT id), count(*)
  FROM "MemoryNodes"
 WHERE ` + accountIdPresentSQL + `
   AND (payload->>'accountId') IN ($1, $2)
 GROUP BY concept`

// inventoryFilesSQL sums the size of the account's current files: the newest
// version of each file row that ever named the account, when that version
// still names it and is not archived. Archived files are left out because an
// archive is the Library's delete; a version whose size is not a JSON number
// contributes nothing rather than failing the read.
//
// staged-data: MUST-NOT-GATE -- the inventoryCountsSQL reason: the bytes are the account's whether or not the file concept's data is staged.
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

// accountTotalsSQL counts the distinct rows per (bare account, concept) over
// every row that names an account. Grouped by the BARE id so a row whose
// versions spell the account both ways counts once.
//
// staged-data: MUST-NOT-GATE -- the inventoryCountsSQL reason, for every account at once.
const accountTotalsSQL = `SELECT CASE WHEN (payload->>'accountId') LIKE 'v1:accounts:account:%'
            THEN substr(payload->>'accountId', 21) ELSE payload->>'accountId' END AS account,
       concept, count(DISTINCT id)
  FROM "MemoryNodes"
 WHERE ` + accountIdPresentSQL + `
 GROUP BY 1, 2`

const switchSQL = `SELECT enabled FROM account_stamping_storage LIMIT 1`

const progressSQL = `SELECT concept, assigned, kept, shared, conflicts, pass_started_at, pass_finished_at, verified_through
  FROM account_assignment_progress
 ORDER BY concept`

const conflictsSQL = `SELECT concept, row_id, kind, detail, found_at
  FROM account_assignment_conflict
 WHERE resolved_at IS NULL
 ORDER BY found_at DESC, concept, row_id, kind
 LIMIT $1`

// normalizeAccountId returns the bare id, refusing an empty one and one that
// is canonical for some other concept.
func normalizeAccountId(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("accountInventory needs an account: accountId is empty")
	}
	if strings.Contains(raw, ":") && !strings.HasPrefix(raw, canonicalPrefix) {
		return "", fmt.Errorf("%q is not an account id", raw)
	}
	bare := memql.BareShortId(strings.TrimPrefix(raw, canonicalPrefix))
	if bare == "" || strings.Contains(bare, ":") {
		return "", fmt.Errorf("%q is not an account id", raw)
	}
	return bare, nil
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

	scoped := i.accountScopedConcepts()
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
	rows, err := db.DB.QueryContext(countCtx, inventoryCountsSQL, bare, canonical)
	if err != nil {
		return nil, tooLarge(err, "count rows")
	}
	type line struct {
		concept, namespace, name string
		rows, versions           int64
	}
	var lines []line
	for rows.Next() {
		var l line
		if err := rows.Scan(&l.concept, &l.rows, &l.versions); err != nil {
			rows.Close()
			return nil, tooLarge(err, "read counts")
		}
		if !scoped[l.concept] {
			continue
		}
		l.namespace, l.name = splitConcept(l.concept)
		lines = append(lines, l)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, tooLarge(err, "read counts")
	}
	rows.Close()
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
	if err := db.DB.QueryRowContext(countCtx, inventoryFilesSQL, bare, canonical, fileConcept).Scan(&bytes); err != nil {
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
func (i *Integration) accountTotals(ctx context.Context, db *bun.DB) ([]memorynodes.MemoryNode, error) {
	ctx, cancel := context.WithTimeout(ctx, i.totalsDeadline)
	defer cancel()
	scoped := i.accountScopedConcepts()
	totals, err := db.DB.QueryContext(ctx, accountTotalsSQL)
	if err != nil {
		return nil, fmt.Errorf("count rows per account: %w", err)
	}
	defer totals.Close()
	perAccount := map[string]int64{}
	for totals.Next() {
		var account, concept string
		var count int64
		if err := totals.Scan(&account, &concept, &count); err != nil {
			return nil, fmt.Errorf("count rows per account: %w", err)
		}
		if !scoped[concept] || strings.TrimSpace(account) == "" {
			continue
		}
		// Ids are unique per concept (the concept is part of the id), so
		// summing each concept's distinct count is the account's distinct
		// row count.
		perAccount[account] += count
	}
	if err := totals.Err(); err != nil {
		return nil, fmt.Errorf("count rows per account: %w", err)
	}
	accounts := make([]string, 0, len(perAccount))
	for a := range perAccount {
		accounts = append(accounts, a)
	}
	sort.Strings(accounts)
	out := make([]memorynodes.MemoryNode, 0, len(accounts))
	for _, a := range accounts {
		n, err := node("account:"+a, map[string]any{"kind": "account", "accountId": a, "rows": perAccount[a]})
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
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
