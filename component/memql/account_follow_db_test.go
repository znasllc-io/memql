package memql

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
)

// FOLLOWERS FOLLOW, AGAINST REAL TABLES (account_follow.go, and the follow and
// re-apply rules of account_assignment_rules.go).
//
// A goal is moved through the governed move, which records the change and asks
// the job for full passes of the kinds below a goal. The job is then called as
// the runner calls it until those passes have finished. Postgres-gated like
// their neighbours.

type followFixture struct {
	a, b, c         string
	owner           string
	goal            string
	run1, run2      string
	step1, step2    string
	foreignRun      string
	foreignRunsStep string
}

func newFollowFixture(t *testing.T, db *bun.DB, name string) followFixture {
	t.Helper()
	ctx := context.Background()
	s := uniqueSuffix(name)
	f := followFixture{a: "fa-" + s, b: "fb-" + s, c: "fc-" + s, owner: "follow-owner-" + s}
	at := time.Now().Add(-time.Hour)
	for _, account := range []string{f.a, f.b, f.c} {
		seedRawRow(t, ctx, db, conceptAccountsAccount, conceptAccountsAccount+":"+account, at.Add(-time.Hour), "follow-test",
			map[string]any{"name": account, "status": "active"})
	}
	raw := func(concept, id string, payload map[string]any) string {
		full := concept + ":" + id
		seedRawRow(t, ctx, db, concept, full, at, "follow-test", payload)
		return full
	}
	f.goal = raw("v1:work:goal", "goal-"+s, map[string]any{"ownerUserId": f.owner, "title": "g", "accountId": f.a})
	f.run1 = raw("v1:work:run", "run1-"+s, map[string]any{"ownerUserId": f.owner, "goalId": f.goal, "accountId": f.a})
	// The second run names its goal bare, as some writers store it.
	f.run2 = raw("v1:work:run", "run2-"+s, map[string]any{"ownerUserId": f.owner, "goalId": "goal-" + s, "accountId": f.a})
	f.step1 = raw("v1:work:step", "step1-"+s, map[string]any{"ownerUserId": f.owner, "runId": f.run1, "accountId": f.a})
	f.step2 = raw("v1:work:step", "step2-"+s, map[string]any{"ownerUserId": f.owner, "runId": f.run2, "accountId": f.a})
	// Attached to the goal from another account: row ownership allows it, and
	// it is not a follower.
	f.foreignRun = raw("v1:work:run", "foreign-run-"+s, map[string]any{"ownerUserId": f.owner, "goalId": f.goal, "accountId": f.c})
	f.foreignRunsStep = raw("v1:work:step", "foreign-step-"+s, map[string]any{"ownerUserId": f.owner, "runId": f.foreignRun, "accountId": f.c})
	return f
}

func (f followFixture) followers() []string {
	return []string{f.run1, f.run2, f.step1, f.step2}
}

func (f followFixture) accounts(t *testing.T, db *bun.DB) map[string]any {
	t.Helper()
	out := map[string]any{}
	for _, id := range []string{f.goal, f.run1, f.run2, f.step1, f.step2, f.foreignRun, f.foreignRunsStep} {
		out[id] = newestPayload(t, db, id)["accountId"]
	}
	return out
}

// recordingEngine is an engine that records accounts, put back when the test ends.
func recordingEngine(t *testing.T) (*MemQLEngine, *bun.DB) {
	t.Helper()
	engine, db, _ := readMergeTestEngine(t)
	engine.SetAccountStampingEnabled(true)
	t.Cleanup(func() { engine.SetAccountStampingEnabled(false) })
	_, order, err := assignmentConcepts()
	require.NoError(t, err)
	require.NoError(t, ensureAssignmentProgressRows(context.Background(), db, order))
	return engine, db
}

// runJobUntil calls the job as the runner does until done reports true.
func runJobUntil(t *testing.T, engine *MemQLEngine, db *bun.DB, what string, done func() bool) {
	t.Helper()
	for i := 0; i < 3000; i++ {
		if done() {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), accountOwnershipDeadline)
		err := engine.runAccountAssignmentBatch(ctx)
		cancel()
		require.NoError(t, err)
	}
	t.Fatalf("the job never got to: %s", what)
}

// fullPassPending reads FullPassPending for one kind.
func fullPassPending(t *testing.T, engine *MemQLEngine, db *bun.DB, concept string) bool {
	t.Helper()
	states, err := engine.AccountAssignmentStates(context.Background(), db.DB)
	require.NoError(t, err)
	for _, st := range states {
		if st.Concept == concept {
			return st.FullPassPending
		}
	}
	t.Fatalf("no state for %s", concept)
	return false
}

func changeRecordsFor(t *testing.T, db *bun.DB, id string) []string {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), `SELECT from_account || '>' || to_account FROM account_follow_tasks
		WHERE parent_id = ? ORDER BY id`, id)
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

func moveOne(t *testing.T, engine *MemQLEngine, db *bun.DB, concept, id, to string) {
	t.Helper()
	res, err := engine.MoveAccountRows(context.Background(), db, MoveRequest{Rows: []AccountRowRef{{Concept: concept, ID: id}}, To: to, By: "follow-test"})
	require.NoError(t, err)
	require.Equal(t, []MoveLine{{Concept: concept, Rows: 1}}, res.Moved, "refused: %v", res.Refused)
}

func TestAccountFollowersFollowTheirParent(t *testing.T) {
	engine, db := recordingEngine(t)
	ctx := context.Background()
	f := newFollowFixture(t, db, t.Name())
	// No requested pass left over from another test of the package, so the
	// one this move asks for is the one observed.
	runJobUntil(t, engine, db, "no pass pending", func() bool {
		return !fullPassPending(t, engine, db, "v1:work:run") && !fullPassPending(t, engine, db, "v1:work:step")
	})

	moveOne(t, engine, db, "v1:work:goal", f.goal, f.b)
	require.Equal(t, []string{f.a + ">" + f.b}, changeRecordsFor(t, db, f.goal))
	require.EqualValues(t, 1, pendingFor(t, db, f.b), "the move records the goal's change")
	require.True(t, fullPassPending(t, engine, db, "v1:work:run"), "the move asks for a full pass of the runs")
	require.True(t, fullPassPending(t, engine, db, "v1:work:step"), "and of the kinds below them")

	runJobUntil(t, engine, db, "the followers' passes finished", func() bool {
		n, err := AccountFollowPending(ctx, db.DB, f.b)
		require.NoError(t, err)
		if n > 0 {
			return false
		}
		require.False(t, fullPassPending(t, engine, db, "v1:work:run"), "the records settled before the runs' requested pass finished")
		require.False(t, fullPassPending(t, engine, db, "v1:work:step"), "the records settled before the steps' requested pass finished")
		return true
	})
	require.Equal(t, map[string]any{
		f.goal: f.b, f.run1: f.b, f.run2: f.b, f.step1: f.b, f.step2: f.b,
		f.foreignRun: f.c, f.foreignRunsStep: f.c,
	}, f.accounts(t, db))
	for _, id := range f.followers() {
		require.Equal(t, []string{f.a + ">" + f.b}, changeRecordsFor(t, db, id), "%s carries a change record of its own", id)
	}
	require.Empty(t, changeRecordsFor(t, db, f.foreignRun))
}

// A goal moved twice before any pass ran: every follower ends where the goal
// is now, never where it once went.
func TestAccountFollowersFollowAParentMovedTwice(t *testing.T) {
	engine, db := recordingEngine(t)
	f := newFollowFixture(t, db, t.Name())
	moveOne(t, engine, db, "v1:work:goal", f.goal, f.b)
	moveOne(t, engine, db, "v1:work:goal", f.goal, f.c)

	runJobUntil(t, engine, db, "the followers' passes finished", func() bool {
		return pendingFor(t, db, f.b) == 0 && pendingFor(t, db, f.c) == 0 && pendingFor(t, db, f.a) == 0
	})
	for _, id := range f.followers() {
		require.Equal(t, f.c, newestPayload(t, db, id)["accountId"], "%s", id)
	}
}

// A follower re-stamped with the account it left, by an ordinary write that
// read it before it was carried, is carried again by the next incremental pass:
// the change record of its parent still stands.
func TestAccountFollowerReStampedByAStaleAppendIsCarriedAgain(t *testing.T) {
	engine, db := recordingEngine(t)
	f := newFollowFixture(t, db, t.Name())
	moveOne(t, engine, db, "v1:work:goal", f.goal, f.b)
	runJobUntil(t, engine, db, "the followers' passes finished", func() bool { return pendingFor(t, db, f.b) == 0 })
	require.Equal(t, f.b, newestPayload(t, db, f.run1)["accountId"])

	seedRawRow(t, context.Background(), db, "v1:work:run", f.run1, time.Now(), "follow-test",
		map[string]any{"ownerUserId": f.owner, "goalId": f.goal, "accountId": f.a, "status": "stale"})
	require.Equal(t, f.a, newestPayload(t, db, f.run1)["accountId"], "fixture: the stale append put the run back")

	makeDue(t, db, "v1:work:run")
	runOnePassOf(t, engine, db, "v1:work:run")
	require.False(t, assignmentProgressOf(t, db, "v1:work:run").passFull, "an incremental pass, not a full one")
	require.Equal(t, f.b, newestPayload(t, db, f.run1)["accountId"])
}

// A moved root re-stamped by a stale append is moved again by the next
// incremental pass of its kind, from the ledger line.
func TestAccountAssignmentReappliesAMoveAfterAStaleAppend(t *testing.T) {
	engine, db := recordingEngine(t)
	f := newFollowFixture(t, db, t.Name())
	moveOne(t, engine, db, "v1:work:goal", f.goal, f.b)

	seedRawRow(t, context.Background(), db, "v1:work:goal", f.goal, time.Now(), "follow-test",
		map[string]any{"ownerUserId": f.owner, "title": "stale", "accountId": f.a})
	require.Equal(t, f.a, newestPayload(t, db, f.goal)["accountId"], "fixture: the stale append put the goal back")

	makeDue(t, db, "v1:work:goal")
	runOnePassOf(t, engine, db, "v1:work:goal")
	require.Equal(t, f.b, newestPayload(t, db, f.goal)["accountId"])
	require.Equal(t, "stale", newestPayload(t, db, f.goal)["title"], "fixture: the stale version is the newest")

	t.Run("a later change of its own is not undone", func(t *testing.T) {
		// The row changes account by a change of its own after the move: that
		// change records itself, and the re-apply rule leaves it.
		_, err := db.ExecContext(context.Background(), `UPDATE "MemoryNodes" SET payload = jsonb_set(payload, '{accountId}', to_jsonb(?::text))
			WHERE id = ? AND "createdAt" = (SELECT max("createdAt") FROM "MemoryNodes" WHERE id = ?)`, f.a, f.goal, f.goal)
		require.NoError(t, err)
		require.NoError(t, EnqueueAccountFollow(context.Background(), db, AccountRowRef{Concept: "v1:work:goal", ID: f.goal}, f.b, f.a))
		require.NoError(t, RequestAccountFullPass(context.Background(), db, []string{"v1:work:goal"}))
		runOnePassOf(t, engine, db, "v1:work:goal")
		require.Equal(t, f.a, newestPayload(t, db, f.goal)["accountId"])
	})
}
