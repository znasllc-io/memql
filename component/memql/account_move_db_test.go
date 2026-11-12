package memql

import (
	"context"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// THE GOVERNED MOVE, AGAINST REAL TABLES (account_move.go). Postgres-gated like
// their neighbours.

type moveFixture struct {
	s       string
	a, b, c string
	owner   string
}

func newMoveFixture(t *testing.T, db *bun.DB, name string) moveFixture {
	t.Helper()
	s := uniqueSuffix(name)
	f := moveFixture{s: s, a: "ma-" + s, b: "mb-" + s, c: "mc-" + s, owner: "move-owner-" + s}
	for _, account := range []string{f.a, f.b, f.c} {
		seedRawRow(t, context.Background(), db, conceptAccountsAccount, conceptAccountsAccount+":"+account,
			time.Now().Add(-2*time.Hour), "move-test", map[string]any{"name": account, "status": "active"})
	}
	return f
}

// row writes one row an hour old and returns its id.
func (f moveFixture) row(t *testing.T, db *bun.DB, concept, id string, payload map[string]any) string {
	t.Helper()
	full := concept + ":" + id + "-" + f.s
	seedRawRow(t, context.Background(), db, concept, full, time.Now().Add(-time.Hour), "move-test", payload)
	return full
}

func (f moveFixture) goal(t *testing.T, db *bun.DB, id, account string) string {
	t.Helper()
	payload := map[string]any{"ownerUserId": f.owner, "title": id}
	if account != "" {
		payload["accountId"] = account
	}
	return f.row(t, db, "v1:work:goal", id, payload)
}

func (f moveFixture) run(t *testing.T, db *bun.DB, id, goal, account string) string {
	t.Helper()
	return f.row(t, db, "v1:work:run", id, map[string]any{"ownerUserId": f.owner, "goalId": goal, "accountId": account})
}

func moveLedger(t *testing.T, db *bun.DB, ids ...string) []string {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), `SELECT row_id || ' ' || from_account || '>' || to_account || ' ' || followers::text
		FROM account_row_moves WHERE row_id = ANY(?::text[]) ORDER BY row_id`, pq.Array(ids))
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

func pendingFor(t *testing.T, db *bun.DB, account string) int64 {
	t.Helper()
	n, err := AccountFollowPending(context.Background(), db.DB, account)
	require.NoError(t, err)
	return n
}

// conceptWith finds a loaded concept the predicate holds for, so the fixtures
// do not hard-code which kinds the tree happens to declare so.
func conceptWith(t *testing.T, pred func(*memorynodes.Concept) bool) string {
	t.Helper()
	for _, c := range memorynodes.All() {
		if pred(c) {
			return c.Name
		}
	}
	t.Fatal("no loaded concept fits")
	return ""
}

func TestAccountMoveRefusesWhatItMayNotMove(t *testing.T) {
	engine, db, _ := readMergeTestEngine(t)
	ctx := context.Background()
	f := newMoveFixture(t, db, t.Name())

	clusterWide := conceptWith(t, func(c *memorynodes.Concept) bool { return c.ClusterWide() })
	// Every loaded concept declares @belongsTo, so a kind the registry does
	// not know stands in for an undeclared one: nothing says whose its rows are.
	undeclared := "v1:nothing:undeclared"
	stays := conceptWith(t, func(c *memorynodes.Concept) bool { return c.Stays() != "" })
	goalA := f.goal(t, db, "goal-a", f.a)
	goalB := f.goal(t, db, "goal-b", f.b)
	runA := f.run(t, db, "run-a", goalA, f.a)
	campaign := f.row(t, db, "v1:campaigns:campaign", "campaign", map[string]any{"name": "c", "accountId": accountCanonicalPrefix + f.a})
	refs := []AccountRowRef{
		{Concept: clusterWide, ID: f.row(t, db, clusterWide, "cw", map[string]any{})},
		{Concept: undeclared, ID: f.row(t, db, undeclared, "ud", map[string]any{})},
		{Concept: stays, ID: f.row(t, db, stays, "st", map[string]any{"accountId": f.a})},
		{Concept: "v1:work:run", ID: runA},
		{Concept: "v1:campaigns:campaign", ID: campaign},
		{Concept: "v1:work:goal", ID: goalB},
		{Concept: "v1:work:goal", ID: "v1:work:goal:missing-" + f.s},
	}
	res, err := engine.MoveAccountRows(ctx, db, MoveRequest{Rows: refs, To: f.b, By: f.owner})
	require.NoError(t, err)
	require.Empty(t, res.Moved)
	reasons := map[string]string{}
	for _, r := range res.Refused {
		reasons[r.ID] = r.Reason
	}
	require.Equal(t, map[string]string{
		refs[0].ID: "cluster_wide", refs[1].ID: "undeclared", refs[2].ID: "stays", runA: "follows_parent",
		campaign: "states_own_account", goalB: "same_account", refs[6].ID: "not_found",
	}, reasons)

	res, err = engine.MoveAccountRows(ctx, db, MoveRequest{Rows: []AccountRowRef{{Concept: "v1:work:goal", ID: goalA}}, To: "nobody-" + f.s, By: f.owner})
	require.NoError(t, err)
	require.Equal(t, []MoveRefusal{{Concept: "v1:work:goal", ID: goalA, Reason: "unknown_account"}}, res.Refused)
	require.Equal(t, f.a, newestPayload(t, db, goalA)["accountId"])
	require.Empty(t, moveLedger(t, db, goalA, goalB, runA, campaign))
}

func TestAccountMovePreviewWritesNothing(t *testing.T) {
	engine, db, _ := readMergeTestEngine(t)
	ctx := context.Background()
	f := newMoveFixture(t, db, t.Name())
	g := f.goal(t, db, "goal", f.a)
	r1 := f.run(t, db, "run1", g, f.a)
	f.run(t, db, "run2", g, f.a)
	f.row(t, db, "v1:work:step", "step", map[string]any{"ownerUserId": f.owner, "runId": r1, "accountId": f.a})
	f.run(t, db, "foreign", g, f.c)
	before := snapshotPayloads(t, db, []string{g, r1})

	res, err := engine.MoveAccountRows(ctx, db, MoveRequest{Rows: []AccountRowRef{{Concept: "v1:work:goal", ID: g}}, To: f.b, By: f.owner, Preview: true})
	require.NoError(t, err)
	require.Equal(t, []MoveLine{{Concept: "v1:work:goal", Rows: 1}}, res.Moved)
	require.Equal(t, []MoveLine{{Concept: "v1:work:run", Rows: 2}, {Concept: "v1:work:step", Rows: 1}}, res.Followers)
	require.Empty(t, res.Refused)
	require.Equal(t, before, snapshotPayloads(t, db, []string{g, r1}))
	require.Empty(t, moveLedger(t, db, g))
	require.Zero(t, pendingFor(t, db, f.b))
}

func TestAccountMoveMovesRootsAndTheirFollowers(t *testing.T) {
	engine, db, _ := readMergeTestEngine(t)
	ctx := context.Background()
	f := newMoveFixture(t, db, t.Name())
	g1, g2 := f.goal(t, db, "goal1", f.a), f.goal(t, db, "goal2", f.a)
	r1 := f.run(t, db, "run1", g1, f.a)
	r2 := f.run(t, db, "run2", g2, f.a)

	res, err := engine.MoveAccountRows(ctx, db, MoveRequest{
		Rows: []AccountRowRef{{Concept: "v1:work:goal", ID: g1}, {Concept: "v1:work:goal", ID: g2}},
		To:   f.b, By: f.owner, Note: "client handover",
	})
	require.NoError(t, err)
	require.Equal(t, []MoveLine{{Concept: "v1:work:goal", Rows: 2}}, res.Moved)
	require.Equal(t, []MoveLine{{Concept: "v1:work:run", Rows: 2}}, res.Followers)
	require.Equal(t, f.b, newestPayload(t, db, g1)["accountId"])
	require.Equal(t, f.b, newestPayload(t, db, g2)["accountId"])
	require.Equal(t, []string{g1 + " " + f.a + ">" + f.b + " 1", g2 + " " + f.a + ">" + f.b + " 1"}, moveLedger(t, db, g1, g2))
	var note, by string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT note, moved_by FROM account_row_moves WHERE row_id = ?`, g1).Scan(&note, &by))
	require.Equal(t, "client handover", note)
	require.Equal(t, f.owner, by)
	require.EqualValues(t, 2, pendingFor(t, db, f.b))

	runFollowUntilDone(t, engine, db)
	require.Equal(t, f.b, newestPayload(t, db, r1)["accountId"])
	require.Equal(t, f.b, newestPayload(t, db, r2)["accountId"])
}

func TestAccountMovePlacesRowsWithNoAccount(t *testing.T) {
	engine, db, _ := readMergeTestEngine(t)
	ctx := context.Background()
	f := newMoveFixture(t, db, t.Name())
	todo := f.row(t, db, "v1:todos:todo", "todo", map[string]any{"ownerUserId": f.owner, "title": "t"})

	res, err := engine.MoveAccountRows(ctx, db, MoveRequest{Rows: []AccountRowRef{{Concept: "v1:todos:todo", ID: todo}}, To: f.b, By: f.owner})
	require.NoError(t, err)
	require.Equal(t, []MoveLine{{Concept: "v1:todos:todo", Rows: 1}}, res.Moved)
	require.Equal(t, f.b, newestPayload(t, db, todo)["accountId"])
	require.Equal(t, []string{todo + " >" + f.b + " 0"}, moveLedger(t, db, todo))

	t.Run("a stated root carries the rows tied to it that have none", func(t *testing.T) {
		audience := f.row(t, db, "v1:campaigns:audience", "audience", map[string]any{"name": "a"})
		template := f.row(t, db, "v1:campaigns:template", "template", map[string]any{"name": "t", "accountId": accountCanonicalPrefix + f.c})
		campaign := f.row(t, db, "v1:campaigns:campaign", "campaign", map[string]any{
			"name": "c", "audienceId": audience, "templateId": template,
		})
		res, err := engine.MoveAccountRows(ctx, db, MoveRequest{Rows: []AccountRowRef{{Concept: "v1:campaigns:campaign", ID: campaign}}, To: f.b, By: f.owner})
		require.NoError(t, err)
		require.Equal(t, []MoveLine{{Concept: "v1:campaigns:campaign", Rows: 1}}, res.Moved)
		require.Equal(t, []MoveLine{{Concept: "v1:campaigns:audience", Rows: 1}}, res.Followers)
		require.Equal(t, accountCanonicalPrefix+f.b, newestPayload(t, db, campaign)["accountId"], "a stated kind keeps its writers' spelling")
		require.Equal(t, accountCanonicalPrefix+f.b, newestPayload(t, db, audience)["accountId"])
		require.Equal(t, accountCanonicalPrefix+f.c, newestPayload(t, db, template)["accountId"], "a tied row with an account is not moved")
	})
}

// A row whose account changed between the move's read and its write is left
// as the other writer left it, and nothing records a move of it.
func TestAccountMoveGuardSkipsARowChangedMeanwhile(t *testing.T) {
	engine, db, _ := readMergeTestEngine(t)
	ctx := context.Background()
	f := newMoveFixture(t, db, t.Name())
	g := f.goal(t, db, "goal", f.a)
	accountMoveBeforeWrite = func() {
		_, err := db.ExecContext(context.Background(), `UPDATE "MemoryNodes" SET payload = jsonb_set(payload, '{accountId}', to_jsonb(?::text)) WHERE id = ?`, f.c, g)
		require.NoError(t, err)
	}
	t.Cleanup(func() { accountMoveBeforeWrite = nil })

	res, err := engine.MoveAccountRows(ctx, db, MoveRequest{Rows: []AccountRowRef{{Concept: "v1:work:goal", ID: g}}, To: f.b, By: f.owner})
	require.NoError(t, err)
	require.Empty(t, res.Moved)
	require.Equal(t, f.c, newestPayload(t, db, g)["accountId"])
	require.Empty(t, moveLedger(t, db, g))
	require.Zero(t, pendingFor(t, db, f.b))
}
