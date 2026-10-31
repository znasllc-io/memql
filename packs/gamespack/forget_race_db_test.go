package gamespack

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
)

// forget_race_db_test.go -- a forget and a ladder move of the same skill, at the same time.
//
// A ladder move holds the write gate games-skill:<id> from its fresh read to its write. A
// forget that took no gate could delete a skill while a move that had read it was still to
// write: the move's new version commits after the DELETE's snapshot, so the DELETE never sees
// it, and the skill survives with the words that name the player, while forget answers that
// it deleted the skill. So a forget holds the gate of every skill it is about to delete until
// its transaction ends, on the same lock the moves take (a transaction-scoped advisory lock).

// writeGateClass is the class of the advisory lock every write gate takes (component/memql
// write_gate.go: pg_advisory_xact_lock(1296387409, hashtext(key))).
const writeGateClass = 1296387409

// arrivalPatience is how long a test waits for something to ARRIVE: a forget to reach the
// hook the test holds it at, a ladder move to reach a gate, a call to finish. Every such wait
// is event-driven (a channel, or a poll that stops the moment its answer is in), so a long
// patience costs nothing when things go well, and what it buys is that a slow lane cannot
// turn slowness into a failure. The first of these tests in a process is the slow one (engine
// start and a cold first forget): 41 s under the race detector on a loaded machine, and 7 s
// for the next. It is not a margin for the gate itself: a write gate gives a waiter six
// seconds from the moment it arrives, so what happens after an arrival is seen is done at once.
const arrivalPatience = 3 * time.Minute

// gateWaiters counts the sessions that have asked for the write gate of key and are waiting
// for it: advisory lock requests not yet granted.
func (lg *liveGames) gateWaiters(t *testing.T, key string) int {
	t.Helper()
	var n int
	if err := lg.db.DB.QueryRow(`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted
		AND objsubid = 2 AND classid = $1::oid AND objid = hashtext($2)::oid
		AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`, writeGateClass, key).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// waitForGateWaiter waits until somebody is waiting for the gate of key, and says so. It gives
// up when stopped says the call that should have been waiting has finished instead (nothing
// stopped it, which is the answer), and after arrivalPatience. How long it takes the waiter to
// ARRIVE is the machine's to say, and under load it is seconds; once it has arrived a write gate
// holds it for six, and this sees it within a few milliseconds.
func (lg *liveGames) waitForGateWaiter(t *testing.T, key string, stopped func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(arrivalPatience)
	for time.Now().Before(deadline) {
		if lg.gateWaiters(t, key) > 0 {
			return true
		}
		if stopped != nil && stopped() {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// finishedOf reports, without blocking, whether the call has answered, and keeps the answer.
func finishedOf(done <-chan outcome, answer **outcome) func() bool {
	return func() bool {
		select {
		case o := <-done:
			*answer = &o
			return true
		default:
			return false
		}
	}
}

// outcome is what a call that ran in a goroutine answered.
type outcome struct {
	rows []map[string]any
	err  error
}

// inGoroutine runs one builtin on its own goroutine and delivers what it answered.
func (lg *liveGames) inGoroutine(ctx context.Context, name string, args map[string]any) <-chan outcome {
	done := make(chan outcome, 1)
	go func() {
		rows, err := lg.execute(ctx, "builtin", name, args)
		done <- outcome{rows, err}
	}()
	return done
}

// heldStore holds a ladder move between its fresh read of the skill and its write, until the
// test lets it go: a move in flight, with its gate in hand.
type heldStore struct {
	rowStore
	once    sync.Once
	reading chan struct{} // closed when the move has read the skill
	release chan struct{} // closed by the test to let the move go on
}

func (h *heldStore) query(ctx context.Context, name string, args map[string]any) ([]map[string]any, error) {
	rows, err := h.rowStore.query(ctx, name, args)
	if name == "gamesSkillById" {
		h.once.Do(func() { close(h.reading) })
		<-h.release
	}
	return rows, err
}

// A forget with its transaction open -- the skill's rows deleted, and its vectors, nothing
// committed, as it is the moment before its commit -- and a ladder move of the same skill that
// starts then. The move must wait for the forget: its gate is the forget's until the commit.
// When the forget has committed the move finds no skill and passes it over, and the skill is
// gone, with its vector. Without the gate the move read the skill (the delete was not yet
// committed), wrote a new version the delete's snapshot never saw, and the skill survived
// with the words that name the player.
func TestLiveAForgetFencesTheSkillsItDeletesAgainstALadderMove(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	owner := "user-games-forget-race-" + n
	ctx := asOwner(owner)
	t.Cleanup(func() {
		if _, err := lg.db.DB.Exec(`DELETE FROM "MemoryNodes" WHERE concept = $1 AND payload->>'ownerUserId' = $2`, conceptSkill, ownerKey(owner)); err != nil {
			t.Logf("cleanup: %v", err)
		}
	})
	tag := "RaceTag" + n
	goal := "follow " + tag + " home"
	player := "p-race-" + n
	lg.must(t, ctx, "mutation", "upsertGamesPlayer", map[string]any{"playerId": player, "gamertag": tag, "displayName": "x", "consented": true})
	skill := lg.plantSkill(t, owner, skillSpec{id: "gs-race-" + n, goal: goal})
	if err := lg.provider.space.store(context.Background(), canonical(conceptSkill, skill), conceptSkill, goal); err != nil {
		t.Fatal(err)
	}
	gate := "games-skill:" + skill

	// The forget stops after its last statement, the vectors, with its transaction open: all of
	// it deleted, nothing committed. Held there, all it has left to do is commit, which is what
	// the ladder move below waits for (its gate gives it six seconds from when it arrives).
	var (
		reached = make(chan struct{})
		resume  = make(chan struct{})
		once    sync.Once
	)
	lg.provider.afterForgetStep = func(step string) error {
		if step == stepVectors {
			once.Do(func() { close(reached) })
			<-resume
		}
		return nil
	}
	defer func() { lg.provider.afterForgetStep = nil }()
	forgetting := lg.inGoroutine(ctx, "gamesForgetPlayer", map[string]any{"playerId": player})
	select {
	case <-reached:
	case <-time.After(arrivalPatience):
		t.Fatal("the forget never reached the end of its work")
	}

	// The forget's transaction is open and holds the gate; it holds no connection of the gate pool
	// to do it (four of those serve every write gate of the node).
	if inUse := lg.provider.gates().Stats().InUse; inUse != 0 {
		t.Errorf("the forget in the middle of its transaction holds %d connection(s) of the write-gate pool, want none: the gate is taken on the forget's own transaction", inUse)
	}

	// The ladder move, with the forget uncommitted.
	moving := lg.inGoroutine(ctx, "gamesShadowCompare", map[string]any{
		"goalSignature": GoalSignature(goal, nil),
		"plan":          []any{map[string]any{"action": "look", "args": map[string]any{"where": "around"}}},
	})
	var early *outcome
	waited := lg.waitForGateWaiter(t, gate, finishedOf(moving, &early))
	select {
	case o := <-moving:
		early = &o
	default:
	}
	close(resume)

	if !waited || early != nil {
		t.Errorf("the ladder move did not wait for the forget: queued behind its gate %v, finished before the forget committed %v (%+v): it was free to write a version the forget's delete never saw", waited, early != nil, early)
	}
	forgot := <-forgetting
	if forgot.err != nil {
		t.Fatalf("forget: %v", forgot.err)
	}
	if got := asInt(asObject(forgot.rows[0]["deleted"])["skills"], -1); got != 1 {
		t.Errorf("forget answered %v, want the one skill deleted", forgot.rows[0]["deleted"])
	}
	moved := early
	if moved == nil {
		o := <-moving
		moved = &o
	}
	if moved.err != nil {
		t.Errorf("the ladder move, after the forget committed: %v, want the skill passed over", moved.err)
	} else if got := asInt(moved.rows[0]["compared"], -1); got != 0 {
		t.Errorf("the ladder move compared %v skill(s), want it to find the skill gone and compare nothing", moved.rows[0]["compared"])
	}
	if got := lg.stored(t, conceptSkill, skill); got != 0 {
		_, payload := lg.skillVersion(t, skill)
		t.Errorf("the skill has %d stored version(s) after the forget committed, want none; the newest is %s", got, payload)
	}
	if got := lg.vectorsOf(t, lg.table, conceptSkill, skill); got != 0 {
		t.Errorf("the skill's vector is still stored: %d", got)
	}
}

// The other order: a ladder move is in flight (it holds the skill's gate, has read the skill
// and not yet written) when the forget arrives. The forget waits for the move to finish, and
// then deletes every version of the skill, the one the move wrote included.
func TestLiveAForgetWaitsForALadderMoveInFlight(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	owner := "user-games-forget-wait-" + n
	ctx := asOwner(owner)
	t.Cleanup(func() {
		if _, err := lg.db.DB.Exec(`DELETE FROM "MemoryNodes" WHERE concept = $1 AND payload->>'ownerUserId' = $2`, conceptSkill, ownerKey(owner)); err != nil {
			t.Logf("cleanup: %v", err)
		}
	})
	tag := "WaitTag" + n
	goal := "follow " + tag + " home"
	player := "p-wait-" + n
	lg.must(t, ctx, "mutation", "upsertGamesPlayer", map[string]any{"playerId": player, "gamertag": tag, "displayName": "x", "consented": true})
	skill := lg.plantSkill(t, owner, skillSpec{id: "gs-wait-" + n, goal: goal})
	gate := "games-skill:" + skill

	held := &heldStore{rowStore: lg.provider.store, reading: make(chan struct{}), release: make(chan struct{})}
	lg.provider.store = held
	moving := lg.inGoroutine(ctx, "gamesShadowCompare", map[string]any{
		"goalSignature": GoalSignature(goal, nil),
		"plan":          []any{map[string]any{"action": "look", "args": map[string]any{"where": "around"}}},
	})
	select {
	case <-held.reading:
	case <-time.After(arrivalPatience):
		t.Fatal("the ladder move never read the skill")
	}

	forgetting := lg.inGoroutine(ctx, "gamesForgetPlayer", map[string]any{"playerId": player})
	var early *outcome
	waited := lg.waitForGateWaiter(t, gate, finishedOf(forgetting, &early))
	select {
	case o := <-forgetting:
		early = &o
	default:
	}
	close(held.release)

	if !waited || early != nil {
		t.Errorf("the forget did not wait for the ladder move in flight: queued behind its gate %v, finished while the move was still to write %v (%+v)", waited, early != nil, early)
	}
	forgot := early
	if forgot == nil {
		o := <-forgetting
		forgot = &o
	}
	moved := <-moving
	if forgot.err != nil {
		t.Fatalf("forget: %v", forgot.err)
	}
	if got := asInt(asObject(forgot.rows[0]["deleted"])["skills"], -1); got != 1 {
		t.Errorf("forget answered %v, want the one skill deleted", forgot.rows[0]["deleted"])
	}
	if moved.err != nil {
		t.Errorf("the ladder move: %v", moved.err)
	} else if got := asInt(moved.rows[0]["compared"], -1); got != 1 {
		t.Errorf("the ladder move compared %v skill(s), want the one it held", moved.rows[0]["compared"])
	}
	if got := lg.stored(t, conceptSkill, skill); got != 0 {
		_, payload := lg.skillVersion(t, skill)
		t.Errorf("the skill has %d stored version(s) after both finished, want none; the newest is %s", got, payload)
	}
}

// The gate a forget holds is the gate a ladder move takes: the same advisory lock, held on
// forget's own transaction, until that transaction ends; and the statements after it get
// the lock_timeout they had, not what the gate left of its wait.
func TestLiveFenceSkillsHoldsTheWriteGateUntilTheTransactionEnds(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	owner, other := "user-games-fence-"+n, "user-games-fence-other-"+n
	t.Cleanup(func() {
		for _, name := range []string{owner, other} {
			if _, err := lg.db.DB.Exec(`DELETE FROM "MemoryNodes" WHERE concept = $1 AND payload->>'ownerUserId' = $2`, conceptSkill, ownerKey(name)); err != nil {
				t.Logf("cleanup: %v", err)
			}
		}
	})
	tag := "FenceTag" + n
	named := []string{
		lg.plantSkill(t, owner, skillSpec{id: "gs-fence-b-" + n, goal: "follow " + tag}),
		lg.plantSkill(t, owner, skillSpec{id: "gs-fence-a-" + n, goal: "meet " + tag}),
	}
	clean := lg.plantSkill(t, owner, skillSpec{id: "gs-fence-c-" + n, goal: "dig a trench"})
	foreign := lg.plantSkill(t, other, skillSpec{id: "gs-fence-x-" + n, goal: "follow " + tag})
	held := func(id string) bool {
		var c int
		if err := lg.db.DB.QueryRow(`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND granted AND objsubid = 2
			AND classid = $1::oid AND objid = hashtext($2)::oid`, writeGateClass, "games-skill:"+id).Scan(&c); err != nil {
			t.Fatal(err)
		}
		return c > 0
	}

	tx, err := lg.db.DB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var before string
	if err := tx.QueryRow(`SELECT current_setting('lock_timeout')`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := lg.provider.fenceSkills(context.Background(), lg.db.DB, tx, ownerKey(owner), traceMentionPattern(tag)); err != nil {
		t.Fatal(err)
	}
	for _, id := range named {
		if !held(id) {
			t.Errorf("the gate of %s, which names the player, is not held", id)
		}
	}
	if held(clean) {
		t.Errorf("the gate of %s, which names nobody, is held", clean)
	}
	if held(foreign) {
		t.Errorf("the gate of %s, which is another owner's though it names the same gamertag, is held", foreign)
	}
	var after string
	if err := tx.QueryRow(`SELECT current_setting('lock_timeout')`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Errorf("the statements after the gates run with lock_timeout %s, want the %s they had", after, before)
	}

	// The real gate, taken the way a ladder move takes it, waits for the forget.
	mover := make(chan error, 1)
	go func() {
		release, err := memql.AcquireWriteGate(context.Background(), lg.provider.gates(), "games-skill:"+named[0])
		if err == nil {
			release()
		}
		mover <- err
	}()
	var moverErr error
	moverDone := false
	if !lg.waitForGateWaiter(t, "games-skill:"+named[0], func() bool {
		select {
		case moverErr = <-mover:
			moverDone = true
			return true
		default:
			return false
		}
	}) {
		t.Error("the write gate of a skill the transaction holds did not wait for it")
	}
	select {
	case err := <-mover:
		moverDone, moverErr = true, err
	default:
	}
	if moverDone {
		t.Errorf("a ladder move's gate was granted while the transaction held it: %v", moverErr)
	}

	// ...and goes when the transaction ends, committed or not.
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if !moverDone {
		select {
		case err := <-mover:
			if err != nil {
				t.Errorf("the gate, once the transaction ended: %v", err)
			}
		case <-time.After(arrivalPatience):
			t.Error("the gate was not granted when the transaction ended")
		}
	}
	for _, id := range named {
		if held(id) {
			t.Errorf("the gate of %s is still held after the transaction ended", id)
		}
	}
}

// The gates are taken in the order of the skills' ids, whatever order the rows were written in,
// so two forgets of one owner can only wait for each other and never each hold the gate the
// other wants. Seen from outside: with the gate of the LAST skill held by somebody else, the
// fence has already been granted the gate of the first one, and is waiting for the last.
func TestLiveFenceSkillsTakesTheGatesInTheOrderOfTheIds(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	owner := "user-games-fence-order-" + n
	t.Cleanup(func() {
		if _, err := lg.db.DB.Exec(`DELETE FROM "MemoryNodes" WHERE concept = $1 AND payload->>'ownerUserId' = $2`, conceptSkill, ownerKey(owner)); err != nil {
			t.Logf("cleanup: %v", err)
		}
	})
	tag := "OrderTag" + n
	// written last first, so that neither the write order nor an index order is the id order
	last := lg.plantSkill(t, owner, skillSpec{id: "gs-order-c-" + n, goal: "follow " + tag})
	middle := lg.plantSkill(t, owner, skillSpec{id: "gs-order-b-" + n, goal: "meet " + tag})
	first := lg.plantSkill(t, owner, skillSpec{id: "gs-order-a-" + n, goal: "find " + tag})
	granted := func(id string) bool {
		var c int
		if err := lg.db.DB.QueryRow(`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND granted AND objsubid = 2
			AND classid = $1::oid AND objid = hashtext($2)::oid`, writeGateClass, "games-skill:"+id).Scan(&c); err != nil {
			t.Fatal(err)
		}
		return c > 0
	}

	release, err := memql.AcquireWriteGate(context.Background(), lg.provider.gates(), "games-skill:"+last)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	defer func() {
		if !released {
			release()
		}
	}()
	tx, err := lg.db.DB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	fenced := make(chan error, 1)
	go func() {
		fenced <- lg.provider.fenceSkills(context.Background(), lg.db.DB, tx, ownerKey(owner), traceMentionPattern(tag))
	}()
	var fenceErr error
	fenceDone := false
	if !lg.waitForGateWaiter(t, "games-skill:"+last, func() bool {
		select {
		case fenceErr = <-fenced:
			fenceDone = true
			return true
		default:
			return false
		}
	}) {
		t.Fatalf("the fence did not ask for the gate of the last skill (finished: %v, %v)", fenceDone, fenceErr)
	}
	if !granted(first) || !granted(middle) {
		t.Errorf("waiting for the last skill's gate, the fence holds the first's (%v) and the middle's (%v), want both: the ids are taken in order", granted(first), granted(middle))
	}
	release()
	released = true
	select {
	case err := <-fenced:
		if err != nil {
			t.Errorf("the fence, once the last gate was free: %v", err)
		}
	case <-time.After(arrivalPatience):
		t.Error("the fence did not finish when the last gate was free")
	}
}

// A forget that cannot have a skill's gate in time is refused whole: it answers that nothing
// was deleted, as a busy gate does (the caller may send it again), and every statement that
// ran before the gate was asked for is undone. Sent again when the gate is free it deletes.
func TestLiveAForgetThatCannotHoldASkillsGateDeletesNothing(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	owner := "user-games-forget-busy-" + n
	ctx := asOwner(owner)
	t.Cleanup(func() {
		if _, err := lg.db.DB.Exec(`DELETE FROM "MemoryNodes" WHERE concept = $1 AND payload->>'ownerUserId' = $2`, conceptSkill, ownerKey(owner)); err != nil {
			t.Logf("cleanup: %v", err)
		}
	})
	tag := "BusyTag" + n
	player := "p-busy-" + n
	now := time.Now().UTC()
	lg.must(t, ctx, "mutation", "upsertGamesPlayer", map[string]any{"playerId": player, "gamertag": tag, "displayName": "x", "consented": true})
	lg.must(t, ctx, "builtin", "gamesRecordEpisodes", map[string]any{"episodes": episodesArg(episode("e-busy-"+n, "wren", player, "played with "+player, now, 4))})
	lg.writeTrace(t, owner, traceAbout{id: "t-busy-" + n, goal: "follow " + tag})
	skill := lg.plantSkill(t, owner, skillSpec{id: "gs-busy-" + n, goal: "follow " + tag})
	stored := func() []int {
		return []int{lg.stored(t, conceptPlayer, player), lg.stored(t, conceptEpisode, "e-busy-"+n), lg.stored(t, conceptPlanTrace, "t-busy-"+n), lg.stored(t, conceptSkill, skill)}
	}
	if got := stored(); !sameInts(got, []int{1, 1, 1, 1}) {
		t.Fatalf("the setup stored %v", got)
	}

	release, err := memql.AcquireWriteGate(context.Background(), lg.provider.gates(), "games-skill:"+skill)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	defer func() {
		if !released {
			release()
		}
	}()
	lg.provider.forgetGateWait = 300 * time.Millisecond
	defer func() { lg.provider.forgetGateWait = 0 }()

	started := time.Now()
	_, err = lg.call(t, ctx, "builtin", "gamesForgetPlayer", map[string]any{"playerId": player})
	if err == nil || !errors.Is(err, memql.ErrWriteGateBusy) || !memql.IsWriteGateBusy(err) || !strings.Contains(err.Error(), "nothing was deleted") {
		t.Fatalf("a forget whose skill's gate is held = %v, want the busy answer saying nothing was deleted", err)
	}
	if took := time.Since(started); took > 5*time.Second {
		t.Errorf("the refusal took %v, want it at the wait the seam names (300ms) and not the gate's six seconds", took)
	}
	if got := stored(); !sameInts(got, []int{1, 1, 1, 1}) {
		t.Errorf("the refused forget left %v stored, want everything as it was: the deletes before the gate are undone", got)
	}
	if rows := lg.must(t, ctx, "query", "gamesPlayerById", map[string]any{"playerId": player}); len(rows) != 1 {
		t.Errorf("the refused forget left the player unreadable: %v", rows)
	}

	release()
	released = true
	lg.provider.forgetGateWait = 0
	res := lg.must(t, ctx, "builtin", "gamesForgetPlayer", map[string]any{"playerId": player})
	if got := asInt(asObject(res[0]["deleted"])["skills"], -1); got != 1 {
		t.Errorf("the forget sent again answered %v, want the skill deleted", res[0]["deleted"])
	}
	if got := stored(); !sameInts(got, []int{0, 0, 0, 0}) {
		t.Errorf("the forget sent again left %v stored", got)
	}
}

func sameInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
