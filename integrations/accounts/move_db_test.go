package accounts

// move_db_test.go -- the five move calls through a real engine over the
// embedded tree and a real Postgres (move_builtins.go; design record
// docs/superpowers/specs/2026-11-12-account-export-design.md, sections H, I
// and K). Each call is made as the screen makes it, `builtin <name>(...)`
// through MemQLEngine.Execute, so the capability, the builtin's declared
// arguments and the handler are all on the path.
//
// The row table is shared with the other tests of this package and with the
// engine's own writes, so every fixture row carries a suffix unique to its
// test, every account is the test's own, and a read of the rows in no account
// starts past an id below the test's own.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// mvSecretKind is a kind whose @displayCard names a field it declares @secret.
// The shipped tree has none (TestNoKindThatTravelsDeclaresASecretField), so
// the kind is the test's own, added to the lister the plug-in reads.
var mvSecretKind = &memorynodes.Concept{
	Name:        "v1:testmv:secretCard",
	BelongsTo:   &parser.BelongsToDecl{Scope: parser.BelongsToAccount},
	DisplayCard: &memorynodes.DisplayCard{Primary: "title", Secondary: "token", Status: "state"},
	RowAuthz:    &parser.RowAuthzDecl{Tier: parser.RowAuthzOwned, Owner: "ownerUserId"},
	Schemas: map[string]json.RawMessage{"definition": json.RawMessage(
		`{"properties":{"title":{"type":"string"},"token":{"type":"string","x-secret":true},"state":{"type":"string"},"ownerUserId":{"type":"string"},"accountId":{"type":"string"}}}`)},
}

var (
	mvOnce   sync.Once
	mvEngine *memql.MemQLEngine
	mvDB     *bun.DB
	mvErr    error
)

// moveEngine is one engine for the package's move tests, booted once: Init
// loads the whole tree.
func moveEngine(t *testing.T) (*memql.MemQLEngine, *bun.DB) {
	t.Helper()
	reachable, err := dbtest.EnsureSchema(context.Background())
	require.NoError(t, err)
	if !reachable {
		dbtest.Unreachable(t, "account move", dbtest.DSN(), sql.ErrConnDone)
	}
	mvOnce.Do(func() {
		mvDB = bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
		if _, mvErr = memql.LoadUnifiedConcepts(nil); mvErr != nil {
			return
		}
		if mvEngine, mvErr = memql.New(mvDB); mvErr != nil {
			return
		}
		mvEngine.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
		if mvErr = mvEngine.Init(memorynodes.DefaultRegistry()); mvErr != nil {
			return
		}
		lister := func() []*memorynodes.Concept { return append(memorynodes.List(), mvSecretKind) }
		inv := New(func() *bun.DB { return mvDB }, lister, false, nil)
		mvErr = mvEngine.RegisterIntegration(&Provider{Integration: inv, Exports: NewExports(inv, mvEngine, nil), Moves: NewMoves(inv, mvEngine, mvEngine, nil)})
	})
	require.NoError(t, mvErr)
	// The act refuses outright while this node does not record accounts. Set
	// on every call, not once, so a test that turned it off cannot leave the
	// next one refused.
	mvEngine.SetAccountStampingEnabled(true)
	return mvEngine, mvDB
}

// mvFixture is one test's accounts and rows.
type mvFixture struct {
	s, a, b string
	owner   string
	clock   time.Time
}

func newMvFixture(t *testing.T, db *bun.DB) mvFixture {
	t.Helper()
	s := strings.ToLower(fmt.Sprintf("%x", time.Now().UnixNano()))
	f := mvFixture{s: s, a: "mva-" + s, b: "mvb-" + s, owner: "v1:identity:user:mover-" + s, clock: time.Now().Add(-2 * time.Hour)}
	for _, acct := range []string{f.a, f.b} {
		f.raw(t, db, accountConcept, canonicalPrefix+acct, 0, map[string]any{"name": acct, "status": "active"})
	}
	// The row table is shared with the package's other tests, some of which
	// count rows of the shipped kinds, so the test's rows leave with it.
	t.Cleanup(func() {
		ctx := context.Background()
		like := "%mvt" + s + "%"
		for _, stmt := range []string{
			`DELETE FROM "MemoryNodes" WHERE id LIKE $1 OR id IN ($2, $3)`,
			`DELETE FROM account_row_moves WHERE row_id LIKE $1 OR to_account IN ($2, $3) OR from_account IN ($2, $3)`,
			`DELETE FROM account_follow_tasks WHERE parent_id LIKE $1 OR to_account IN ($2, $3) OR from_account IN ($2, $3)`,
			`DELETE FROM account_assignment_conflict WHERE row_id LIKE $1 OR $2 = ANY(candidates) OR $3 = ANY(candidates)`,
		} {
			args := []any{like, canonicalPrefix + f.a, canonicalPrefix + f.b}
			if !strings.HasPrefix(stmt, `DELETE FROM "MemoryNodes"`) {
				args = []any{like, f.a, f.b}
			}
			_, _ = db.DB.ExecContext(ctx, stmt, args...)
		}
	})
	return f
}

// raw writes one version of a row; minutes orders versions of one id.
func (f mvFixture) raw(t *testing.T, db *bun.DB, concept, id string, minutes int, payload map[string]any) {
	t.Helper()
	encoded, err := json.Marshal(payload)
	require.NoError(t, err)
	_, err = db.DB.ExecContext(context.Background(),
		`INSERT INTO "MemoryNodes" (id, "createdAt", "createdBy", schema, payload, concept) VALUES ($1, $2, 'move-test', '{}', $3, $4)`,
		id, f.clock.Add(time.Duration(minutes)*time.Minute), string(encoded), concept)
	require.NoError(t, err)
}

func (f mvFixture) goal(t *testing.T, db *bun.DB, name, account, statement string) string {
	t.Helper()
	id := "v1:work:goal:mvt" + f.s + "-" + name
	p := map[string]any{"ownerUserId": f.owner, "statement": statement, "status": "open"}
	if account != "" {
		p["accountId"] = account
	}
	f.raw(t, db, "v1:work:goal", id, 1, p)
	return id
}

func (f mvFixture) run(t *testing.T, db *bun.DB, name, goal, account string) string {
	t.Helper()
	id := "v1:work:run:mvt" + f.s + "-" + name
	f.raw(t, db, "v1:work:run", id, 2, map[string]any{"ownerUserId": f.owner, "goalId": goal, "accountId": account, "automationName": name, "status": "done"})
	return id
}

// mvPerson is a person's own signed-in session, an owner.
func mvPerson(id string) context.Context {
	ctx := auth.ContextWithClaims(context.Background(), map[string]any{"sub": id, "role": "owner"})
	return auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: id, Role: auth.RoleOwner, PrimaryEmail: "mover@example.test"})
}

// mvRole is a signed-in person of the given role.
func mvRole(id, role string) context.Context {
	ctx := auth.ContextWithClaims(context.Background(), map[string]any{"sub": id, "role": role})
	return auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: id, Role: auth.Role(role)})
}

// mvPAT is an owner's personal access token.
func mvPAT(id string) context.Context {
	ctx := auth.ContextWithClaims(context.Background(), map[string]any{"sub": id, "role": "owner", "pat_id": "pat-1"})
	return auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: id, Role: auth.RoleOwner})
}

// mvCall runs one builtin as the screen does and answers its rows' payloads.
func mvCall(ctx context.Context, eng *memql.MemQLEngine, name string, args map[string]any) ([]map[string]any, error) {
	call, err := parser.RenderCall(name, args)
	if err != nil {
		return nil, err
	}
	res, err := eng.Execute(ctx, "builtin "+call)
	if err != nil {
		return nil, err
	}
	// A builtin's answer is its rows keyed by id. PreserveOrder stamps each
	// row's createdAt, newest first in the order the handler answered.
	byID, ok := res.OutputPayload().(map[string]memorynodes.MemoryNode)
	if !ok {
		return nil, fmt.Errorf("%s answered %T, not rows", name, res.OutputPayload())
	}
	nodes := make([]memorynodes.MemoryNode, 0, len(byID))
	for _, n := range byID {
		nodes = append(nodes, n)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].CreatedAt.After(nodes[j].CreatedAt) })
	out := make([]map[string]any, 0, len(nodes))
	for _, n := range nodes {
		var p map[string]any
		if err := json.Unmarshal(n.Payload, &p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func mvMust(t *testing.T, ctx context.Context, eng *memql.MemQLEngine, name string, args map[string]any) []map[string]any {
	t.Helper()
	rows, err := mvCall(ctx, eng, name, args)
	require.NoError(t, err, name)
	return rows
}

func mvOfKind(rows []map[string]any, kind string) []map[string]any {
	var out []map[string]any
	for _, r := range rows {
		if r["kind"] == kind {
			out = append(out, r)
		}
	}
	return out
}

func mvCursor(t *testing.T, rows []map[string]any) string {
	t.Helper()
	pages := mvOfKind(rows, "page")
	require.Len(t, pages, 1)
	s, _ := pages[0]["nextCursor"].(string)
	return s
}

func mvIDs(rows []map[string]any) []string {
	out := []string{}
	for _, r := range mvOfKind(rows, "row") {
		out = append(out, r["id"].(string))
	}
	return out
}

func mvNewestAccount(t *testing.T, db *bun.DB, id string) string {
	t.Helper()
	var account sql.NullString
	require.NoError(t, db.DB.QueryRowContext(context.Background(),
		`SELECT payload->>'accountId' FROM "MemoryNodes" WHERE id = $1 ORDER BY "createdAt" DESC LIMIT 1`, id).Scan(&account))
	return account.String
}

func mvLedger(t *testing.T, db *bun.DB, rowIDs ...string) []string {
	t.Helper()
	rows, err := db.DB.QueryContext(context.Background(),
		`SELECT row_id || ' ' || from_account || '>' || to_account || ' ' || moved_by || ' ' || note
		   FROM account_row_moves WHERE row_id = ANY($1::text[]) ORDER BY row_id`, pq.Array(rowIDs))
	require.NoError(t, err)
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var line string
		require.NoError(t, rows.Scan(&line))
		out = append(out, line)
	}
	return out
}

// auditEvents are the payloads of the audit events of an action naming the
// target (or, for an empty target, whose detail names the row).
func mvAudits(t *testing.T, db *bun.DB, action, target, rowID string) []map[string]any {
	t.Helper()
	q := `SELECT payload FROM "MemoryNodes" WHERE concept = 'v1:identity:auditEvent' AND payload->>'action' = $1 AND COALESCE(payload->>'targetId', '') = $2`
	args := []any{action, target}
	if rowID != "" {
		q += ` AND payload->'detail'->>'rowId' = $3`
		args = append(args, rowID)
	}
	rows, err := db.DB.QueryContext(context.Background(), q, args...)
	require.NoError(t, err)
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var raw []byte
		require.NoError(t, rows.Scan(&raw))
		var p map[string]any
		require.NoError(t, json.Unmarshal(raw, &p))
		out = append(out, p)
	}
	return out
}

var mvBuiltins = map[string]map[string]any{
	"accountRows":             {"accountId": "self", "concept": "v1:work:goal"},
	"accountUnplaced":         {},
	"accountRowsMove":         {"rows": []any{map[string]any{"concept": "v1:work:goal", "id": "v1:work:goal:none"}}, "toAccountId": "self", "preview": true},
	"accountConflictLeaveOut": {"concept": "v1:work:goal", "rowId": "none", "conflict": "store_bound_twice"},
	"accountMoves":            {},
}

func TestMoveCallsRequireTheMovePart(t *testing.T) {
	eng, _ := moveEngine(t)
	for name, args := range mvBuiltins {
		for _, role := range []string{"admin", "user", "viewer"} {
			_, err := mvCall(mvRole("v1:identity:user:caller", role), eng, name, args)
			require.Error(t, err, "%s answered a %s", name, role)
			require.Contains(t, err.Error(), memql.CodeCapabilityNotHeld, "%s refused a %s for another reason", name, role)
			require.Contains(t, err.Error(), "app:accounts/move")
		}
	}
	// The reads answer an owner and a developer.
	for _, role := range []string{"owner", "developer"} {
		mvMust(t, mvRole("v1:identity:user:caller", role), eng, "accountUnplaced", nil)
		mvMust(t, mvRole("v1:identity:user:caller", role), eng, "accountMoves", nil)
	}
}

func TestMoveActsRefuseAPersonalAccessToken(t *testing.T) {
	eng, db := moveEngine(t)
	f := newMvFixture(t, db)
	g := f.goal(t, db, "g", f.a, "token")
	rows := []any{map[string]any{"concept": "v1:work:goal", "id": g}}

	_, err := mvCall(mvPAT(f.owner), eng, "accountRowsMove", map[string]any{"rows": rows, "toAccountId": f.b})
	require.Error(t, err)
	require.Contains(t, err.Error(), auth.PersonSessionRequired)
	_, err = mvCall(mvPAT(f.owner), eng, "accountConflictLeaveOut", map[string]any{"concept": "v1:work:goal", "rowId": g, "conflict": "store_bound_twice"})
	require.Error(t, err)
	require.Contains(t, err.Error(), auth.PersonSessionRequired)
	require.Equal(t, f.a, mvNewestAccount(t, db, g))
	require.Empty(t, mvLedger(t, db, g))

	// A preview changes nothing and is not a person's decision.
	answer := mvMust(t, mvPAT(f.owner), eng, "accountRowsMove", map[string]any{"rows": rows, "toAccountId": f.b, "preview": true})
	require.Len(t, mvOfKind(answer, "wouldMove"), 1)
}

func TestAccountRowsPageAndSearch(t *testing.T) {
	eng, db := moveEngine(t)
	f := newMvFixture(t, db)
	g1 := f.goal(t, db, "1", f.a, "Alpha one")
	g2 := f.goal(t, db, "2", f.a, "beta two")
	g3 := f.goal(t, db, "3", f.a, "alpha three")
	f.goal(t, db, "other", f.b, "alpha elsewhere")
	ctx := mvPerson(f.owner)

	page := mvMust(t, ctx, eng, "accountRows", map[string]any{"accountId": f.a, "concept": "v1:work:goal", "limit": 2})
	require.Equal(t, []string{g1, g2}, mvIDs(page))
	require.Equal(t, g2, mvCursor(t, page))
	first := mvOfKind(page, "row")[0]
	require.Equal(t, "Alpha one", first["primary"])
	require.Equal(t, "open", first["status"])
	require.Equal(t, f.owner, first["ownerUserId"])
	require.Equal(t, false, first["followsParent"])
	require.NotEmpty(t, first["createdAt"])
	require.NotEmpty(t, first["updatedAt"])
	_, hasPayload := first["payload"]
	require.False(t, hasPayload)
	page = mvMust(t, ctx, eng, "accountRows", map[string]any{"accountId": f.a, "concept": "v1:work:goal", "limit": 2, "after": g2})
	require.Equal(t, []string{g3}, mvIDs(page))
	require.Equal(t, "", mvCursor(t, page))

	// Search filters the page it read, and the cursor moves past what it
	// dropped, so paging still ends.
	page = mvMust(t, ctx, eng, "accountRows", map[string]any{"accountId": f.a, "concept": "v1:work:goal", "limit": 2, "search": "ALPHA"})
	require.Equal(t, []string{g1}, mvIDs(page))
	require.Equal(t, g2, mvCursor(t, page))
	page = mvMust(t, ctx, eng, "accountRows", map[string]any{"accountId": f.a, "concept": "v1:work:goal", "limit": 2, "search": "ALPHA", "after": g2})
	require.Equal(t, []string{g3}, mvIDs(page))
	require.Equal(t, "", mvCursor(t, page))

	// An empty account reads the rows in no account.
	loose := f.goal(t, db, "loose", "", "no account")
	page = mvMust(t, ctx, eng, "accountRows", map[string]any{"accountId": "", "concept": "v1:work:goal", "limit": 1, "after": "v1:work:goal:mvt" + f.s + "-"})
	require.Equal(t, []string{loose}, mvIDs(page))

	_, err := mvCall(ctx, eng, "accountRows", map[string]any{"accountId": "nobody-" + f.s, "concept": "v1:work:goal"})
	require.ErrorContains(t, err, "there is no account")
	_, err = mvCall(ctx, eng, "accountRows", map[string]any{"accountId": f.a, "concept": "v1:nothing:undeclared"})
	require.ErrorContains(t, err, "is not a kind whose rows belong to accounts")
}

func TestAccountRowsMarkTheRowsThatFollowTheirParent(t *testing.T) {
	eng, db := moveEngine(t)
	f := newMvFixture(t, db)
	gA := f.goal(t, db, "ga", f.a, "in a")
	gB := f.goal(t, db, "gb", f.b, "in b")
	follows := f.run(t, db, "r1", gA, f.a)
	// A run attached to a goal of another account stays where it is, and
	// moves by itself.
	apart := f.run(t, db, "r2", gB, f.a)

	page := mvMust(t, mvPerson(f.owner), eng, "accountRows", map[string]any{"accountId": f.a, "concept": "v1:work:run"})
	byID := map[string]map[string]any{}
	for _, r := range mvOfKind(page, "row") {
		byID[r["id"].(string)] = r
	}
	require.Equal(t, true, byID[follows]["followsParent"])
	require.Equal(t, "v1:work:goal", byID[follows]["parentConcept"])
	require.Equal(t, "r1", byID[follows]["primary"])
	require.Equal(t, false, byID[apart]["followsParent"])
	_, has := byID[apart]["parentConcept"]
	require.False(t, has)

	// The act refuses exactly what the list marks.
	answer := mvMust(t, mvPerson(f.owner), eng, "accountRowsMove", map[string]any{
		"rows":        []any{map[string]any{"concept": "v1:work:run", "id": follows}, map[string]any{"concept": "v1:work:run", "id": apart}},
		"toAccountId": f.b, "preview": true,
	})
	refused := mvOfKind(answer, "refused")
	require.Len(t, refused, 1)
	require.Equal(t, follows, refused[0]["id"])
	require.Equal(t, "follows_parent", refused[0]["reason"])
}

func TestAccountRowsNeverShowASecretField(t *testing.T) {
	eng, db := moveEngine(t)
	f := newMvFixture(t, db)
	id := mvSecretKind.Name + ":mvt" + f.s
	f.raw(t, db, mvSecretKind.Name, id, 1, map[string]any{"title": "Visible", "token": "sk-hidden-value", "state": "on", "ownerUserId": "sk-hidden-owner", "accountId": f.a})

	page := mvMust(t, mvPerson(f.owner), eng, "accountRows", map[string]any{"accountId": f.a, "concept": mvSecretKind.Name})
	rows := mvOfKind(page, "row")
	require.Len(t, rows, 1)
	require.Equal(t, "Visible", rows[0]["primary"])
	require.Equal(t, "", rows[0]["secondary"])
	require.Equal(t, "on", rows[0]["status"])
	encoded, err := json.Marshal(page)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "sk-hidden-value")

	// The search does not reach the secret either.
	page = mvMust(t, mvPerson(f.owner), eng, "accountRows", map[string]any{"accountId": f.a, "concept": mvSecretKind.Name, "search": "sk-hidden"})
	require.Empty(t, mvOfKind(page, "row"))
}

func TestMovePreviewWritesNothingAndAMoveIsRecorded(t *testing.T) {
	eng, db := moveEngine(t)
	f := newMvFixture(t, db)
	g := f.goal(t, db, "g", f.a, "moving")
	r := f.run(t, db, "r", g, f.a)
	ctx := mvPerson(f.owner)
	rows := []any{map[string]any{"concept": "v1:work:goal", "id": g}}

	answer := mvMust(t, ctx, eng, "accountRowsMove", map[string]any{"rows": rows, "toAccountId": f.b, "preview": true, "note": "preview"})
	require.Equal(t, []map[string]any{{"kind": "wouldMove", "concept": "v1:work:goal", "rows": float64(1)}}, mvOfKind(answer, "wouldMove"))
	// A follows line names the kind and no count: followers are found by a
	// background pass, not counted while the person waits.
	// The engine names every kind that may follow a goal, rows or not.
	follows := mvOfKind(answer, "follows")
	require.Contains(t, follows, map[string]any{"kind": "follows", "concept": "v1:work:run"})
	for _, l := range follows {
		_, hasRows := l["rows"]
		require.False(t, hasRows, "a follows line carries no count: %v", l)
	}
	require.Equal(t, []map[string]any{{"kind": "summary", "moved": float64(1), "followerKinds": float64(len(follows)), "refused": float64(0)}}, mvOfKind(answer, "summary"))
	require.Empty(t, mvOfKind(answer, "moved"))
	require.Equal(t, f.a, mvNewestAccount(t, db, g))
	require.Empty(t, mvLedger(t, db, g))
	require.Empty(t, mvAudits(t, db, auditRowsMoved, f.b, ""))

	answer = mvMust(t, ctx, eng, "accountRowsMove", map[string]any{"rows": rows, "toAccountId": canonicalPrefix + f.b, "note": "client asked"})
	require.Equal(t, []map[string]any{{"kind": "moved", "concept": "v1:work:goal", "rows": float64(1)}}, mvOfKind(answer, "moved"))
	require.Equal(t, f.b, mvNewestAccount(t, db, g))
	require.Equal(t, f.a, mvNewestAccount(t, db, r), "a follower is carried by its follow task, not by the act")
	require.Equal(t, []string{g + " " + f.a + ">" + f.b + " " + f.owner + " client asked"}, mvLedger(t, db, g))

	events := mvAudits(t, db, auditRowsMoved, f.b, "")
	require.Len(t, events, 1)
	e := events[0]
	require.Equal(t, "data", e["category"])
	require.Equal(t, "account", e["targetType"])
	require.Equal(t, f.owner, e["actorUserId"])
	require.Equal(t, "success", e["outcome"])
	detail := e["detail"].(map[string]any)
	require.Equal(t, map[string]any{"v1:work:goal": float64(1)}, detail["roots"])
	require.Contains(t, detail["followerKinds"], "v1:work:run")
	require.Equal(t, "client asked", detail["note"])

	// The ledger reads back, for either account and for all.
	for _, account := range []string{f.a, f.b, ""} {
		page := mvMust(t, ctx, eng, "accountMoves", map[string]any{"accountId": account, "limit": 200})
		var mine []map[string]any
		for _, m := range mvOfKind(page, "move") {
			if m["rowId"] == g {
				mine = append(mine, m)
			}
		}
		require.Len(t, mine, 1, "account %q", account)
		require.Equal(t, f.a, mine[0]["fromAccount"])
		require.Equal(t, f.b, mine[0]["toAccount"])
		require.Equal(t, f.owner, mine[0]["movedBy"])
		_, hasFollowers := mine[0]["followers"]
		require.False(t, hasFollowers, "a ledger line is one root; what followed it is not counted there")
		require.Equal(t, "client asked", mine[0]["note"])
		require.NotEmpty(t, mine[0]["movedAt"])
	}
}

func TestAccountMovesPagesNewestFirst(t *testing.T) {
	eng, db := moveEngine(t)
	f := newMvFixture(t, db)
	ctx := mvPerson(f.owner)
	var goals []string
	for i := range 3 {
		g := f.goal(t, db, fmt.Sprintf("g%d", i), f.a, "x")
		goals = append(goals, g)
		mvMust(t, ctx, eng, "accountRowsMove", map[string]any{"rows": []any{map[string]any{"concept": "v1:work:goal", "id": g}}, "toAccountId": f.b})
	}
	page := mvMust(t, ctx, eng, "accountMoves", map[string]any{"accountId": f.b, "limit": 2})
	moves := mvOfKind(page, "move")
	require.Len(t, moves, 2)
	require.Equal(t, goals[2], moves[0]["rowId"])
	require.Equal(t, goals[1], moves[1]["rowId"])
	next := mvCursor(t, page)
	require.NotEmpty(t, next)
	page = mvMust(t, ctx, eng, "accountMoves", map[string]any{"accountId": f.b, "limit": 2, "after": next})
	moves = mvOfKind(page, "move")
	require.Len(t, moves, 1)
	require.Equal(t, goals[0], moves[0]["rowId"])
	require.Equal(t, "", mvCursor(t, page))
}

func TestMoveRefusesInWords(t *testing.T) {
	eng, db := moveEngine(t)
	f := newMvFixture(t, db)
	ctx := mvPerson(f.owner)
	many := make([]any, 501)
	for i := range many {
		many[i] = map[string]any{"concept": "v1:work:goal", "id": fmt.Sprintf("v1:work:goal:many-%d", i)}
	}
	_, err := mvCall(ctx, eng, "accountRowsMove", map[string]any{"rows": many, "toAccountId": f.b, "preview": true})
	require.ErrorContains(t, err, "One move takes at most 500 rows, and 501 were chosen.")

	_, err = mvCall(ctx, eng, "accountRowsMove", map[string]any{"rows": []any{"v1:work:goal:loose"}, "toAccountId": f.b, "preview": true})
	require.ErrorContains(t, err, "Row 1 is not a row: each row is {concept, id}.")

	g := f.goal(t, db, "g", f.a, "x")
	_, err = mvCall(ctx, eng, "accountRowsMove", map[string]any{"rows": []any{map[string]any{"concept": "v1:work:goal", "id": g}}, "toAccountId": "nobody-" + f.s})
	require.ErrorContains(t, err, "There is no active account")
	require.Equal(t, f.a, mvNewestAccount(t, db, g))
	require.Empty(t, mvLedger(t, db, g))
}

func TestLeavingAConflictOutResolvesIt(t *testing.T) {
	eng, db := moveEngine(t)
	f := newMvFixture(t, db)
	ctx := mvPerson(f.owner)
	g := f.goal(t, db, "conflicted", "", "two stores")
	_, err := db.DB.ExecContext(context.Background(), `INSERT INTO account_assignment_conflict (concept, row_id, kind, detail, candidates)
		VALUES ('v1:work:goal', $1, 'store_bound_twice', 'bound to two stores', $2::text[])`, g, "{"+f.a+","+f.b+"}")
	require.NoError(t, err)

	listed := func() []map[string]any {
		var out []map[string]any
		for _, c := range mvOfKind(mvMust(t, ctx, eng, "accountUnplaced", nil), "conflict") {
			if c["rowId"] == g {
				out = append(out, c)
			}
		}
		return out
	}
	open := listed()
	require.Len(t, open, 1)
	require.Equal(t, []any{f.a, f.b}, open[0]["candidates"])
	require.Equal(t, "store_bound_twice", open[0]["conflict"])
	require.Equal(t, "bound to two stores", open[0]["detail"])
	n, err := memql.ConflictsNaming(context.Background(), db.DB, f.a)
	require.NoError(t, err)
	require.Equal(t, int64(1), n)

	answer := mvMust(t, ctx, eng, "accountConflictLeaveOut", map[string]any{"concept": "v1:work:goal", "rowId": g, "conflict": "store_bound_twice"})
	rows := mvOfKind(answer, "conflict")
	require.Len(t, rows, 1)
	require.NotEmpty(t, rows[0]["leftOutAt"])
	require.Empty(t, listed())
	n, err = memql.ConflictsNaming(context.Background(), db.DB, f.a)
	require.NoError(t, err)
	require.Equal(t, int64(0), n)
	var by string
	require.NoError(t, db.DB.QueryRowContext(context.Background(), `SELECT left_out_by FROM account_assignment_conflict WHERE row_id = $1`, g).Scan(&by))
	require.Equal(t, f.owner, by)
	events := mvAudits(t, db, auditConflictLeftOut, "", g)
	require.Len(t, events, 1)
	require.Equal(t, "data", events[0]["category"])
	require.Equal(t, "account", events[0]["targetType"])

	_, err = mvCall(ctx, eng, "accountConflictLeaveOut", map[string]any{"concept": "v1:work:goal", "rowId": g, "conflict": "store_bound_twice"})
	require.ErrorContains(t, err, "This row was already left out of every account")
	require.Len(t, mvAudits(t, db, auditConflictLeftOut, "", g), 1)
}

func TestPlacingAConflictedRowResolvesItsConflict(t *testing.T) {
	eng, db := moveEngine(t)
	f := newMvFixture(t, db)
	ctx := mvPerson(f.owner)
	g := f.goal(t, db, "placed", "", "two stores")
	_, err := db.DB.ExecContext(context.Background(), `INSERT INTO account_assignment_conflict (concept, row_id, kind, detail, candidates)
		VALUES ('v1:work:goal', $1, 'store_bound_twice', '', $2::text[])`, g, "{"+f.a+"}")
	require.NoError(t, err)
	listed := func() bool {
		for _, c := range mvOfKind(mvMust(t, ctx, eng, "accountUnplaced", nil), "conflict") {
			if c["rowId"] == g {
				return true
			}
		}
		return false
	}
	rows := []any{map[string]any{"concept": "v1:work:goal", "id": g}}

	// A preview closes nothing.
	answer := mvMust(t, ctx, eng, "accountRowsMove", map[string]any{"rows": rows, "toAccountId": f.a, "preview": true})
	require.Len(t, mvOfKind(answer, "wouldMove"), 1)
	require.True(t, listed())

	answer = mvMust(t, ctx, eng, "accountRowsMove", map[string]any{"rows": rows, "toAccountId": f.a})
	require.Len(t, mvOfKind(answer, "moved"), 1)
	require.Equal(t, f.a, mvNewestAccount(t, db, g))
	require.False(t, listed(), "a placed row no longer awaits a decision")
	_, err = mvCall(ctx, eng, "accountConflictLeaveOut", map[string]any{"concept": "v1:work:goal", "rowId": g, "conflict": "store_bound_twice"})
	require.ErrorContains(t, err, "no longer awaiting a decision")
}

func TestAMoveRefusedWhileRecordingIsOffSaysSo(t *testing.T) {
	eng, db := moveEngine(t)
	f := newMvFixture(t, db)
	g := f.goal(t, db, "g", f.a, "x")
	eng.SetAccountStampingEnabled(false)
	t.Cleanup(func() { eng.SetAccountStampingEnabled(true) })
	for _, preview := range []bool{true, false} {
		_, err := mvCall(mvPerson(f.owner), eng, "accountRowsMove", map[string]any{
			"rows": []any{map[string]any{"concept": "v1:work:goal", "id": g}}, "toAccountId": f.b, "preview": preview})
		require.Error(t, err)
		require.Contains(t, err.Error(), "Rows cannot be moved yet: this cluster does not record which account a row belongs to.")
		require.NotContains(t, err.Error(), memql.ErrAccountRecordingOff.Error())
	}
	require.Equal(t, f.a, mvNewestAccount(t, db, g))
	require.Empty(t, mvLedger(t, db, g))
}

func TestAMoveThatMovesNothingWritesNoAuditEvent(t *testing.T) {
	eng, db := moveEngine(t)
	f := newMvFixture(t, db)
	g := f.goal(t, db, "g", f.b, "already there")
	answer := mvMust(t, mvPerson(f.owner), eng, "accountRowsMove", map[string]any{
		"rows": []any{map[string]any{"concept": "v1:work:goal", "id": g}}, "toAccountId": f.b})
	require.Empty(t, mvOfKind(answer, "moved"))
	refused := mvOfKind(answer, "refused")
	require.Len(t, refused, 1)
	require.Equal(t, "same_account", refused[0]["reason"])
	require.Empty(t, mvAudits(t, db, auditRowsMoved, f.b, ""))
	require.Empty(t, mvLedger(t, db, g))
}

func TestAccountUnplacedCountsFromTheJobsCounters(t *testing.T) {
	eng, db := moveEngine(t)
	f := newMvFixture(t, db)
	_, err := db.DB.ExecContext(context.Background(), `INSERT INTO account_assignment_progress (concept, shared, conflicts, waiting) VALUES ('v1:work:goal', 2, 1, 4)
		ON CONFLICT (concept) DO UPDATE SET shared = 2, conflicts = 1, waiting = 4`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.DB.ExecContext(context.Background(), `DELETE FROM account_assignment_progress WHERE concept = 'v1:work:goal'`)
	})
	var line map[string]any
	for _, u := range mvOfKind(mvMust(t, mvPerson(f.owner), eng, "accountUnplaced", nil), "unplaced") {
		if u["concept"] == "v1:work:goal" {
			line = u
		}
	}
	require.Equal(t, map[string]any{"kind": "unplaced", "concept": "v1:work:goal", "namespace": "work", "name": "goal", "rows": float64(6)}, line,
		"shared and waiting; an open conflict is listed on its own and not counted twice")
}
