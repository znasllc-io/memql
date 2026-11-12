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
	"fmt"
	"slices"
	"strings"
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

// On a kind with an observation store, a row whose versioned rows name an
// account and whose newer observation names none is in no account; a row whose
// observation names one is not, whatever its versioned rows say; and an
// observation older than the newest versioned row decides nothing. Paging past
// one of them walks on.
func TestUnplacedRowIDsReadTheObservation(t *testing.T) {
	db := openAccountRowsDB(t)
	ctx := context.Background()
	nodes, live := `"MemoryNodes"`, "memory_liveness"
	a := map[string]any{"accountId": rowsAcme}
	cleared, named, stale, plain := rowsRun+":o1", rowsRun+":o2", rowsRun+":o3", rowsRun+":o4"
	rowsVersion(t, db, nodes, rowsRun, cleared, 1, a)
	rowsVersion(t, db, nodes, rowsRun, cleared, 2, a)
	rowsVersion(t, db, live, rowsRun, cleared, 3, map[string]any{"state": "done"})
	rowsVersion(t, db, nodes, rowsRun, named, 1, map[string]any{"state": "running"})
	rowsVersion(t, db, live, rowsRun, named, 2, a)
	rowsVersion(t, db, nodes, rowsRun, stale, 5, map[string]any{"accountId": rowsBeta})
	rowsVersion(t, db, live, rowsRun, stale, 4, map[string]any{"state": "old"})
	rowsVersion(t, db, nodes, rowsRun, plain, 1, map[string]any{"state": "none"})

	ids, err := UnplacedRowIDs(ctx, db, rowsRun, "", 100)
	require.NoError(t, err)
	require.Equal(t, []string{cleared, plain}, ids)
	ids, err = UnplacedRowIDs(ctx, db, rowsRun, cleared, 1)
	require.NoError(t, err)
	require.Equal(t, []string{plain}, ids)
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

func TestAccountRowCountsAreTheCensusRows(t *testing.T) {
	db := openAccountRowsDB(t)
	seedAccountRows(t, db)
	ctx := context.Background()
	kinds := []string{rowsNote, rowsRun, rowsLoose, rowsEmptyKind}

	// Exactly the census's Rows and Versions per kind, for both accounts and
	// both spellings: n2 (beta then acme) is acme's, n3 (acme then beta)
	// beta's, n5 is spelled canonically, r1's and r2's newest versions are
	// observations.
	for _, account := range []string{rowsAcme, rowsAcmeCanon, rowsBeta} {
		census, err := AccountCensus(ctx, db, account, kinds)
		require.NoError(t, err)
		want := []AccountKindCount{}
		for _, l := range census {
			if l.Rows > 0 {
				want = append(want, AccountKindCount{Concept: l.Concept, Rows: l.Rows, Versions: l.Versions})
			}
		}
		got, err := AccountRowCounts(ctx, db, account, kinds)
		require.NoError(t, err)
		require.Equal(t, want, got, account)
	}
	acme, err := AccountRowCounts(ctx, db, rowsAcme, kinds)
	require.NoError(t, err)
	require.Equal(t, []AccountKindCount{
		{Concept: rowsNote, Rows: 3, Versions: 5},
		{Concept: rowsRun, Rows: 2, Versions: 3},
	}, acme)

	// A row whose account a later version removed is counted nowhere: n6
	// (acme, then none) and r4 (an acme observation older than its versioned
	// row naming none) are in no account's count, walk or kind read.
	for _, account := range []string{rowsAcme, rowsBeta} {
		for _, concept := range []string{rowsNote, rowsRun} {
			ids, err := AccountKindRowIDs(ctx, db, account, concept)
			require.NoError(t, err)
			require.NotContains(t, ids, rowsNoteN6)
			require.NotContains(t, ids, rowsRunR4)
		}
	}

	none, err := AccountRowCounts(ctx, db, rowsAcme, nil)
	require.NoError(t, err)
	require.Empty(t, none)
}

func TestAccountKindVersionsAcrossBatchBoundaries(t *testing.T) {
	db := openAccountRowsDB(t)
	seedAccountRows(t, db)
	ctx := context.Background()
	saved := AccountKindVersionBatch
	t.Cleanup(func() { AccountKindVersionBatch = saved })
	for _, batch := range []int{1, 2, 3, 5000} {
		AccountKindVersionBatch = batch
		for _, account := range []string{rowsAcme, rowsBeta} {
			for _, concept := range []string{rowsNote, rowsRun, rowsLoose, rowsEmptyKind} {
				var got []rowsVersionKey
				require.NoError(t, AccountKindVersions(ctx, db, account, concept, rowsCollect(t, concept, &got)))
				require.Equal(t, rowsWalk(t, db, account, concept), got, "batch %d %s %s", batch, account, concept)
			}
		}
	}
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

func TestAccountKindRowIDsAreTheIdsOfTheWalk(t *testing.T) {
	db := openAccountRowsDB(t)
	seedAccountRows(t, db)
	ctx := context.Background()
	for _, account := range []string{rowsAcme, rowsAcmeCanon, rowsBeta} {
		for _, concept := range []string{rowsNote, rowsRun, rowsLoose, rowsEmptyKind} {
			walked, err := AccountRowIDs(ctx, db, account, concept, "", 1000)
			require.NoError(t, err)
			got, err := AccountKindRowIDs(ctx, db, account, concept)
			require.NoError(t, err)
			require.Equal(t, walked, got, "%s %s", account, concept)
		}
	}
	ids, err := AccountKindRowIDs(ctx, db, rowsAcme, rowsRun)
	require.NoError(t, err)
	require.Equal(t, []string{rowsRunR1, rowsRunR3}, ids, "r1 by its observation; r4's older observation does not decide")
	_, err = AccountKindRowIDs(ctx, db, "", rowsNote)
	require.Error(t, err)
}

func TestAccountRowHeadsAreTheNewestVersionAndTheFirstTime(t *testing.T) {
	db := openAccountRowsDB(t)
	seedAccountRows(t, db)
	ctx := context.Background()
	// n9 has three versions, the newest naming beta.
	n9 := rowsNote + ":n9"
	rowsVersion(t, db, `"MemoryNodes"`, rowsNote, n9, 1, map[string]any{"title": "one"})
	rowsVersion(t, db, `"MemoryNodes"`, rowsNote, n9, 2, map[string]any{"accountId": rowsAcme})
	rowsVersion(t, db, `"MemoryNodes"`, rowsNote, n9, 3, map[string]any{"accountId": rowsBeta})

	type head struct {
		key   rowsVersionKey
		first int
	}
	read := func(concept string, ids []string) []head {
		t.Helper()
		var out []head
		var keys []rowsVersionKey
		collect := rowsCollect(t, concept, &keys)
		require.NoError(t, AccountRowHeads(ctx, db, concept, ids, func(n memorynodes.MemoryNode, observed bool, first time.Time) error {
			require.NoError(t, collect(n, observed))
			out = append(out, head{keys[len(keys)-1], int(first.Sub(rowsClock) / time.Minute)})
			return nil
		}))
		return out
	}

	// In the ids' order whatever order they were given; an id that does not
	// exist has no call.
	require.Equal(t, []head{
		{rowsVersionKey{rowsNoteN1, 2, rowsAcme, false}, 1},
		{rowsVersionKey{n9, 3, rowsBeta, false}, 1},
	}, read(rowsNote, []string{n9, rowsNote + ":missing", rowsNoteN1}))
	// r1's newest version is its observation; r4's observation is older than
	// its versioned row and is not.
	require.Equal(t, []head{
		{rowsVersionKey{rowsRunR1, 3, rowsAcme, true}, 1},
		{rowsVersionKey{rowsRunR4, 5, "", false}, 5},
	}, read(rowsRun, []string{rowsRunR4, rowsRunR1}))
	require.Empty(t, read(rowsNote, nil))

	tooMany := make([]string, 501)
	require.Error(t, AccountRowHeads(ctx, db, rowsNote, tooMany, func(memorynodes.MemoryNode, bool, time.Time) error { return nil }))
}

// ruleCase is one fixture row that pins a case of the rule, and what the rule
// says of it.
type ruleCase struct {
	name     string
	concept  string
	id       string
	owner    string // the account whose row it is, "" for in no account
	exists   bool   // false: no reader may answer for it at all
	versions int64  // every version, the observation included
	newest   int    // minutes of its newest version
	observed bool   // its newest version is the observation
}

const (
	rowsCaseKind     = "v1:aerows:case"
	rowsCaseOther    = "v1:aerows:other"
	rowsCaseObserved = "v1:platform:customDomain" // listed in memorynodes.HasLiveness
)

// seedRuleCases writes one row per case into kinds of their own, so a kind's
// counts are the cases' counts.
func seedRuleCases(t *testing.T, db *sql.DB) []ruleCase {
	t.Helper()
	nodes, live := `"MemoryNodes"`, "memory_liveness"
	a := map[string]any{"accountId": rowsAcme}
	canon := map[string]any{"accountId": rowsAcmeCanon}
	b := map[string]any{"accountId": rowsBeta}
	null := map[string]any{"accountId": nil}
	none := map[string]any{"title": "none"}
	dual := rowsCaseKind + ":dual"

	rowsVersion(t, db, nodes, rowsCaseObserved, rowsCaseObserved+":tie", 1, b)
	rowsVersion(t, db, nodes, rowsCaseObserved, rowsCaseObserved+":tie", 2, b)
	rowsVersion(t, db, live, rowsCaseObserved, rowsCaseObserved+":tie", 2, a)
	rowsVersion(t, db, nodes, rowsCaseObserved, rowsCaseObserved+":cleared", 1, a)
	rowsVersion(t, db, live, rowsCaseObserved, rowsCaseObserved+":cleared", 2, none)
	rowsVersion(t, db, nodes, rowsCaseObserved, rowsCaseObserved+":nulled", 1, a)
	rowsVersion(t, db, live, rowsCaseObserved, rowsCaseObserved+":nulled", 2, null)
	rowsVersion(t, db, live, rowsCaseObserved, rowsCaseObserved+":orphan", 1, a)
	rowsVersion(t, db, nodes, rowsCaseKind, rowsCaseKind+":null", 1, a)
	rowsVersion(t, db, nodes, rowsCaseKind, rowsCaseKind+":null", 2, null)
	rowsVersion(t, db, nodes, rowsCaseKind, rowsCaseKind+":bareThenCanon", 1, a)
	rowsVersion(t, db, nodes, rowsCaseKind, rowsCaseKind+":bareThenCanon", 2, canon)
	rowsVersion(t, db, nodes, rowsCaseKind, rowsCaseKind+":canonThenBare", 1, canon)
	rowsVersion(t, db, nodes, rowsCaseKind, rowsCaseKind+":canonThenBare", 2, a)
	rowsVersion(t, db, nodes, rowsCaseKind, dual, 1, a)
	// The key is (id, createdAt), so the second concept's version is later.
	rowsVersion(t, db, nodes, rowsCaseOther, dual, 3, b)

	return []ruleCase{
		{"an observation with the newest versioned row's createdAt decides", rowsCaseObserved, rowsCaseObserved + ":tie", rowsAcme, true, 3, 2, true},
		{"a newer observation naming no account over a versioned row naming one", rowsCaseObserved, rowsCaseObserved + ":cleared", "", true, 2, 2, true},
		{"an observation whose accountId is JSON null", rowsCaseObserved, rowsCaseObserved + ":nulled", "", true, 2, 2, true},
		{"an observation with no versioned row is no row", rowsCaseObserved, rowsCaseObserved + ":orphan", "", false, 0, 0, false},
		{"a versioned row whose accountId is JSON null", rowsCaseKind, rowsCaseKind + ":null", "", true, 2, 2, false},
		{"both spellings, bare then canonical, are one row of one account", rowsCaseKind, rowsCaseKind + ":bareThenCanon", rowsAcme, true, 2, 2, false},
		{"both spellings, canonical then bare, are one row of one account", rowsCaseKind, rowsCaseKind + ":canonThenBare", rowsAcme, true, 2, 2, false},
		{"an id under two concepts is decided in its first kind", rowsCaseKind, dual, rowsAcme, true, 1, 1, false},
		{"an id under two concepts is decided in its second kind", rowsCaseOther, dual, rowsBeta, true, 1, 3, false},
	}
}

// withDecisionShape runs fn with every account sent down one shape of the
// decision: few true forces the FEW statements, false the MANY ones.
func withDecisionShape(t *testing.T, few bool, fn func()) {
	t.Helper()
	saved := accountDecisionFewLimit
	defer func() { accountDecisionFewLimit = saved }()
	if few {
		accountDecisionFewLimit = 1 << 30
	} else {
		accountDecisionFewLimit = 0
	}
	fn()
}

// TestTheRuleHoldsCaseByCase asks every function that decides whose a row is
// about every case, in both shapes of the decision.
func TestTheRuleHoldsCaseByCase(t *testing.T) {
	db := openAccountRowsDB(t)
	seedAccountRows(t, db)
	cases := seedRuleCases(t, db)
	ctx := context.Background()
	kinds := []string{rowsCaseKind, rowsCaseOther, rowsCaseObserved}
	bare := func(account string) string {
		if account == "" {
			return ""
		}
		b, err := BareAccountID(account)
		require.NoError(t, err)
		return b
	}

	for _, few := range []bool{true, false} {
		withDecisionShape(t, few, func() {
			for _, account := range []string{rowsAcme, rowsBeta} {
				census, err := AccountCensus(ctx, db, account, kinds)
				require.NoError(t, err)
				counts, err := AccountRowCounts(ctx, db, account, kinds)
				require.NoError(t, err)
				wantCensus := map[string]AccountCensusLine{}
				wantCounts := map[string]AccountKindCount{}
				for _, c := range cases {
					label := fmt.Sprintf("few=%t %s, asked for %s", few, c.name, account)
					mine := c.exists && c.owner == account

					byPage, err := AccountRowIDs(ctx, db, account, c.concept, "", 1000)
					require.NoError(t, err)
					require.Equal(t, mine, slices.Contains(byPage, c.id), "AccountRowIDs: %s", label)
					byKind, err := AccountKindRowIDs(ctx, db, account, c.concept)
					require.NoError(t, err)
					require.Equal(t, mine, slices.Contains(byKind, c.id), "AccountKindRowIDs: %s", label)
					var versions int64
					require.NoError(t, AccountKindVersions(ctx, db, account, c.concept, func(n memorynodes.MemoryNode, _ bool) error {
						if n.ID == c.id {
							versions++
						}
						return nil
					}))
					if mine {
						require.Equal(t, c.versions, versions, "AccountKindVersions: %s", label)
					} else {
						require.Zero(t, versions, "AccountKindVersions: %s", label)
					}
					unplaced, err := UnplacedRowIDs(ctx, db, c.concept, "", 1000)
					require.NoError(t, err)
					require.Equal(t, c.exists && c.owner == "", slices.Contains(unplaced, c.id), "UnplacedRowIDs: %s", label)

					var heads int
					require.NoError(t, AccountRowHeads(ctx, db, c.concept, []string{c.id}, func(n memorynodes.MemoryNode, observed bool, _ time.Time) error {
						heads++
						var p struct {
							AccountID *string `json:"accountId"`
						}
						require.NoError(t, json.Unmarshal(n.Payload, &p))
						got := ""
						if p.AccountID != nil {
							got = *p.AccountID
						}
						require.Equal(t, c.owner, bare(got), "AccountRowHeads' newest version: %s", label)
						require.Equal(t, c.observed, observed, "AccountRowHeads: %s", label)
						require.Equal(t, c.newest, int(n.CreatedAt.Sub(rowsClock)/time.Minute), "AccountRowHeads: %s", label)
						return nil
					}))
					if c.exists {
						require.Equal(t, 1, heads, "AccountRowHeads: %s", label)
					} else {
						require.Zero(t, heads, "AccountRowHeads: %s", label)
					}

					w := wantCensus[c.concept]
					w.Concept = c.concept
					k := wantCounts[c.concept]
					k.Concept = c.concept
					if mine {
						w.Rows++
						w.Versions += c.versions
						k.Rows++
						k.Versions += c.versions
						wantCounts[c.concept] = k
					}
					if c.exists && c.owner == "" {
						w.InNoAccount++
					}
					wantCensus[c.concept] = w
				}
				want := []AccountCensusLine{}
				wantK := []AccountKindCount{}
				for _, concept := range []string{rowsCaseKind, rowsCaseOther, rowsCaseObserved} {
					if l := wantCensus[concept]; l.Rows > 0 || l.InNoAccount > 0 {
						want = append(want, l)
					}
					if k, ok := wantCounts[concept]; ok {
						wantK = append(wantK, k)
					}
				}
				slices.SortFunc(want, func(x, y AccountCensusLine) int { return strings.Compare(x.Concept, y.Concept) })
				slices.SortFunc(wantK, func(x, y AccountKindCount) int { return strings.Compare(x.Concept, y.Concept) })
				require.Equal(t, want, census, "AccountCensus few=%t for %s", few, account)
				require.Equal(t, wantK, counts, "AccountRowCounts few=%t for %s", few, account)
			}
		})
	}
}

// TestBothShapesOfTheDecisionGiveOneAnswer runs every deciding function over
// the whole fixture with every account sent down the FEW statements and then
// down the MANY ones: a wrong choice of shape may cost time, never an answer.
func TestBothShapesOfTheDecisionGiveOneAnswer(t *testing.T) {
	db := openAccountRowsDB(t)
	seedAccountRows(t, db)
	seedRuleCases(t, db)
	ctx := context.Background()
	kinds := []string{rowsNote, rowsRun, rowsLoose, rowsEmptyKind, rowsCaseKind, rowsCaseOther, rowsCaseObserved}
	type answer struct {
		ids      map[string][]string
		versions map[string][]rowsVersionKey
		counts   []AccountKindCount
		census   []AccountCensusLine
	}
	read := func(account string) answer {
		t.Helper()
		out := answer{ids: map[string][]string{}, versions: map[string][]rowsVersionKey{}}
		for _, concept := range kinds {
			ids, err := AccountKindRowIDs(ctx, db, account, concept)
			require.NoError(t, err)
			out.ids[concept] = ids
			var v []rowsVersionKey
			require.NoError(t, AccountKindVersions(ctx, db, account, concept, func(n memorynodes.MemoryNode, observed bool) error {
				v = append(v, rowsVersionKey{n.ID, int(n.CreatedAt.Sub(rowsClock) / time.Minute), "", observed})
				return nil
			}))
			out.versions[concept] = v
		}
		var err error
		out.counts, err = AccountRowCounts(ctx, db, account, kinds)
		require.NoError(t, err)
		out.census, err = AccountCensus(ctx, db, account, kinds)
		require.NoError(t, err)
		return out
	}
	for _, account := range []string{rowsAcme, rowsAcmeCanon, rowsBeta} {
		var few, many answer
		withDecisionShape(t, true, func() { few = read(account) })
		withDecisionShape(t, false, func() { many = read(account) })
		require.Equal(t, few, many, account)
		require.NotEmpty(t, few.counts, "the fixture holds rows of %s", account)
		for _, concept := range kinds {
			walked, err := AccountRowIDs(ctx, db, account, concept, "", 1000)
			require.NoError(t, err)
			require.Equal(t, walked, many.ids[concept], "%s %s", account, concept)
		}
	}
}
