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
	engine, db := recordingEngine(t)
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

// followerLine finds one kind's line in Followers.
func followerLine(res MoveResult, concept string) (MoveLine, bool) {
	for _, l := range res.Followers {
		if l.Concept == concept {
			return l, true
		}
	}
	return MoveLine{}, false
}

// Followers are named, not counted: each line but a tied kind's has Rows zero.
func requireFollowersNamed(t *testing.T, res MoveResult, kinds ...string) {
	t.Helper()
	for _, kind := range kinds {
		l, ok := followerLine(res, kind)
		require.True(t, ok, "%s is not named among the followers: %v", kind, res.Followers)
		require.Zero(t, l.Rows, "followers of %s were counted", kind)
	}
}

func TestAccountMovePreviewWritesNothing(t *testing.T) {
	engine, db := recordingEngine(t)
	ctx := context.Background()
	f := newMoveFixture(t, db, t.Name())
	g := f.goal(t, db, "goal", f.a)
	r1 := f.run(t, db, "run1", g, f.a)
	f.row(t, db, "v1:work:step", "step", map[string]any{"ownerUserId": f.owner, "runId": r1, "accountId": f.a})
	before := snapshotPayloads(t, db, []string{g, r1})
	requested := 0
	accountFullPassRequested = func([]string) { requested++ }
	t.Cleanup(func() { accountFullPassRequested = nil })

	res, err := engine.MoveAccountRows(ctx, db, MoveRequest{Rows: []AccountRowRef{{Concept: "v1:work:goal", ID: g}}, To: f.b, By: f.owner, Preview: true})
	require.NoError(t, err)
	require.Equal(t, []MoveLine{{Concept: "v1:work:goal", Rows: 1}}, res.Moved)
	requireFollowersNamed(t, res, "v1:work:run", "v1:work:step")
	require.Empty(t, res.Refused)
	require.Equal(t, before, snapshotPayloads(t, db, []string{g, r1}))
	require.Empty(t, moveLedger(t, db, g))
	require.Zero(t, pendingFor(t, db, f.b))
	require.Zero(t, requested, "a preview asked the job for passes")
}

func TestAccountMoveMovesRoots(t *testing.T) {
	engine, db := recordingEngine(t)
	ctx := context.Background()
	f := newMoveFixture(t, db, t.Name())
	g1, g2 := f.goal(t, db, "goal1", f.a), f.goal(t, db, "goal2", f.a)
	f.run(t, db, "run1", g1, f.a)

	res, err := engine.MoveAccountRows(ctx, db, MoveRequest{
		Rows: []AccountRowRef{{Concept: "v1:work:goal", ID: g1}, {Concept: "v1:work:goal", ID: g2}},
		To:   f.b, By: f.owner, Note: "client handover",
	})
	require.NoError(t, err)
	require.Equal(t, []MoveLine{{Concept: "v1:work:goal", Rows: 2}}, res.Moved)
	requireFollowersNamed(t, res, "v1:work:run")
	require.Equal(t, f.b, newestPayload(t, db, g1)["accountId"])
	require.Equal(t, f.b, newestPayload(t, db, g2)["accountId"])
	require.Equal(t, []string{g1 + " " + f.a + ">" + f.b + " 0", g2 + " " + f.a + ">" + f.b + " 0"}, moveLedger(t, db, g1, g2))
	var note, by string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT note, moved_by FROM account_row_moves WHERE row_id = ?`, g1).Scan(&note, &by))
	require.Equal(t, "client handover", note)
	require.Equal(t, f.owner, by)
	require.EqualValues(t, 2, pendingFor(t, db, f.b), "one change record per root")
}

func TestAccountMovePlacesRowsWithNoAccount(t *testing.T) {
	engine, db := recordingEngine(t)
	ctx := context.Background()
	f := newMoveFixture(t, db, t.Name())
	todo1 := f.row(t, db, "v1:todos:todo", "todo1", map[string]any{"ownerUserId": f.owner, "title": "t"})
	todo2 := f.row(t, db, "v1:todos:todo", "todo2", map[string]any{"ownerUserId": f.owner, "title": "t"})

	res, err := engine.MoveAccountRows(ctx, db, MoveRequest{Rows: []AccountRowRef{
		{Concept: "v1:todos:todo", ID: todo1}, {Concept: "v1:todos:todo", ID: todo2},
	}, To: f.b, By: f.owner})
	require.NoError(t, err)
	require.Equal(t, []MoveLine{{Concept: "v1:todos:todo", Rows: 2}}, res.Moved)
	require.Equal(t, f.b, newestPayload(t, db, todo1)["accountId"])
	require.Equal(t, []string{todo1 + " >" + f.b + " 0", todo2 + " >" + f.b + " 0"}, moveLedger(t, db, todo1, todo2))
	require.Equal(t, []string{">" + f.b}, changeRecordsFor(t, db, todo1), "a placement's record names no account it left; it is also the request")
	require.EqualValues(t, 2, pendingFor(t, db, f.b))

	t.Run("a stated root carries the rows tied to it that have none", func(t *testing.T) {
		audience := f.row(t, db, "v1:campaigns:audience", "audience", map[string]any{"name": "a"})
		template := f.row(t, db, "v1:campaigns:template", "template", map[string]any{"name": "t", "accountId": accountCanonicalPrefix + f.c})
		campaign := f.row(t, db, "v1:campaigns:campaign", "campaign", map[string]any{
			"name": "c", "audienceId": audience, "templateId": template,
		})
		res, err := engine.MoveAccountRows(ctx, db, MoveRequest{Rows: []AccountRowRef{{Concept: "v1:campaigns:campaign", ID: campaign}}, To: f.b, By: f.owner})
		require.NoError(t, err)
		require.Equal(t, []MoveLine{{Concept: "v1:campaigns:campaign", Rows: 1}}, res.Moved)
		l, ok := followerLine(res, "v1:campaigns:audience")
		require.True(t, ok)
		require.EqualValues(t, 1, l.Rows, "the tied rows are counted")
		require.Equal(t, accountCanonicalPrefix+f.b, newestPayload(t, db, campaign)["accountId"], "a stated kind keeps its writers' spelling")
		require.Equal(t, accountCanonicalPrefix+f.b, newestPayload(t, db, audience)["accountId"])
		require.Equal(t, accountCanonicalPrefix+f.c, newestPayload(t, db, template)["accountId"], "a tied row with an account is not moved")
		require.Equal(t, []string{campaign + " >" + f.b + " 1"}, moveLedger(t, db, campaign))
	})

	t.Run("a row whose parent has no account either follows it", func(t *testing.T) {
		goal := f.goal(t, db, "unplaced-goal", "")
		run := f.row(t, db, "v1:work:run", "unplaced-run", map[string]any{"ownerUserId": f.owner, "goalId": goal})
		res, err := engine.MoveAccountRows(ctx, db, MoveRequest{Rows: []AccountRowRef{{Concept: "v1:work:run", ID: run}}, To: f.b, By: f.owner})
		require.NoError(t, err)
		require.Equal(t, []MoveRefusal{{Concept: "v1:work:run", ID: run, Reason: "follows_parent"}}, res.Refused)
	})
}

// Placing a row awaiting a decision resolves its conflict in the move's own
// transaction, so an export of the accounts it named stops waiting on it; a
// preview resolves nothing.
func TestAccountMovePlacingAConflictResolvesIt(t *testing.T) {
	engine, db := recordingEngine(t)
	ctx := context.Background()
	f := newMoveFixture(t, db, t.Name())
	todo := f.row(t, db, "v1:todos:todo", "todo", map[string]any{"ownerUserId": f.owner, "title": "t"})
	_, err := db.ExecContext(ctx, `INSERT INTO account_assignment_conflict (concept, row_id, kind, detail, candidates)
		VALUES ('v1:todos:todo', ?, 'store_bound_twice', 'test', ?::text[])`, todo, pq.Array([]string{f.b, f.c}))
	require.NoError(t, err)
	naming := func() int64 {
		n, err := ConflictsNaming(ctx, db.DB, f.c)
		require.NoError(t, err)
		return n
	}
	require.EqualValues(t, 1, naming())

	_, err = engine.MoveAccountRows(ctx, db, MoveRequest{Rows: []AccountRowRef{{Concept: "v1:todos:todo", ID: todo}}, To: f.b, By: f.owner, Preview: true})
	require.NoError(t, err)
	require.EqualValues(t, 1, naming(), "a preview resolved a conflict")

	_, err = engine.MoveAccountRows(ctx, db, MoveRequest{Rows: []AccountRowRef{{Concept: "v1:todos:todo", ID: todo}}, To: f.b, By: f.owner})
	require.NoError(t, err)
	require.Zero(t, naming(), "a placed row still holds the export of the accounts its conflict named")
}

// A row whose account changed between the move's read and its write is left
// as the other writer left it, and nothing records a move of it.
func TestAccountMoveGuardSkipsARowChangedMeanwhile(t *testing.T) {
	engine, db := recordingEngine(t)
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

// An ordinary write that read the row before the move APPENDS a version naming
// the account the row is leaving. The move's own re-check writes that version
// too, and the row ends in the destination.
func TestAccountMoveRewritesAVersionAppendedMeanwhile(t *testing.T) {
	engine, db := recordingEngine(t)
	ctx := context.Background()
	f := newMoveFixture(t, db, t.Name())
	g := f.goal(t, db, "goal", f.a)
	accountMoveBeforeWrite = func() {
		seedRawRow(t, context.Background(), db, "v1:work:goal", g, time.Now(), "move-test",
			map[string]any{"ownerUserId": f.owner, "title": "appended", "accountId": f.a})
	}
	t.Cleanup(func() { accountMoveBeforeWrite = nil })

	res, err := engine.MoveAccountRows(ctx, db, MoveRequest{Rows: []AccountRowRef{{Concept: "v1:work:goal", ID: g}}, To: f.b, By: f.owner})
	require.NoError(t, err)
	require.Equal(t, []MoveLine{{Concept: "v1:work:goal", Rows: 1}}, res.Moved)
	require.Equal(t, "appended", newestPayload(t, db, g)["title"], "fixture: the appended version is the newest")
	require.Equal(t, f.b, newestPayload(t, db, g)["accountId"])
	require.Len(t, moveLedger(t, db, g), 1)
}

// While this node does not record accounts the move refuses outright and
// writes nothing.
func TestAccountMoveIsRefusedWhileRecordingIsOff(t *testing.T) {
	engine, db := recordingEngine(t)
	ctx := context.Background()
	f := newMoveFixture(t, db, t.Name())
	g := f.goal(t, db, "goal", f.a)
	engine.SetAccountStampingEnabled(false)

	_, err := engine.MoveAccountRows(ctx, db, MoveRequest{Rows: []AccountRowRef{{Concept: "v1:work:goal", ID: g}}, To: f.b, By: f.owner})
	require.ErrorIs(t, err, ErrAccountRecordingOff)
	require.Equal(t, f.a, newestPayload(t, db, g)["accountId"])
	require.Empty(t, moveLedger(t, db, g))
	require.Zero(t, pendingFor(t, db, f.b))
}

// The rule the lists mark rows by is the rule the move refuses them by.
func TestAccountRowsFollowingTheirParentAgreesWithTheMove(t *testing.T) {
	engine, db := recordingEngine(t)
	ctx := context.Background()
	f := newMoveFixture(t, db, t.Name())
	g := f.goal(t, db, "goal", f.a)
	unplaced := f.goal(t, db, "unplaced", "")
	rows := map[string]string{
		f.run(t, db, "same", g, f.a):  "follows its goal in a",
		f.run(t, db, "other", g, f.c): "attached from c",
		f.row(t, db, "v1:work:run", "none", map[string]any{"ownerUserId": f.owner, "goalId": unplaced}):                                       "both in no account",
		f.row(t, db, "v1:work:run", "orphan", map[string]any{"ownerUserId": f.owner, "goalId": "v1:work:goal:gone-" + f.s, "accountId": f.a}): "a parent that does not exist",
		f.row(t, db, "v1:work:run", "root", map[string]any{"ownerUserId": f.owner, "accountId": f.a}):                                         "no parent",
	}
	var page []RowPayload
	for id := range rows {
		page = append(page, RowPayload{ID: id, Payload: newestPayload(t, db, id)})
	}
	links, err := RowsFollowingTheirParent(ctx, db.DB, "v1:work:run", page)
	require.NoError(t, err)
	require.Len(t, links, len(page))
	for i, link := range links {
		require.Equal(t, page[i].ID, link.ID)
		_, reason, err := engine.judgeMoveRoot(ctx, db.DB, AccountRowRef{Concept: "v1:work:run", ID: link.ID}, f.b)
		require.NoError(t, err)
		require.Equal(t, reason == "follows_parent", link.FollowsParent, "%s (%s): the list and the move disagree", link.ID, rows[link.ID])
		want := rows[link.ID] == "follows its goal in a" || rows[link.ID] == "both in no account"
		require.Equal(t, want, link.FollowsParent, "%s (%s)", link.ID, rows[link.ID])
		if rows[link.ID] != "no parent" {
			require.Equal(t, "v1:work:goal", link.ParentConcept, "%s", rows[link.ID])
		}
	}
}

// The move writes no row of the job's progress: its change records are the
// requests, and the job folds them in itself.
func TestAccountMoveWritesNoProgressRow(t *testing.T) {
	engine, db := recordingEngine(t)
	ctx := context.Background()
	f := newMoveFixture(t, db, t.Name())
	g := f.goal(t, db, "goal", f.a)
	before := progressXmins(t, db)
	_, err := engine.MoveAccountRows(ctx, db, MoveRequest{Rows: []AccountRowRef{{Concept: "v1:work:goal", ID: g}}, To: f.b, By: f.owner})
	require.NoError(t, err)
	require.Equal(t, before, progressXmins(t, db))
}

// The move chases at most moveRecheckRounds newer versions; past that it
// reports the row not moved, and writes no ledger line for it.
func TestAccountMoveChasesAtMostThreeVersions(t *testing.T) {
	engine, db := recordingEngine(t)
	ctx := context.Background()
	f := newMoveFixture(t, db, t.Name())
	g := f.goal(t, db, "goal", f.a)
	writes := 0
	appendOne := func() {
		seedRawRow(t, context.Background(), db, "v1:work:goal", g, time.Now(), "move-test",
			map[string]any{"ownerUserId": f.owner, "title": "appended", "accountId": f.a})
		time.Sleep(2 * time.Millisecond)
	}
	accountMoveBeforeWrite = appendOne
	accountMoveAfterRound = func() { writes++; appendOne() }
	t.Cleanup(func() { accountMoveBeforeWrite, accountMoveAfterRound = nil, nil })

	res, err := engine.MoveAccountRows(ctx, db, MoveRequest{Rows: []AccountRowRef{{Concept: "v1:work:goal", ID: g}}, To: f.b, By: f.owner})
	require.NoError(t, err)
	require.Equal(t, 3, writes, "the move chased a number of versions other than three")
	require.Empty(t, res.Moved)
	require.Equal(t, f.a, newestPayload(t, db, g)["accountId"])
	require.Empty(t, moveLedger(t, db, g))
}
