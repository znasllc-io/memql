package accounts

// accounts_db_test.go -- both builtins' handlers against a real Postgres.
//
// Every table the handlers read is a TEMP copy of the real one on a single
// connection, so the fixture is exactly what each test writes and nothing in
// the shared test database is touched.

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/parser"
)

// The fixture's concepts. Four declare an account form (one of them a concept
// whose newest version may be an observation in memory_liveness), one is cluster-wide
// with an unrelated field called accountId (a mirrored vendor's own account
// number), and one declares nothing.
var fixtureConcepts = []*memorynodes.Concept{
	{Name: "v1:testa:note", BelongsTo: &parser.BelongsToDecl{Scope: parser.BelongsToAccount}},
	{Name: "v1:testb:task", BelongsTo: &parser.BelongsToDecl{Scope: parser.BelongsToAccount, Via: "noteId"}},
	{Name: fileConcept, BelongsTo: &parser.BelongsToDecl{Scope: parser.BelongsToAccount}},
	{Name: runConcept, BelongsTo: &parser.BelongsToDecl{Scope: parser.BelongsToAccount}},
	{Name: "v1:testc:vendorChannel", BelongsTo: &parser.BelongsToDecl{Scope: parser.BelongsToCluster}},
	{Name: "v1:testc:undeclared"},
}

// runConcept is listed in memorynodes.HasLiveness.
const runConcept = "v1:pipelines:run"

func fixtureLister() []*memorynodes.Concept { return fixtureConcepts }

func openTempDB(t *testing.T) *bun.DB {
	t.Helper()
	ctx := context.Background()
	reachable, err := dbtest.EnsureSchema(ctx)
	require.NoError(t, err)
	if !reachable {
		dbtest.Unreachable(t, "accounts inventory", dbtest.DSN(), sql.ErrConnDone)
	}
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	// One connection, so every statement sees the temp tables below, which
	// shadow the public ones on this session's search path.
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	for _, table := range []string{`"MemoryNodes"`, "memory_liveness", "account_stamping_storage", "account_assignment_progress", "account_assignment_conflict"} {
		_, err := db.ExecContext(ctx, `CREATE TEMP TABLE `+table+` (LIKE public.`+table+` INCLUDING ALL)`)
		require.NoError(t, err, table)
	}
	return db
}

var fixtureClock = time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)

// version writes one version of a row; minutes orders the versions of one id.
func version(t *testing.T, db *bun.DB, concept, id string, minutes int, payload map[string]any) {
	t.Helper()
	versionIn(t, db, `"MemoryNodes"`, concept, id, minutes, payload)
}

// versionIn writes one version to the given table: "MemoryNodes", or
// memory_liveness for a row's current observation (the temp copy has no
// trigger, so it is written there directly).
func versionIn(t *testing.T, db *bun.DB, table, concept, id string, minutes int, payload map[string]any) {
	t.Helper()
	encoded, err := json.Marshal(payload)
	require.NoError(t, err)
	_, err = db.DB.ExecContext(context.Background(),
		`INSERT INTO `+table+` (id, "createdAt", "createdBy", schema, payload, concept) VALUES ($1, $2, 'test', '{}', $3, $4)`,
		id, fixtureClock.Add(time.Duration(minutes)*time.Minute), string(encoded), concept)
	require.NoError(t, err)
}

const (
	acmeCanonical   = "v1:accounts:account:acme"
	globexCanonical = "v1:accounts:account:globex"
)

// seedTwoAccounts writes the fixture both builtins read. The expected counts
// are worked out beside each row.
func seedTwoAccounts(t *testing.T, db *bun.DB) {
	t.Helper()
	version(t, db, accountConcept, acmeCanonical, 0, map[string]any{"name": "Acme"})
	version(t, db, accountConcept, globexCanonical, 0, map[string]any{"name": "Globex"})

	// acme notes: 3 rows, 5 versions. n3's versions spell the id both ways
	// and still count as one row.
	version(t, db, "v1:testa:note", "v1:testa:note:n1", 1, map[string]any{"accountId": "acme"})
	version(t, db, "v1:testa:note", "v1:testa:note:n1", 2, map[string]any{"accountId": "acme"})
	version(t, db, "v1:testa:note", "v1:testa:note:n2", 1, map[string]any{"accountId": acmeCanonical})
	version(t, db, "v1:testa:note", "v1:testa:note:n3", 1, map[string]any{"accountId": "acme"})
	version(t, db, "v1:testa:note", "v1:testa:note:n3", 2, map[string]any{"accountId": acmeCanonical})
	// No account: empty and absent are both "none".
	version(t, db, "v1:testa:note", "v1:testa:note:n8", 1, map[string]any{"accountId": ""})
	version(t, db, "v1:testa:note", "v1:testa:note:n9", 1, map[string]any{"title": "unassigned"})
	// acme task: 1 row, 1 version.
	version(t, db, "v1:testb:task", "v1:testb:task:t1", 1, map[string]any{"accountId": "acme"})

	// acme files: f1, f2, f3 (3 rows, 4 versions); f4 named acme once and its
	// newest version names globex, so it is globex's. Bytes count the CURRENT
	// version only: f1 100, f2's newest 50, f3 archived -- 150.
	version(t, db, fileConcept, "v1:library:file:f1", 1, map[string]any{"accountId": "acme", "size": 100})
	version(t, db, fileConcept, "v1:library:file:f2", 1, map[string]any{"accountId": acmeCanonical, "size": 40})
	version(t, db, fileConcept, "v1:library:file:f2", 2, map[string]any{"accountId": acmeCanonical, "size": 50})
	version(t, db, fileConcept, "v1:library:file:f3", 1, map[string]any{"accountId": "acme", "size": 1000, "archived": true})
	version(t, db, fileConcept, "v1:library:file:f4", 1, map[string]any{"accountId": "acme", "size": 7})
	version(t, db, fileConcept, "v1:library:file:f4", 2, map[string]any{"accountId": "globex", "size": 7})

	// Fields called accountId on concepts that do not declare an account
	// form: never the account's.
	version(t, db, "v1:testc:vendorChannel", "v1:testc:vendorChannel:c1", 1, map[string]any{"accountId": "acme"})
	version(t, db, "v1:testc:undeclared", "v1:testc:undeclared:u1", 1, map[string]any{"accountId": "acme"})

	// globex: 1 note; files f4 (above, both versions) and f5 -- 2 rows, 3
	// versions, 27 bytes.
	version(t, db, "v1:testa:note", "v1:testa:note:g1", 1, map[string]any{"accountId": globexCanonical})
	version(t, db, fileConcept, "v1:library:file:f5", 1, map[string]any{"accountId": "globex", "size": 20})
}

func payloads(t *testing.T, nodes []memorynodes.MemoryNode) []map[string]any {
	t.Helper()
	out := make([]map[string]any, 0, len(nodes))
	seen := map[string]bool{}
	for _, n := range nodes {
		require.Equal(t, resultConcept, n.Concept)
		require.False(t, seen[n.ID], "duplicate answer id %s: the engine folds rows by id", n.ID)
		seen[n.ID] = true
		var p map[string]any
		require.NoError(t, json.Unmarshal(n.Payload, &p))
		out = append(out, p)
	}
	return out
}

func conceptLine(concept, namespace, name string, rows, versions float64) map[string]any {
	return map[string]any{"kind": "concept", "concept": concept, "namespace": namespace, "name": name, "rows": rows, "versions": versions}
}

func TestAccountInventoryCountsOnlyTheAccountsDeclaredConcepts(t *testing.T) {
	db := openTempDB(t)
	seedTwoAccounts(t, db)
	i := New(func() *bun.DB { return db }, fixtureLister, false, nil)
	ctx := context.Background()

	acme := []map[string]any{
		conceptLine(fileConcept, "library", "file", 3, 4),
		conceptLine("v1:testa:note", "testa", "note", 3, 5),
		conceptLine("v1:testb:task", "testb", "task", 1, 1),
		{"kind": "files", "bytes": float64(150)},
	}
	for _, spelling := range []string{"acme", acmeCanonical, "  acme  "} {
		nodes, err := i.handleInventory(ctx, map[string]any{"accountId": spelling}, 0)
		require.NoError(t, err, spelling)
		require.Equal(t, acme, payloads(t, nodes), "inventory of %q", spelling)
	}

	nodes, err := i.handleInventory(ctx, map[string]any{"accountId": "globex"}, 0)
	require.NoError(t, err)
	require.Equal(t, []map[string]any{
		conceptLine(fileConcept, "library", "file", 2, 3),
		conceptLine("v1:testa:note", "testa", "note", 1, 1),
		{"kind": "files", "bytes": float64(27)},
	}, payloads(t, nodes), "globex's inventory must hold none of acme's rows")

	// Never cached: a row written after the first read is in the next one.
	version(t, db, "v1:testb:task", "v1:testb:task:t2", 1, map[string]any{"accountId": "globex"})
	nodes, err = i.handleInventory(ctx, map[string]any{"accountId": "globex"}, 0)
	require.NoError(t, err)
	require.Contains(t, payloads(t, nodes), conceptLine("v1:testb:task", "testb", "task", 1, 1))

	// The primary account is an account before its row is written.
	nodes, err = i.handleInventory(ctx, map[string]any{"accountId": "self"}, 0)
	require.NoError(t, err)
	require.Equal(t, []map[string]any{{"kind": "files", "bytes": float64(0)}}, payloads(t, nodes))
}

func TestAccountInventoryCountsARowUnderTheAccountItsNewestVersionNames(t *testing.T) {
	db := openTempDB(t)
	seedTwoAccounts(t, db)
	i := New(func() *bun.DB { return db }, fixtureLister, false, nil)
	ctx := context.Background()

	// t5 moved from acme to globex: globex's, with both versions.
	version(t, db, "v1:testb:task", "v1:testb:task:t5", 1, map[string]any{"accountId": "acme"})
	version(t, db, "v1:testb:task", "v1:testb:task:t5", 2, map[string]any{"accountId": globexCanonical})
	// r1's versioned row names no account; its newest version is an
	// observation naming globex.
	version(t, db, runConcept, runConcept+":r1", 1, map[string]any{"state": "running"})
	versionIn(t, db, "memory_liveness", runConcept, runConcept+":r1", 2, map[string]any{"state": "running", "accountId": "globex"})
	// n10's newest version names no account after one that named acme. The
	// export leaves it out (it is in no account); the inventory counts by the
	// counting rule (memql.AccountRowCounts) and still counts it under acme,
	// with its one version that names an account.
	version(t, db, "v1:testa:note", "v1:testa:note:n10", 1, map[string]any{"accountId": "acme"})
	version(t, db, "v1:testa:note", "v1:testa:note:n10", 2, map[string]any{"title": "cleared"})

	nodes, err := i.handleInventory(ctx, map[string]any{"accountId": "acme"}, 0)
	require.NoError(t, err)
	got := payloads(t, nodes)
	require.Contains(t, got, conceptLine("v1:testb:task", "testb", "task", 1, 1), "only t1 is acme's")
	require.Contains(t, got, conceptLine("v1:testa:note", "testa", "note", 4, 6), "n1-n3 and n10")
	for _, p := range got {
		require.NotEqual(t, runConcept, p["concept"], "the run is globex's: %v", p)
	}

	nodes, err = i.handleInventory(ctx, map[string]any{"accountId": "globex"}, 0)
	require.NoError(t, err)
	require.Equal(t, []map[string]any{
		conceptLine(fileConcept, "library", "file", 2, 3),
		// versions counts the versions that name an account: r1's
		// observation, not its versioned row that names none.
		conceptLine(runConcept, "pipelines", "run", 1, 1),
		conceptLine("v1:testa:note", "testa", "note", 1, 1),
		conceptLine("v1:testb:task", "testb", "task", 1, 2),
		{"kind": "files", "bytes": float64(27)},
	}, payloads(t, nodes))
}

func TestAccountInventoryRefusesAnEmptyOrUnknownAccount(t *testing.T) {
	db := openTempDB(t)
	seedTwoAccounts(t, db)
	i := New(func() *bun.DB { return db }, fixtureLister, false, nil)
	for raw, want := range map[string]string{
		"":                     "accountId is empty",
		"   ":                  "accountId is empty",
		"initech":              `there is no account "initech"`,
		"v1:accounts:account:": "not an account id",
		"v1:library:file:f1":   "not an account id",
	} {
		_, err := i.handleInventory(context.Background(), map[string]any{"accountId": raw}, 0)
		require.Error(t, err, "%q", raw)
		require.True(t, strings.Contains(err.Error(), want), "%q refused with %q, want %q", raw, err, want)
	}
	_, err := i.handleInventory(context.Background(), map[string]any{}, 0)
	require.ErrorContains(t, err, "accountId is empty")
}

func TestAccountAssignmentStatusOnEmptyTablesIsTheSwitchAlone(t *testing.T) {
	db := openTempDB(t)
	ctx := context.Background()
	// No switch row at all reads as off, which is what the engine does too.
	nodes, err := New(func() *bun.DB { return db }, fixtureLister, false, nil).handleAssignmentStatus(ctx, nil, 0)
	require.NoError(t, err)
	require.Equal(t, []map[string]any{{"kind": "switch", "enabled": false, "held": false}}, payloads(t, nodes))

	_, err = db.DB.ExecContext(ctx, `INSERT INTO account_stamping_storage DEFAULT VALUES`)
	require.NoError(t, err)
	nodes, err = New(func() *bun.DB { return db }, fixtureLister, true, nil).handleAssignmentStatus(ctx, nil, 0)
	require.NoError(t, err)
	require.Equal(t, []map[string]any{{"kind": "switch", "enabled": false, "held": true}}, payloads(t, nodes))
}

func TestAccountAssignmentStatusReportsProgressTotalsAndOpenConflicts(t *testing.T) {
	db := openTempDB(t)
	seedTwoAccounts(t, db)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		_, err := db.DB.ExecContext(ctx, q, args...)
		require.NoError(t, err)
	}
	// The switch is on in the cluster's table; this node's own flag is not
	// consulted.
	exec(`INSERT INTO account_stamping_storage (enabled) VALUES (true)`)
	started := fixtureClock.Add(time.Hour)
	finished := started.Add(time.Minute)
	exec(`INSERT INTO account_assignment_progress (concept, assigned, kept, shared, conflicts, pass_started_at, pass_finished_at, verified_through)
	      VALUES ('v1:testb:task', 5, 1, 0, 2, $1, $2, $2)`, started, finished)
	exec(`INSERT INTO account_assignment_progress (concept, assigned, pass_started_at) VALUES ('v1:testa:note', 3, $1)`, started)
	exec(`INSERT INTO account_assignment_conflict (concept, row_id, kind, detail, found_at) VALUES ('v1:testb:task', 'v1:testb:task:t9', 'parent_disagrees', 'parent names globex', $1)`, started)
	exec(`INSERT INTO account_assignment_conflict (concept, row_id, kind, detail, found_at) VALUES ('v1:testa:note', 'v1:testa:note:n7', 'several_accounts', 'acme and globex', $1)`, finished)
	exec(`INSERT INTO account_assignment_conflict (concept, row_id, kind, found_at, resolved_at) VALUES ('v1:testa:note', 'v1:testa:note:n6', 'several_accounts', $1, $1)`, finished)

	nodes, err := New(func() *bun.DB { return db }, fixtureLister, false, nil).handleAssignmentStatus(ctx, nil, 0)
	require.NoError(t, err)
	at := func(ts time.Time) string { return ts.UTC().Format(time.RFC3339Nano) }
	require.Equal(t, []map[string]any{
		{"kind": "switch", "enabled": true, "held": false},
		// An unfinished pass carries no finish and no verified point.
		{"kind": "progress", "concept": "v1:testa:note", "assigned": float64(3), "kept": float64(0), "shared": float64(0), "conflicts": float64(0),
			"passStartedAt": at(started)},
		{"kind": "progress", "concept": "v1:testb:task", "assigned": float64(5), "kept": float64(1), "shared": float64(0), "conflicts": float64(2),
			"passStartedAt": at(started), "passFinishedAt": at(finished), "verifiedThrough": at(finished)},
		// acme: notes 3 + task 1 + files 3; globex: note 1 + files 2 (f4
		// moved to globex counts once, under globex). The cluster-wide and
		// undeclared concepts add nothing.
		{"kind": "account", "accountId": "acme", "rows": float64(7)},
		{"kind": "account", "accountId": "globex", "rows": float64(3)},
		// Unresolved only, newest first.
		{"kind": "conflict", "concept": "v1:testa:note", "rowId": "v1:testa:note:n7", "conflict": "several_accounts", "detail": "acme and globex", "foundAt": at(finished)},
		{"kind": "conflict", "concept": "v1:testb:task", "rowId": "v1:testb:task:t9", "conflict": "parent_disagrees", "detail": "parent names globex", "foundAt": at(started)},
	}, payloads(t, nodes))

	// Totals that do not arrive in time are left out; the rest of the status
	// still answers.
	slow := New(func() *bun.DB { return db }, fixtureLister, false, nil)
	slow.totalsDeadline = time.Nanosecond
	nodes, err = slow.handleAssignmentStatus(ctx, nil, 0)
	require.NoError(t, err)
	got := payloads(t, nodes)
	require.Len(t, got, 5, "switch, two progress rows and two conflicts, no account rows: %v", got)
	for _, p := range got {
		require.NotEqual(t, "account", p["kind"], "a timed-out total reached the answer: %v", p)
	}
}

func TestAccountAssignmentStatusTotalsCountARowUnderItsNewestAccount(t *testing.T) {
	db := openTempDB(t)
	seedTwoAccounts(t, db)
	ctx := context.Background()
	_, err := db.DB.ExecContext(ctx, `INSERT INTO account_stamping_storage (enabled) VALUES (true)`)
	require.NoError(t, err)
	// t5's older version names acme and its newest globex (canonically): it
	// counts once, under globex. So does f4 in the seed.
	version(t, db, "v1:testb:task", "v1:testb:task:t5", 1, map[string]any{"accountId": "acme"})
	version(t, db, "v1:testb:task", "v1:testb:task:t5", 2, map[string]any{"accountId": globexCanonical})
	// r1's versioned row names acme; its newer observation names globex.
	version(t, db, runConcept, runConcept+":r1", 1, map[string]any{"accountId": "acme"})
	versionIn(t, db, "memory_liveness", runConcept, runConcept+":r1", 2, map[string]any{"accountId": "globex"})
	// r2's versioned row names no account; its observation names acme.
	version(t, db, runConcept, runConcept+":r2", 1, map[string]any{"state": "running"})
	versionIn(t, db, "memory_liveness", runConcept, runConcept+":r2", 2, map[string]any{"accountId": "acme"})

	nodes, err := New(func() *bun.DB { return db }, fixtureLister, false, nil).handleAssignmentStatus(ctx, nil, 0)
	require.NoError(t, err)
	var accounts []map[string]any
	for _, p := range payloads(t, nodes) {
		if p["kind"] == "account" {
			accounts = append(accounts, p)
		}
	}
	// acme: notes n1-n3, task t1, files f1-f3, run r2 -- 8.
	// globex: note g1, files f4 and f5, task t5, run r1 -- 5.
	require.Equal(t, []map[string]any{
		{"kind": "account", "accountId": "acme", "rows": float64(8)},
		{"kind": "account", "accountId": "globex", "rows": float64(5)},
	}, accounts)
}

func TestAccountAssignmentStatusShowsNoTotalsBeforeTheSwitch(t *testing.T) {
	db := openTempDB(t)
	// Rows that carry an account already (the organization rules stamp some
	// before the switch) are not totals the panel shows while it is off.
	seedTwoAccounts(t, db)
	ctx := context.Background()
	_, err := db.DB.ExecContext(ctx, `INSERT INTO account_stamping_storage (enabled) VALUES (false)`)
	require.NoError(t, err)
	nodes, err := New(func() *bun.DB { return db }, fixtureLister, false, nil).handleAssignmentStatus(ctx, nil, 0)
	require.NoError(t, err)
	require.Equal(t, []map[string]any{{"kind": "switch", "enabled": false, "held": false}}, payloads(t, nodes))
}

func TestAccountInventoryPastItsDeadlineRefusesInWords(t *testing.T) {
	db := openTempDB(t)
	seedTwoAccounts(t, db)
	i := New(func() *bun.DB { return db }, fixtureLister, false, nil)
	i.inventoryDeadline = time.Nanosecond
	_, err := i.handleInventory(context.Background(), map[string]any{"accountId": "acme"}, 0)
	require.EqualError(t, err, inventoryTooLarge)
}
