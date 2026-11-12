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

// foldRequests folds the change records into requests, as each job call does
// first.
func foldRequests(t *testing.T, db *bun.DB) {
	t.Helper()
	managed, _, err := assignmentConcepts()
	require.NoError(t, err)
	require.NoError(t, foldAccountChangeRequests(context.Background(), db, managed))
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
	foldRequests(t, db)
	require.True(t, fullPassPending(t, engine, db, "v1:work:run"), "the job folds the record into a request for the runs' pass")

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

// progressXmins is each progress row's xmin: a row any statement updated has a
// new one.
func progressXmins(t *testing.T, db *bun.DB) map[string]string {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), `SELECT concept, xmin::text FROM account_assignment_progress`)
	require.NoError(t, err)
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var concept, xmin string
		require.NoError(t, rows.Scan(&concept, &xmin))
		out[concept] = xmin
	}
	return out
}

// A root that a stale append put back AFTER its followers' pass ran: re-apply
// moves it again and records that, so the followers' kind is read once more and
// they end with the root.
func TestAccountFollowersFollowARootPutBackByReapply(t *testing.T) {
	engine, db := recordingEngine(t)
	ctx := context.Background()
	f := newFollowFixture(t, db, t.Name())
	moveOne(t, engine, db, "v1:work:goal", f.goal, f.b)
	// The stale append lands before the runs' pass reads the runs.
	seedRawRow(t, ctx, db, "v1:work:goal", f.goal, time.Now(), "follow-test",
		map[string]any{"ownerUserId": f.owner, "title": "stale", "accountId": f.a})
	foldRequests(t, db)
	runOnePassOf(t, engine, db, "v1:work:run")
	require.Equal(t, f.a, newestPayload(t, db, f.run1)["accountId"], "fixture: the runs were read while the goal was back in a")

	makeDue(t, db, "v1:work:goal")
	runOnePassOf(t, engine, db, "v1:work:goal")
	require.Equal(t, f.b, newestPayload(t, db, f.goal)["accountId"], "fixture: re-apply put the goal back in b")
	require.Equal(t, []string{f.a + ">" + f.b, f.a + ">" + f.b}, changeRecordsFor(t, db, f.goal), "re-apply records what it did")

	runJobUntil(t, engine, db, "the followers ended with the root", func() bool {
		return newestPayload(t, db, f.run1)["accountId"] == f.b && newestPayload(t, db, f.step1)["accountId"] == f.b
	})
}

// A child whose own explicit change put it back in the account its parent
// left, after the parent's record, keeps it; a child the job carried and a
// stale append then re-stamped is carried again (the second is
// TestAccountFollowerReStampedByAStaleAppendIsCarriedAgain).
func TestAccountFollowLeavesAChildsOwnReTag(t *testing.T) {
	engine, db := recordingEngine(t)
	ctx := context.Background()
	f := newFollowFixture(t, db, t.Name())
	moveOne(t, engine, db, "v1:work:goal", f.goal, f.b)
	runJobUntil(t, engine, db, "the followers' passes finished", func() bool { return pendingFor(t, db, f.b) == 0 })
	require.Equal(t, f.b, newestPayload(t, db, f.run1)["accountId"])

	// A person puts the run back in a: a new version, and what a person's
	// re-tag writes, a ledger line and a change record.
	seedRawRow(t, ctx, db, "v1:work:run", f.run1, time.Now(), "follow-test",
		map[string]any{"ownerUserId": f.owner, "goalId": f.goal, "accountId": f.a, "status": "re-tagged"})
	_, err := db.ExecContext(ctx, `INSERT INTO account_row_moves (concept, row_id, from_account, to_account, moved_by, followers, note)
		VALUES ('v1:work:run', ?, ?, ?, ?, 0, 're-tag')`, f.run1, f.b, f.a, f.owner)
	require.NoError(t, err)
	require.NoError(t, EnqueueAccountFollow(ctx, db, AccountRowRef{Concept: "v1:work:run", ID: f.run1}, f.b, f.a))

	makeDue(t, db, "v1:work:run")
	runOnePassOf(t, engine, db, "v1:work:run")
	require.Equal(t, f.a, newestPayload(t, db, f.run1)["accountId"], "the follow rule undid the run's own change")
}

// A parent's record carries only rows that existed before the job first saw
// it: a row attached to the moved parent afterwards, filed under the account
// the parent left, is not this record's to move.
func TestAccountFollowCarriesOnlyRowsThatExistedBeforeTheChange(t *testing.T) {
	engine, db := recordingEngine(t)
	ctx := context.Background()
	f := newFollowFixture(t, db, t.Name())
	moveOne(t, engine, db, "v1:work:goal", f.goal, f.b)
	foldRequests(t, db)
	require.True(t, recordSeen(t, db, f.goal), "fixture: the fold saw the goal's record")
	time.Sleep(10 * time.Millisecond)
	late := "v1:work:run:late-" + uniqueSuffix(t.Name())
	seedRawRow(t, ctx, db, "v1:work:run", late, time.Now(), "follow-test",
		map[string]any{"ownerUserId": f.owner, "goalId": f.goal, "accountId": f.a})

	runJobUntil(t, engine, db, "the followers' passes finished", func() bool { return pendingFor(t, db, f.b) == 0 })
	require.Equal(t, f.b, newestPayload(t, db, f.run1)["accountId"], "a run that existed before the move was not carried")
	require.Equal(t, f.a, newestPayload(t, db, late)["accountId"], "a run attached after the record was seen was carried")
}

// recordSeen reports whether every change record of the row has been seen by
// the job's fold.
func recordSeen(t *testing.T, db *bun.DB, id string) bool {
	t.Helper()
	var seen bool
	require.NoError(t, db.QueryRowContext(context.Background(), `SELECT bool_and(seen_at IS NOT NULL) FROM account_follow_tasks
		WHERE parent_id = ?`, id).Scan(&seen))
	return seen
}

// changeGoalInOpenTx changes the goal's account from a to b in place, with its
// change record, in a transaction left open for the caller to commit: a change
// whose record is inserted well before it becomes visible.
func changeGoalInOpenTx(t *testing.T, db *bun.DB, f followFixture) bun.Tx {
	t.Helper()
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	_, err = tx.ExecContext(ctx, `UPDATE "MemoryNodes" SET payload = jsonb_set(payload, '{accountId}', to_jsonb(?::text))
		WHERE id = ? AND "createdAt" = (SELECT max("createdAt") FROM "MemoryNodes" WHERE id = ?)`, f.b, f.goal, f.goal)
	require.NoError(t, err)
	require.NoError(t, EnqueueAccountFollow(ctx, tx, AccountRowRef{Concept: "v1:work:goal", ID: f.goal}, f.a, f.b))
	return tx
}

// A child written while its parent's change was still uncommitted read the
// parent in the account it was leaving, and was filed there. It existed before
// the job could see the record, so the record carries it.
func TestAccountFollowCarriesAChildWrittenWhileTheChangeWasUncommitted(t *testing.T) {
	engine, db := recordingEngine(t)
	ctx := context.Background()
	f := newFollowFixture(t, db, t.Name())
	tx := changeGoalInOpenTx(t, db, f)
	time.Sleep(10 * time.Millisecond)
	// Another connection: the goal still reads as in a, so the run is filed there.
	inWindow := "v1:work:run:window-" + uniqueSuffix(t.Name())
	seedRawRow(t, ctx, db, "v1:work:run", inWindow, time.Now(), "follow-test",
		map[string]any{"ownerUserId": f.owner, "goalId": f.goal, "accountId": f.a})
	time.Sleep(10 * time.Millisecond)
	require.NoError(t, tx.Commit())

	runJobUntil(t, engine, db, "the followers' passes finished", func() bool { return pendingFor(t, db, f.b) == 0 })
	require.Equal(t, f.b, newestPayload(t, db, f.run1)["accountId"])
	require.Equal(t, f.b, newestPayload(t, db, inWindow)["accountId"], "a run written while the change was uncommitted was stranded in a")
}

// A person's own re-tag of a child into the account its parent is leaving,
// made while the parent's change was still uncommitted, is newer than the
// parent's leaving and is kept: a child's own change is compared with when the
// parent's record was written, not with when the job saw it.
func TestAccountFollowKeepsAChildsReTagMadeWhileTheChangeWasUncommitted(t *testing.T) {
	engine, db := recordingEngine(t)
	ctx := context.Background()
	f := newFollowFixture(t, db, t.Name())
	tx := changeGoalInOpenTx(t, db, f)
	time.Sleep(10 * time.Millisecond)
	// The run is in c; a person re-tags it into a, while the goal's change is open.
	seedRawRow(t, ctx, db, "v1:work:run", f.foreignRun, time.Now(), "follow-test",
		map[string]any{"ownerUserId": f.owner, "goalId": f.goal, "accountId": f.a, "status": "re-tagged"})
	_, err := db.ExecContext(ctx, `INSERT INTO account_row_moves (concept, row_id, from_account, to_account, moved_by, followers, note)
		VALUES ('v1:work:run', ?, ?, ?, ?, 0, 're-tag')`, f.foreignRun, f.c, f.a, f.owner)
	require.NoError(t, err)
	require.NoError(t, EnqueueAccountFollow(ctx, db, AccountRowRef{Concept: "v1:work:run", ID: f.foreignRun}, f.c, f.a))
	time.Sleep(10 * time.Millisecond)
	require.NoError(t, tx.Commit())

	runJobUntil(t, engine, db, "the followers' passes finished", func() bool { return pendingFor(t, db, f.b) == 0 && pendingFor(t, db, f.a) == 0 })
	require.Equal(t, f.b, newestPayload(t, db, f.run1)["accountId"])
	require.Equal(t, f.a, newestPayload(t, db, f.foreignRun)["accountId"], "the follow rule undid a person's re-tag made while the change was open")
}

// A record the job has not seen yet carries nothing and settles nothing, even
// once full passes of every follower kind have finished after it committed;
// the export still waits on it. The next call's fold sees it, asks for the
// passes again, and those carry and settle.
func TestAccountChangeRecordNotYetSeenCarriesNothingAndIsNotSettled(t *testing.T) {
	engine, db := recordingEngine(t)
	ctx := context.Background()
	f := newFollowFixture(t, db, t.Name())
	const run = "v1:work:run"
	runJobUntil(t, engine, db, "no pass pending", func() bool { return !fullPassPending(t, engine, db, run) })

	tx := changeGoalInOpenTx(t, db, f)
	// The job runs while the change is uncommitted: its fold cannot see the record.
	foldRequests(t, db)
	require.NoError(t, tx.Commit())
	require.False(t, recordSeen(t, db, f.goal), "fixture: the fold saw an uncommitted record")

	// Full passes of every follower kind, with no fold between: none may use
	// or settle the record.
	for _, kind := range followerKindNames("v1:work:goal") {
		require.NoError(t, RequestAccountFullPass(ctx, db, []string{kind}))
		runOnePassOf(t, engine, db, kind)
	}
	require.Equal(t, f.a, newestPayload(t, db, f.run1)["accountId"], "a record not yet seen carried a run")
	require.EqualValues(t, 1, pendingFor(t, db, f.b), "a record not yet seen was settled")
	require.False(t, fullPassPending(t, engine, db, run), "fixture: the requested pass finished")

	foldRequests(t, db)
	require.True(t, recordSeen(t, db, f.goal))
	require.True(t, fullPassPending(t, engine, db, run), "the fold did not ask for the runs' pass once it saw the record")
	runJobUntil(t, engine, db, "the record settled", func() bool { return pendingFor(t, db, f.b) == 0 })
	require.Equal(t, f.b, newestPayload(t, db, f.run1)["accountId"])
	require.Equal(t, f.b, newestPayload(t, db, f.step1)["accountId"])
}

// An in-place change of account holds the list with it, so members of the
// account a row left stop reading it when it leaves: on the moved root, and on
// a carried Library index row. The index rows here have parents of two kinds,
// a goal and a to-do, both moved.
func TestAccountMovedAndCarriedRowsHoldTheirList(t *testing.T) {
	engine, db := recordingEngine(t)
	ctx := context.Background()
	s := uniqueSuffix(t.Name())
	f := newFollowFixture(t, db, t.Name())
	at := time.Now().Add(-time.Hour)
	tagged := "v1:work:goal:tagged-" + s
	seedRawRow(t, ctx, db, "v1:work:goal", tagged, at, "follow-test",
		map[string]any{"ownerUserId": f.owner, "title": "t", "accountId": f.a, "accountIds": []string{f.a}})
	todo := "v1:todos:todo:parent-" + s
	seedRawRow(t, ctx, db, "v1:todos:todo", todo, at, "follow-test",
		map[string]any{"ownerUserId": f.owner, "title": "t", "accountId": f.a})
	const index = "v1:library:artifact"
	ofGoal, ofTodo := index+":of-goal-"+s, index+":of-todo-"+s
	seedRawRow(t, ctx, db, index, ofGoal, at, "follow-test",
		map[string]any{"ownerUserId": f.owner, "sourceConceptRef": tagged, "accountId": f.a, "accountIds": []string{f.a}})
	seedRawRow(t, ctx, db, index, ofTodo, at, "follow-test",
		map[string]any{"ownerUserId": f.owner, "sourceConceptRef": todo, "accountId": f.a, "accountIds": []string{f.a}})

	moveOne(t, engine, db, "v1:work:goal", tagged, f.b)
	require.Equal(t, []any{f.b}, newestPayload(t, db, tagged)["accountIds"], "the moved root's list still names the account it left")
	moveOne(t, engine, db, "v1:todos:todo", todo, f.b)

	runJobUntil(t, engine, db, "the index rows followed", func() bool {
		return newestPayload(t, db, ofGoal)["accountId"] == f.b && newestPayload(t, db, ofTodo)["accountId"] == f.b
	})
	require.Equal(t, []any{f.b}, newestPayload(t, db, ofGoal)["accountIds"], "a carried row's list still names the account it left")
	require.Equal(t, []any{f.b}, newestPayload(t, db, ofTodo)["accountIds"])
}

// A change record written in a transaction that commits after a full pass of
// its follower kind began is not settled by that pass, and the kind's pass
// stays pending: the pass could not see the record's followers as they were.
func TestAccountChangeRecordCommittedLateIsNotSettledByAPassAlreadyRunning(t *testing.T) {
	engine, db := recordingEngine(t)
	ctx := context.Background()
	f := newFollowFixture(t, db, t.Name())
	const run = "v1:work:run"
	runJobUntil(t, engine, db, "no pass pending", func() bool { return !fullPassPending(t, engine, db, run) })

	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	require.NoError(t, EnqueueAccountFollow(ctx, tx, AccountRowRef{Concept: "v1:work:goal", ID: f.goal}, f.a, f.b))
	time.Sleep(20 * time.Millisecond)

	// A full pass of the runs begins while the record is uncommitted, and
	// finishes after it commits.
	require.NoError(t, RequestAccountFullPass(ctx, db, []string{run}))
	_, err = db.ExecContext(ctx, `UPDATE account_assignment_progress SET batch_size = 1 WHERE concept = ?`, run)
	require.NoError(t, err)
	first, err := engine.assignOneBatch(ctx, db, []string{run}, assignmentBatchIds)
	require.NoError(t, err)
	require.True(t, first.requested && !first.finished, "fixture: the pass takes more than one batch")
	require.NoError(t, tx.Commit())
	runOnePassOf(t, engine, db, run)
	// Every other follower kind of a goal passes in full after the commit, so
	// only the runs' pass stands between the record and settling.
	for _, kind := range followerKindNames("v1:work:goal") {
		if kind == run {
			continue
		}
		require.NoError(t, RequestAccountFullPass(ctx, db, []string{kind}))
		runOnePassOf(t, engine, db, kind)
	}

	var settled bool
	require.NoError(t, db.QueryRowContext(ctx, `SELECT done_at IS NOT NULL FROM account_follow_tasks WHERE parent_id = ?`, f.goal).Scan(&settled))
	require.False(t, settled, "a pass that began before the record committed settled it")
	foldRequests(t, db)
	require.True(t, fullPassPending(t, engine, db, run), "the runs' pass was not owed again")
}

// A follower whose parent was deleted stays in the account the parent left: a
// tombstone is no parent, so the follow rule has nothing to follow.
func TestAccountFollowerOfADeletedParentStays(t *testing.T) {
	engine, db := recordingEngine(t)
	ctx := context.Background()
	f := newFollowFixture(t, db, t.Name())
	moveOne(t, engine, db, "v1:work:goal", f.goal, f.b)
	seedRawRow(t, ctx, db, "v1:work:goal", f.goal, time.Now(), "follow-test", map[string]any{"deleted": true, "accountId": f.b})

	runJobUntil(t, engine, db, "the record settled", func() bool { return pendingFor(t, db, f.b) == 0 })
	require.Equal(t, f.a, newestPayload(t, db, f.run1)["accountId"])
}

// A follower the job carried in the same instant as a second move of its
// parent is carried again: the job's own carry record is not the child's
// choice, and only a person's explicit change (a ledger line) protects a child.
// The sequence: the goal moves a to b; a person moves it b to c; the job's carry
// of the run (a to b) commits after that second move, so the run's own record
// is newer than the goal's record that it left b.
func TestAccountFollowCarriesAChildPastAParentMovedWhileItWasCarried(t *testing.T) {
	engine, db := recordingEngine(t)
	ctx := context.Background()
	f := newFollowFixture(t, db, t.Name())
	moveOne(t, engine, db, "v1:work:goal", f.goal, f.b)
	moveOne(t, engine, db, "v1:work:goal", f.goal, f.c)
	time.Sleep(10 * time.Millisecond)
	// The carry as it lands: the run in b, and its carry record, after the
	// goal's second move.
	_, err := db.ExecContext(ctx, `UPDATE "MemoryNodes" SET payload = jsonb_set(payload, '{accountId}', to_jsonb(?::text))
		WHERE id = ? AND "createdAt" = (SELECT max("createdAt") FROM "MemoryNodes" WHERE id = ?)`, f.b, f.run1, f.run1)
	require.NoError(t, err)
	require.NoError(t, EnqueueAccountFollow(ctx, db, AccountRowRef{Concept: "v1:work:run", ID: f.run1}, f.a, f.b))

	runJobUntil(t, engine, db, "the followers' passes finished", func() bool {
		return pendingFor(t, db, f.b) == 0 && pendingFor(t, db, f.c) == 0
	})
	require.Equal(t, f.c, newestPayload(t, db, f.run1)["accountId"], "a run the job carried was stranded in the account its goal left")
	require.Equal(t, f.c, newestPayload(t, db, f.step1)["accountId"])
}

// While a transaction held open keeps the late-commit horizon behind a change
// record, a full pass of its follower kind cannot settle it. One such pass is
// asked for, not one a tick: the next pass is incremental for as long as the
// transaction stays open. Once it ends, one more full pass runs, and the record
// settles.
func TestAccountHeldHorizonAsksForOneFullPassNotOneATick(t *testing.T) {
	engine, db := recordingEngine(t)
	ctx := context.Background()
	f := newFollowFixture(t, db, t.Name())
	const run = "v1:work:run"
	runJobUntil(t, engine, db, "no pass pending", func() bool {
		makeDue(t, db, run)
		return !fullPassPending(t, engine, db, run)
	})

	held, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = held.Rollback() })
	var one int
	require.NoError(t, held.QueryRowContext(ctx, `SELECT 1`).Scan(&one))
	time.Sleep(20 * time.Millisecond)
	moveOne(t, engine, db, "v1:work:goal", f.goal, f.b)

	tick := func() bool {
		t.Helper()
		foldRequests(t, db)
		makeDue(t, db, run)
		runOnePassOf(t, engine, db, run)
		return assignmentProgressOf(t, db, run).passFull
	}
	require.True(t, tick(), "the first pass after the record is the full pass it asked for")
	require.False(t, tick(), "a second full pass ran while the transaction still held the horizon")
	require.False(t, tick())
	goalSettled := func() bool {
		t.Helper()
		var settled bool
		require.NoError(t, db.QueryRowContext(ctx, `SELECT bool_and(done_at IS NOT NULL) FROM account_follow_tasks WHERE parent_id = ?`, f.goal).Scan(&settled))
		return settled
	}
	require.False(t, goalSettled(), "the goal's record settled while the horizon was held")
	require.Equal(t, f.b, newestPayload(t, db, f.run1)["accountId"], "the one full pass carried the runs")

	require.NoError(t, held.Rollback())
	require.True(t, tick(), "no full pass ran once the transaction had ended")
	require.False(t, fullPassPending(t, engine, db, run), "the pass after the transaction did not cover the request")
	var startedAt time.Time
	require.NoError(t, db.QueryRowContext(ctx, `SELECT full_pass_started_at FROM account_assignment_progress WHERE concept = ?`, run).Scan(&startedAt))
	runJobUntil(t, engine, db, "the record settled", goalSettled)
	var startedAfter time.Time
	require.NoError(t, db.QueryRowContext(ctx, `SELECT full_pass_started_at FROM account_assignment_progress WHERE concept = ?`, run).Scan(&startedAfter))
	require.Equal(t, startedAt, startedAfter, "the runs were walked in full again after the one that settled the record")
	require.Equal(t, f.b, newestPayload(t, db, f.run1)["accountId"])
}
