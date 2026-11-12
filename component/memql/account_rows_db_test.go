package memql

// account_rows_db_test.go -- the account reader (account_rows.go) against a
// real Postgres.
//
// Every table the reader reads is a TEMP copy of the real one on a single
// connection, so the fixture is exactly what each test writes, versions carry
// the createdAt the test chose, and nothing in the shared database is touched.
// The copies carry no triggers, so a row written to memory_liveness here is
// written there directly, as the liveness trigger would have left it.

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

const (
	rowsNote = "v1:aerows:note"
	// rowsRun is one of the concepts whose newest version may be an
	// observation in memory_liveness (memorynodes.HasLiveness).
	rowsRun       = "v1:pipelines:run"
	rowsAcme      = "acme"
	rowsAcmeCanon = "v1:accounts:account:acme"
	rowsBeta      = "beta"
	rowsEmptyKind = "v1:aerows:empty"
	// rowsLoose holds a row in no account and a row of beta, none of acme's.
	rowsLoose      = "v1:aerows:loose"
	rowsLooseL1    = rowsLoose + ":l1"
	rowsLooseL2    = rowsLoose + ":l2"
	rowsNoteN1     = rowsNote + ":n1"
	rowsNoteN2     = rowsNote + ":n2"
	rowsNoteN3     = rowsNote + ":n3"
	rowsNoteN5     = rowsNote + ":n5"
	rowsNoteN6     = rowsNote + ":n6"
	rowsNoteN7     = rowsNote + ":n7"
	rowsNoteN8     = rowsNote + ":n8"
	rowsRunR1      = rowsRun + ":r1"
	rowsRunR2      = rowsRun + ":r2"
	rowsRunR3      = rowsRun + ":r3"
	rowsRunR4      = rowsRun + ":r4"
	rowsFixtureTag = "account-rows-test"
)

var rowsClock = time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)

func openAccountRowsDB(t *testing.T) *sql.DB {
	t.Helper()
	ctx := context.Background()
	reachable, err := dbtest.EnsureSchema(ctx)
	require.NoError(t, err)
	if !reachable {
		dbtest.Unreachable(t, "account rows reader", dbtest.DSN(), sql.ErrConnDone)
	}
	db := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN())))
	// One connection, so every statement sees the temp tables, which shadow
	// the public ones on this session's search path.
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	for _, table := range []string{`"MemoryNodes"`, "memory_liveness"} {
		_, err := db.ExecContext(ctx, `CREATE TEMP TABLE `+table+` (LIKE public.`+table+` INCLUDING ALL)`)
		require.NoError(t, err, table)
	}
	return db
}

// rowsVersion writes one version; minutes orders the versions of one id.
func rowsVersion(t *testing.T, db *sql.DB, table, concept, id string, minutes int, payload map[string]any) {
	t.Helper()
	encoded, err := json.Marshal(payload)
	require.NoError(t, err)
	_, err = db.ExecContext(context.Background(),
		`INSERT INTO `+table+` (id, "createdAt", "createdBy", type, schema, payload, concept, provenance) VALUES ($1, $2, $3, 'object', '{}', $4, $5, '{}')`,
		id, rowsClock.Add(time.Duration(minutes)*time.Minute), rowsFixtureTag, string(encoded), concept)
	require.NoError(t, err)
}

// seedAccountRows writes the fixture. Whose each row is, by its newest
// version, is beside it.
func seedAccountRows(t *testing.T, db *sql.DB) {
	t.Helper()
	nodes := `"MemoryNodes"`
	a := map[string]any{"accountId": rowsAcme}
	b := map[string]any{"accountId": rowsBeta}
	// n1: every version names acme -> acme's.
	rowsVersion(t, db, nodes, rowsNote, rowsNoteN1, 1, a)
	rowsVersion(t, db, nodes, rowsNote, rowsNoteN1, 2, a)
	// n2: older names beta, newest acme -> acme's, both versions carried.
	rowsVersion(t, db, nodes, rowsNote, rowsNoteN2, 1, b)
	rowsVersion(t, db, nodes, rowsNote, rowsNoteN2, 2, a)
	// n3: older names acme, newest beta -> beta's, never acme's.
	rowsVersion(t, db, nodes, rowsNote, rowsNoteN3, 1, a)
	rowsVersion(t, db, nodes, rowsNote, rowsNoteN3, 2, b)
	// n5: spelled canonically -> acme's.
	rowsVersion(t, db, nodes, rowsNote, rowsNoteN5, 1, map[string]any{"accountId": rowsAcmeCanon})
	// n6: older names acme, newest names none -> unplaced.
	rowsVersion(t, db, nodes, rowsNote, rowsNoteN6, 1, a)
	rowsVersion(t, db, nodes, rowsNote, rowsNoteN6, 2, map[string]any{"title": "cleared"})
	// n7: an empty string is no account -> unplaced.
	rowsVersion(t, db, nodes, rowsNote, rowsNoteN7, 1, map[string]any{"accountId": ""})
	// n8: no field at all -> unplaced.
	rowsVersion(t, db, nodes, rowsNote, rowsNoteN8, 1, map[string]any{"title": "none"})

	// r1: the versioned row names none, its newest version is an observation
	// naming acme -> acme's, two versions.
	rowsVersion(t, db, nodes, rowsRun, rowsRunR1, 1, map[string]any{"state": "running"})
	rowsVersion(t, db, "memory_liveness", rowsRun, rowsRunR1, 3, map[string]any{"state": "running", "accountId": rowsAcme})
	// r2: the versioned row names acme, the newer observation beta -> beta's.
	rowsVersion(t, db, nodes, rowsRun, rowsRunR2, 1, a)
	rowsVersion(t, db, "memory_liveness", rowsRun, rowsRunR2, 3, b)
	// r3: versioned only, acme -> acme's.
	rowsVersion(t, db, nodes, rowsRun, rowsRunR3, 1, a)
	// r4: an observation naming acme that is OLDER than the versioned row,
	// which names none -> unplaced. The trigger never leaves this shape, but
	// the reader decides by createdAt and not by table.
	rowsVersion(t, db, nodes, rowsRun, rowsRunR4, 5, map[string]any{"state": "done"})
	rowsVersion(t, db, "memory_liveness", rowsRun, rowsRunR4, 4, a)

	// l1: in no account; l2: beta's.
	rowsVersion(t, db, nodes, rowsLoose, rowsLooseL1, 1, map[string]any{"title": "loose"})
	rowsVersion(t, db, nodes, rowsLoose, rowsLooseL2, 1, b)
}

func TestAccountRowIDsAreTheRowsWhoseNewestVersionNamesTheAccount(t *testing.T) {
	db := openAccountRowsDB(t)
	seedAccountRows(t, db)
	ctx := context.Background()

	for _, spelling := range []string{rowsAcme, rowsAcmeCanon} {
		ids, err := AccountRowIDs(ctx, db, spelling, rowsNote, "", 100)
		require.NoError(t, err, spelling)
		require.Equal(t, []string{rowsNoteN1, rowsNoteN2, rowsNoteN5}, ids, "acme's notes, account spelled %q", spelling)
	}
	ids, err := AccountRowIDs(ctx, db, rowsBeta, rowsNote, "", 100)
	require.NoError(t, err)
	require.Equal(t, []string{rowsNoteN3}, ids, "a row moved to beta is beta's")

	ids, err = AccountRowIDs(ctx, db, rowsAcme, rowsRun, "", 100)
	require.NoError(t, err)
	require.Equal(t, []string{rowsRunR1, rowsRunR3}, ids, "the newest version is read across the row table and the observation store")
	ids, err = AccountRowIDs(ctx, db, rowsBeta, rowsRun, "", 100)
	require.NoError(t, err)
	require.Equal(t, []string{rowsRunR2}, ids)

	// Paging by afterID and limit walks the same ids in the same order.
	var walked []string
	after := ""
	for page := 0; page < 10; page++ {
		got, err := AccountRowIDs(ctx, db, rowsAcme, rowsNote, after, 2)
		require.NoError(t, err)
		require.LessOrEqual(t, len(got), 2)
		if len(got) == 0 {
			break
		}
		walked = append(walked, got...)
		after = got[len(got)-1]
	}
	require.Equal(t, []string{rowsNoteN1, rowsNoteN2, rowsNoteN5}, walked)

	// Any querier: a transaction reads the same.
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	ids, err = AccountRowIDs(ctx, tx, rowsAcme, rowsNote, rowsNoteN1, 100)
	require.NoError(t, err)
	require.Equal(t, []string{rowsNoteN2, rowsNoteN5}, ids)
	require.NoError(t, tx.Rollback())

	_, err = AccountRowIDs(ctx, db, "", rowsNote, "", 100)
	require.Error(t, err, "an empty account is refused, never read as \"rows with no account\"")
	_, err = AccountRowIDs(ctx, db, rowsAcme, rowsNote, "", 0)
	require.Error(t, err, "a limit must be positive")
}

func TestUnplacedRowIDsAreTheRowsWhoseNewestVersionNamesNoAccount(t *testing.T) {
	db := openAccountRowsDB(t)
	seedAccountRows(t, db)
	ctx := context.Background()

	ids, err := UnplacedRowIDs(ctx, db, rowsNote, "", 100)
	require.NoError(t, err)
	require.Equal(t, []string{rowsNoteN6, rowsNoteN7, rowsNoteN8}, ids)

	ids, err = UnplacedRowIDs(ctx, db, rowsNote, "", 2)
	require.NoError(t, err)
	require.Equal(t, []string{rowsNoteN6, rowsNoteN7}, ids)
	ids, err = UnplacedRowIDs(ctx, db, rowsNote, rowsNoteN7, 2)
	require.NoError(t, err)
	require.Equal(t, []string{rowsNoteN8}, ids)

	// r1's versioned row names none, but its newest version is an
	// observation that names acme; r4's newest is the versioned row.
	ids, err = UnplacedRowIDs(ctx, db, rowsRun, "", 100)
	require.NoError(t, err)
	require.Equal(t, []string{rowsRunR4}, ids)
}

// rowsVersionKey is one version as the reader handed it over.
type rowsVersionKey struct {
	id       string
	minutes  int
	account  string
	observed bool
}

func rowsCollect(t *testing.T, concept string, out *[]rowsVersionKey) func(memorynodes.MemoryNode, bool) error {
	return func(n memorynodes.MemoryNode, observed bool) error {
		require.Equal(t, concept, n.Concept)
		require.Equal(t, rowsFixtureTag, n.CreatedBy)
		require.NotEmpty(t, n.Provenance)
		var p struct {
			AccountID string `json:"accountId"`
		}
		require.NoError(t, json.Unmarshal(n.Payload, &p))
		*out = append(*out, rowsVersionKey{n.ID, int(n.CreatedAt.Sub(rowsClock) / time.Minute), p.AccountID, observed})
		return nil
	}
}

func TestAccountRowVersionsAreEveryVersionInIdThenTimeOrder(t *testing.T) {
	db := openAccountRowsDB(t)
	seedAccountRows(t, db)
	ctx := context.Background()
	read := func(concept string, ids []string) []rowsVersionKey {
		t.Helper()
		var out []rowsVersionKey
		require.NoError(t, AccountRowVersions(ctx, db, concept, ids, rowsCollect(t, concept, &out)))
		return out
	}

	// Given out of order; every version, including one naming another account.
	require.Equal(t, []rowsVersionKey{
		{rowsNoteN1, 1, rowsAcme, false}, {rowsNoteN1, 2, rowsAcme, false},
		{rowsNoteN2, 1, rowsBeta, false}, {rowsNoteN2, 2, rowsAcme, false},
	}, read(rowsNote, []string{rowsNoteN2, rowsNoteN1}))
	// The observation is a version of the row, the newest one, and says so.
	require.Equal(t, []rowsVersionKey{{rowsRunR1, 1, "", false}, {rowsRunR1, 3, rowsAcme, true}}, read(rowsRun, []string{rowsRunR1}))
	require.Empty(t, read(rowsNote, nil))
	// The concept bounds the read: an id of another kind is not read.
	require.Empty(t, read(rowsRun, []string{rowsNoteN1}))
}

// rowsWalk is what the page reader yields for an account and a kind: its
// ids, then every version of them.
func rowsWalk(t *testing.T, db *sql.DB, account, concept string) []rowsVersionKey {
	t.Helper()
	ctx := context.Background()
	ids, err := AccountRowIDs(ctx, db, account, concept, "", 1000)
	require.NoError(t, err)
	var out []rowsVersionKey
	require.NoError(t, AccountRowVersions(ctx, db, concept, ids, rowsCollect(t, concept, &out)))
	return out
}

func TestAccountKindVersionsYieldWhatTheWalkYields(t *testing.T) {
	db := openAccountRowsDB(t)
	seedAccountRows(t, db)
	ctx := context.Background()
	kind := func(account, concept string) []rowsVersionKey {
		t.Helper()
		var out []rowsVersionKey
		require.NoError(t, AccountKindVersions(ctx, db, account, concept, rowsCollect(t, concept, &out)))
		return out
	}

	require.Equal(t, []rowsVersionKey{
		{rowsNoteN1, 1, rowsAcme, false}, {rowsNoteN1, 2, rowsAcme, false},
		{rowsNoteN2, 1, rowsBeta, false}, {rowsNoteN2, 2, rowsAcme, false},
		{rowsNoteN5, 1, rowsAcmeCanon, false},
	}, kind(rowsAcme, rowsNote))
	require.Equal(t, []rowsVersionKey{
		{rowsRunR1, 1, "", false}, {rowsRunR1, 3, rowsAcme, true},
		{rowsRunR3, 1, rowsAcme, false},
	}, kind(rowsAcmeCanon, rowsRun))
	require.Equal(t, []rowsVersionKey{
		{rowsRunR2, 1, rowsAcme, false}, {rowsRunR2, 3, rowsBeta, true},
	}, kind(rowsBeta, rowsRun))
	for _, account := range []string{rowsAcme, rowsBeta} {
		for _, concept := range []string{rowsNote, rowsRun, rowsLoose, rowsEmptyKind} {
			require.Equal(t, rowsWalk(t, db, account, concept), kind(account, concept), "%s %s", account, concept)
		}
	}
	require.Error(t, AccountKindVersions(ctx, db, "", rowsNote, func(memorynodes.MemoryNode, bool) error { return nil }))
}

func TestAccountCensusAgreesWithTheIdWalk(t *testing.T) {
	db := openAccountRowsDB(t)
	seedAccountRows(t, db)
	ctx := context.Background()
	kinds := []string{rowsNote, rowsRun, rowsLoose, rowsEmptyKind}

	walk := func(account string) []AccountCensusLine {
		t.Helper()
		var out []AccountCensusLine
		for _, concept := range []string{rowsEmptyKind, rowsLoose, rowsNote, rowsRun} {
			ids, err := AccountRowIDs(ctx, db, account, concept, "", 1000)
			require.NoError(t, err)
			unplaced, err := UnplacedRowIDs(ctx, db, concept, "", 1000)
			require.NoError(t, err)
			if len(ids) == 0 && len(unplaced) == 0 {
				continue
			}
			var versions int64
			require.NoError(t, AccountRowVersions(ctx, db, concept, ids, func(memorynodes.MemoryNode, bool) error { versions++; return nil }))
			out = append(out, AccountCensusLine{Concept: concept, Rows: int64(len(ids)), Versions: versions, InNoAccount: int64(len(unplaced))})
		}
		return out
	}

	acme, err := AccountCensus(ctx, db, rowsAcmeCanon, kinds)
	require.NoError(t, err)
	// notes n1 (2), n2 (2), n5 (1), and in no account n6, n7 (""), n8 (absent);
	// runs r1 (2, one an observation), r3 (1), and r4 in no account; loose has
	// none of acme's but l1 in no account (beta's l2 is not counted). Sorted by
	// concept; a kind with neither has no line.
	require.Equal(t, []AccountCensusLine{
		{Concept: rowsLoose, Rows: 0, Versions: 0, InNoAccount: 1},
		{Concept: rowsNote, Rows: 3, Versions: 5, InNoAccount: 3},
		{Concept: rowsRun, Rows: 2, Versions: 3, InNoAccount: 1},
	}, acme)
	require.Equal(t, walk(rowsAcme), acme)

	beta, err := AccountCensus(ctx, db, rowsBeta, kinds)
	require.NoError(t, err)
	require.Equal(t, []AccountCensusLine{
		{Concept: rowsLoose, Rows: 1, Versions: 1, InNoAccount: 1},
		{Concept: rowsNote, Rows: 1, Versions: 2, InNoAccount: 3},
		{Concept: rowsRun, Rows: 1, Versions: 2, InNoAccount: 1},
	}, beta)
	require.Equal(t, walk(rowsBeta), beta)

	// Only the kinds asked for are counted.
	notes, err := AccountCensus(ctx, db, rowsAcme, []string{rowsNote})
	require.NoError(t, err)
	require.Equal(t, []AccountCensusLine{{Concept: rowsNote, Rows: 3, Versions: 5, InNoAccount: 3}}, notes)
	none, err := AccountCensus(ctx, db, rowsAcme, nil)
	require.NoError(t, err)
	require.Empty(t, none)
}

func TestAccountRowCountsCountARowUnderItsNewestAccountNamingVersion(t *testing.T) {
	db := openAccountRowsDB(t)
	seedAccountRows(t, db)
	ctx := context.Background()
	kinds := []string{rowsNote, rowsRun, rowsLoose, rowsEmptyKind}

	// A row moved either way counts under the account its newest version
	// names: n2 (beta then acme) is acme's, n3 (acme then beta) beta's; n5 is
	// spelled canonically; r1's newest is an observation naming acme, r2's an
	// observation naming beta.
	//
	// THE ONE DIFFERENCE FROM THE EXACT RULE, pinned: a row whose newest
	// version names no account after an older version named one is in no
	// account for the export and its census, and is still counted here under
	// the account it last named. n6 (acme, then none) and r4 (an observation
	// naming acme, older than its versioned row naming none) are counted for
	// acme here and are not in acme's census or walk.
	for _, spelling := range []string{rowsAcme, rowsAcmeCanon} {
		acme, err := AccountRowCounts(ctx, db, spelling, kinds)
		require.NoError(t, err)
		require.Equal(t, []AccountKindCount{
			{Concept: rowsNote, Rows: 4, Versions: 6}, // n1 (2), n2 (2: beta and acme), n5 (1), n6 (1: its acme version)
			{Concept: rowsRun, Rows: 3, Versions: 3},  // r1 (1: the observation), r3 (1), r4 (1: the observation)
		}, acme, spelling)
	}
	beta, err := AccountRowCounts(ctx, db, rowsBeta, kinds)
	require.NoError(t, err)
	require.Equal(t, []AccountKindCount{
		{Concept: rowsLoose, Rows: 1, Versions: 1},
		{Concept: rowsNote, Rows: 1, Versions: 2},
		{Concept: rowsRun, Rows: 1, Versions: 2},
	}, beta)

	census, err := AccountCensus(ctx, db, rowsAcme, []string{rowsNote, rowsRun})
	require.NoError(t, err)
	require.Equal(t, int64(3), census[0].Rows, "n6 is not in acme's census")
	require.Equal(t, int64(2), census[1].Rows, "r4 is not in acme's census")
	for _, v := range rowsWalk(t, db, rowsAcme, rowsNote) {
		require.NotEqual(t, rowsNoteN6, v.id, "n6 is not in acme's walk")
	}

	none, err := AccountRowCounts(ctx, db, rowsAcme, nil)
	require.NoError(t, err)
	require.Empty(t, none)
}

func TestAccountKindsAreTheAccountScopedConceptsSorted(t *testing.T) {
	account := func(stays string) *langparser.BelongsToDecl {
		return &langparser.BelongsToDecl{Scope: langparser.BelongsToAccount, Stays: stays}
	}
	concepts := []*memorynodes.Concept{
		{Name: "v1:zz:travels", BelongsTo: account("")},
		{Name: "v1:aa:stays", BelongsTo: account(langparser.BelongsToStaysSignin)},
		{Name: "v1:mm:cluster", BelongsTo: &langparser.BelongsToDecl{Scope: langparser.BelongsToCluster}},
		{Name: "v1:mm:undeclared"},
		nil,
		{Name: "v1:bb:shared", BelongsTo: &langparser.BelongsToDecl{Scope: langparser.BelongsToAccount, Shared: true}},
		{Name: "v1:zz:travels", BelongsTo: account("")},
	}
	require.Equal(t, []string{"v1:aa:stays", "v1:bb:shared", "v1:zz:travels"}, AccountKinds(concepts, false))
	require.Equal(t, []string{"v1:bb:shared", "v1:zz:travels"}, AccountKinds(concepts, true))
	require.Empty(t, AccountKinds(nil, false))
}

func TestBareAccountIDAcceptsBothSpellingsAndNothingElse(t *testing.T) {
	for raw, want := range map[string]string{
		"acme":                     "acme",
		"  acme  ":                 "acme",
		"v1:accounts:account:acme": "acme",
		"self":                     "self",
	} {
		got, err := BareAccountID(raw)
		require.NoError(t, err, raw)
		require.Equal(t, want, got, raw)
	}
	for raw, want := range map[string]string{
		"":                     "accountId is empty",
		"   ":                  "accountId is empty",
		"v1:accounts:account:": "not an account id",
		"v1:library:file:f1":   "not an account id",
		"a:b":                  "not an account id",
	} {
		_, err := BareAccountID(raw)
		require.ErrorContains(t, err, want, "%q", raw)
	}
}
