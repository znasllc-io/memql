package memql

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
)

// FOLLOWERS FOLLOW, AGAINST REAL TABLES (account_follow.go).
//
// A goal is moved from one account to another by hand (its newest version is
// rewritten in place, as the move does), a follow task is recorded for it, and
// the runner's follow batches are called until no task is left. Its runs and
// their steps follow; a run attached to the goal from another account does not.
// Postgres-gated like their neighbours.

type followFixture struct {
	a, b, c         string
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
	f := followFixture{a: "fa-" + s, b: "fb-" + s, c: "fc-" + s}
	at := time.Now().Add(-time.Hour)
	raw := func(concept, id string, payload map[string]any) string {
		full := concept + ":" + id
		seedRawRow(t, ctx, db, concept, full, at, "follow-test", payload)
		return full
	}
	owner := "follow-owner-" + s
	f.goal = raw("v1:work:goal", "goal-"+s, map[string]any{"ownerUserId": owner, "title": "g", "accountId": f.a})
	f.run1 = raw("v1:work:run", "run1-"+s, map[string]any{"ownerUserId": owner, "goalId": f.goal, "accountId": f.a})
	// The second run names its goal bare, as some writers store it.
	f.run2 = raw("v1:work:run", "run2-"+s, map[string]any{"ownerUserId": owner, "goalId": "goal-" + s, "accountId": f.a})
	f.step1 = raw("v1:work:step", "step1-"+s, map[string]any{"ownerUserId": owner, "runId": f.run1, "accountId": f.a})
	f.step2 = raw("v1:work:step", "step2-"+s, map[string]any{"ownerUserId": owner, "runId": f.run2, "accountId": f.a})
	// Attached to the goal from another account: row ownership allows it, and
	// it is not a follower.
	f.foreignRun = raw("v1:work:run", "foreign-run-"+s, map[string]any{"ownerUserId": owner, "goalId": f.goal, "accountId": f.c})
	f.foreignRunsStep = raw("v1:work:step", "foreign-step-"+s, map[string]any{"ownerUserId": owner, "runId": f.foreignRun, "accountId": f.c})
	return f
}

// moveByHand rewrites a row's newest version's account in place and records
// the follow task, as the move does.
func moveByHand(t *testing.T, db *bun.DB, id, concept, from, to string) {
	t.Helper()
	ctx := context.Background()
	_, err := db.ExecContext(ctx, `UPDATE "MemoryNodes" SET payload = jsonb_set(payload, '{accountId}', to_jsonb(?::text))
		WHERE id = ? AND "createdAt" = (SELECT max("createdAt") FROM "MemoryNodes" WHERE id = ?)`, to, id, id)
	require.NoError(t, err)
	require.NoError(t, EnqueueAccountFollow(ctx, db, AccountRowRef{Concept: concept, ID: id}, from, to))
}

// runFollowUntilDone calls the follow batch as the runner does until nothing
// is pending.
func runFollowUntilDone(t *testing.T, engine *MemQLEngine, db *bun.DB) {
	t.Helper()
	for i := 1; i <= 500; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), accountOwnershipDeadline)
		err := engine.runAccountFollowBatch(ctx)
		cancel()
		require.NoError(t, err)
		n, err := AccountFollowPending(context.Background(), db.DB, "")
		require.NoError(t, err)
		if n == 0 {
			return
		}
	}
	t.Fatal("the follow tasks never finished")
}

func (f followFixture) accounts(t *testing.T, db *bun.DB) map[string]any {
	t.Helper()
	out := map[string]any{}
	for _, id := range []string{f.goal, f.run1, f.run2, f.step1, f.step2, f.foreignRun, f.foreignRunsStep} {
		out[id] = newestPayload(t, db, id)["accountId"]
	}
	return out
}

func TestAccountFollowersFollowTheirParent(t *testing.T) {
	engine, db, _ := readMergeTestEngine(t)
	ctx := context.Background()
	f := newFollowFixture(t, db, t.Name())

	moveByHand(t, db, f.goal, "v1:work:goal", f.a, f.b)
	n, err := AccountFollowPending(ctx, db.DB, f.b)
	require.NoError(t, err)
	require.EqualValues(t, 1, n, "the move records one task naming the account it went to")
	n, err = AccountFollowPending(ctx, db.DB, f.a)
	require.NoError(t, err)
	require.EqualValues(t, 1, n, "and the account it left")

	runFollowUntilDone(t, engine, db)
	require.Equal(t, map[string]any{
		f.goal: f.b, f.run1: f.b, f.run2: f.b, f.step1: f.b, f.step2: f.b,
		f.foreignRun: f.c, f.foreignRunsStep: f.c,
	}, f.accounts(t, db))

	t.Run("a second run changes nothing", func(t *testing.T) {
		before := snapshotPayloads(t, db, []string{f.goal, f.run1, f.run2, f.step1, f.step2, f.foreignRun, f.foreignRunsStep})
		require.NoError(t, EnqueueAccountFollow(ctx, db, AccountRowRef{Concept: "v1:work:goal", ID: f.goal}, f.a, f.b))
		runFollowUntilDone(t, engine, db)
		require.Equal(t, before, snapshotPayloads(t, db, []string{f.goal, f.run1, f.run2, f.step1, f.step2, f.foreignRun, f.foreignRunsStep}))
	})
}

// A task cut short by its batch resumes where it stopped, on any replica.
func TestAccountFollowResumesABatchCutShort(t *testing.T) {
	engine, db, _ := readMergeTestEngine(t)
	ctx := context.Background()
	f := newFollowFixture(t, db, t.Name())
	saved := accountFollowBatchRows
	accountFollowBatchRows = 1
	t.Cleanup(func() { accountFollowBatchRows = saved })

	moveByHand(t, db, f.goal, "v1:work:goal", f.a, f.b)
	worked, _, err := engine.followOneBatch(ctx, db)
	require.NoError(t, err)
	require.True(t, worked)
	var followed int
	var cursor string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT followed, cursor_id FROM account_follow_tasks
		WHERE parent_id = ? AND done_at IS NULL`, f.goal).Scan(&followed, &cursor))
	require.Equal(t, 1, followed, "one batch of one row")
	require.NotEmpty(t, cursor, "the cursor was not kept")

	runFollowUntilDone(t, engine, db)
	require.Equal(t, map[string]any{
		f.goal: f.b, f.run1: f.b, f.run2: f.b, f.step1: f.b, f.step2: f.b,
		f.foreignRun: f.c, f.foreignRunsStep: f.c,
	}, f.accounts(t, db))
}
