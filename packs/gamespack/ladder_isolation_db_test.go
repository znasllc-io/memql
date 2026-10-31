package gamespack

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
)

// ---- a ladder move is its owner's alone ----

// skillVersion reads the newest stored version of one skill row, whoever owns it: when
// it was written and its whole payload. The test reads around the owned tier on
// purpose -- it is asking what is in the table.
func (lg *liveGames) skillVersion(t *testing.T, bareID string) (createdAt, payload string) {
	t.Helper()
	if err := lg.db.DB.QueryRow(`SELECT "createdAt"::text, payload::text FROM "MemoryNodes"
WHERE concept = $1 AND id = $2 ORDER BY "createdAt" DESC LIMIT 1`, conceptSkill, canonical(conceptSkill, bareID)).Scan(&createdAt, &payload); err != nil {
		t.Fatalf("read the newest version of skill %s: %v", bareID, err)
	}
	return createdAt, payload
}

// teachGoal teaches owner's skill for a goal other than the torch one, from two
// agreeing traces, and returns the skill and the signature of the goal.
func teachGoal(t *testing.T, lg *liveGames, ctx context.Context, goal, prefix string) (skillID, sig string) {
	t.Helper()
	sig = GoalSignature(goal, []string{"spot"})
	for i, count := range []float64{4, 6} {
		tr := trace(prefix+"-"+string(rune('a'+i)), torchPlan(count), "success", "neutral")
		tr["goalText"] = goal
		if got, _ := lg.must(t, ctx, "builtin", "gamesRecordTrace", tr)[0]["learnedSkillId"].(string); got != "" {
			skillID = got
		}
	}
	if skillID == "" {
		t.Fatalf("two agreeing traces of %q taught no skill", goal)
	}
	return skillID, sig
}

// A ladder move writes a skill's whole state under internal origin, which the owned
// tier's write guard lets through without asking whose row it lands on, and the
// mutation stamps ownerUserId from the ACTOR. The owned tier's READ arm for the
// cluster's owner, though, returns every owner's rows -- so without a check of its
// own a cluster owner could read another owner's skill by id, move it, and have it
// written back as their own: the skill, its ladder and whatever the owner approved
// become somebody else's. moveLadder therefore compares the row's owner with the
// caller's before anything is written, and a skill that is not the caller's is
// reported the way one that does not exist is.
//
// Owner A has a skill awaiting its promotion decision, one in canary, and one that
// has been idle; B is another owner; C is the cluster's owner. Every builtin that
// moves a skill is tried on A's skills by B and by C, and A's skills are as they
// were after: the same owner, the same number of stored versions, the same newest
// version, byte for byte. The controls are the same calls by their owners.
func TestLiveALadderMoveNeverReachesAnotherOwnersSkill(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	aName, bName, cName := "user-games-own-a-"+n, "user-games-own-b-"+n, "user-games-own-c-"+n
	a, b, c := asOwner(aName), asOwner(bName), clusterOwnerOf(cName)

	// A's three skills: awaiting a decision (the torch goal), in canary, and idle.
	awaiting, sigAwaiting := teachGoal(t, lg, a, "Light up the area!", "own-aw-"+n)
	earnPromotion(t, lg, a, sigAwaiting)
	canary, sigCanary := teachGoal(t, lg, a, "Fill the chest!", "own-ca-"+n)
	earnPromotion(t, lg, a, sigCanary)
	digest := lg.must(t, a, "query", "gamesSkillById", map[string]any{"skillId": canary})[0]["templateDigest"]
	lg.must(t, a, "builtin", "gamesDecideSkillPromotion", map[string]any{"skillId": canary, "templateDigest": digest, "approved": true})
	idle, sigIdle := teachGoal(t, lg, a, "Dig a trench!", "own-id-"+n)

	skills := map[string]string{"awaiting": awaiting, "canary": canary, "idle": idle}
	type snapshot struct {
		versions  int
		createdAt string
		payload   string
	}
	take := func() map[string]snapshot {
		out := map[string]snapshot{}
		for name, id := range skills {
			created, payload := lg.skillVersion(t, id)
			out[name] = snapshot{lg.stored(t, conceptSkill, id), created, payload}
		}
		return out
	}
	before := take()
	for name, s := range before {
		if !strings.Contains(s.payload, `"ownerUserId": "`+ownerKey(aName)+`"`) {
			t.Fatalf("the setup: skill %s is not A's: %s", name, s.payload)
		}
	}
	awaitingDigest := lg.must(t, a, "query", "gamesSkillById", map[string]any{"skillId": awaiting})[0]["templateDigest"]
	if st := lg.must(t, a, "query", "gamesSkillById", map[string]any{"skillId": awaiting})[0]["state"]; st != "awaiting_approval" {
		t.Fatalf("the setup: the first skill is %v, want awaiting_approval", st)
	}

	// The cluster's owner READS A's skill: without that the checks below would not
	// test the pack's own, only the owned tier's.
	if rows := lg.must(t, c, "query", "gamesSkillById", map[string]any{"skillId": awaiting}); len(rows) != 1 {
		t.Fatalf("control: the cluster's owner reads %d of A's skill, want 1 -- without it the next checks do not test the pack's own", len(rows))
	}
	if rows := lg.must(t, b, "query", "gamesSkillById", map[string]any{"skillId": awaiting}); len(rows) != 0 {
		t.Fatalf("control: B reads A's skill: %v", rows)
	}

	// 31 days on, past the retirement window: a sweep that reached A's idle skills
	// would retire them.
	lg.provider.now = func() time.Time { return time.Now().Add(31 * 24 * time.Hour) }
	defer func() { lg.provider.now = time.Now }()

	for who, ctx := range map[string]context.Context{"B": b, "the cluster's owner": c} {
		refused := func(label string, err error) {
			t.Helper()
			if err == nil || !strings.HasPrefix(err.Error(), "games_skill_not_found: ") {
				t.Errorf("%s: %s: %v, want games_skill_not_found", who, label, err)
			}
		}
		_, err := lg.call(t, ctx, "builtin", "gamesDecideSkillPromotion", map[string]any{"skillId": awaiting, "templateDigest": awaitingDigest, "approved": true})
		refused("deciding A's promotion", err)
		_, err = lg.call(t, ctx, "builtin", "gamesReportSkillOutcome", map[string]any{"skillId": canary, "ok": true})
		refused("reporting a run of A's canary skill", err)
		_, err = lg.call(t, ctx, "builtin", "gamesReportSkillOutcome", map[string]any{"skillId": canary, "ok": false})
		refused("reporting a failed run of A's canary skill", err)

		// A comparison and a sweep do not name a skill: they reach for every skill of the
		// caller's, and none of A's is the caller's.
		got := lg.must(t, ctx, "builtin", "gamesShadowCompare", map[string]any{"goalSignature": sigAwaiting, "plan": planArg(torchPlan(4))})[0]
		if asInt(got["compared"], -1) != 0 || asInt(got["matched"], -1) != 0 {
			t.Errorf("%s: comparing a plan with A's shadow skill answered %v, want it compared with nothing", who, got)
		}
		got = lg.must(t, ctx, "builtin", "gamesShadowCompare", map[string]any{"goalSignature": sigIdle, "plan": planArg(torchPlan(4))})[0]
		if asInt(got["compared"], -1) != 0 {
			t.Errorf("%s: comparing a plan with A's idle skill answered %v, want it compared with nothing", who, got)
		}
		swept := lg.must(t, ctx, "builtin", "gamesSweepSkills", nil)[0]
		if asInt(swept["retired"], -1) != 0 || asInt(swept["demoted"], -1) != 0 {
			t.Errorf("%s: a sweep answered %v, want it to have retired and demoted nothing of A's", who, swept)
		}
	}

	after := take()
	for name, was := range before {
		if got := after[name]; got != was {
			t.Errorf("A's %s skill was changed by another owner's call: %+v, was %+v", name, got, was)
		}
	}
	for name, id := range skills {
		if rows := lg.must(t, a, "query", "gamesSkillById", map[string]any{"skillId": id}); len(rows) != 1 || ownerKey(trimmed(rows[0]["ownerUserId"])) != ownerKey(aName) {
			t.Errorf("A's %s skill is no longer A's: %v", name, rows)
		}
	}

	// The controls: the same calls by the owner work. (The sweep is A's own, and it
	// retires the idle skill at +31 days, which proves the sweep above reached for the
	// right thing and A's skills were simply not its to move.)
	lg.provider.now = time.Now
	if _, err := lg.call(t, a, "builtin", "gamesReportSkillOutcome", map[string]any{"skillId": canary, "ok": true}); err != nil {
		t.Errorf("control: A reporting a run of A's own canary skill: %v", err)
	}
	if res, err := lg.call(t, a, "builtin", "gamesDecideSkillPromotion", map[string]any{"skillId": awaiting, "templateDigest": awaitingDigest, "approved": true}); err != nil || res[0]["rung"] != "canary" {
		t.Errorf("control: A deciding A's own promotion: %v, %v", res, err)
	}
	lg.provider.now = func() time.Time { return time.Now().Add(31 * 24 * time.Hour) }
	if swept := lg.must(t, a, "builtin", "gamesSweepSkills", nil)[0]; asInt(swept["retired"], -1) < 1 {
		t.Errorf("control: A's own sweep at +31 days retired %v, want A's idle skills among them", swept)
	}
	lg.provider.now = time.Now

	// ...and the cluster's owner moves their own skills as anybody does.
	own, sigOwn := teachGoal(t, lg, c, "Mine some stone!", "own-c-"+n)
	earnPromotion(t, lg, c, sigOwn)
	ownDigest := lg.must(t, c, "query", "gamesSkillById", map[string]any{"skillId": own})[0]["templateDigest"]
	if res, err := lg.call(t, c, "builtin", "gamesDecideSkillPromotion", map[string]any{"skillId": own, "templateDigest": ownDigest, "approved": true}); err != nil || res[0]["rung"] != "canary" {
		t.Errorf("control: the cluster's owner deciding their own promotion: %v, %v", res, err)
	}
	rows := lg.must(t, c, "query", "gamesSkillById", map[string]any{"skillId": own})
	if len(rows) != 1 || ownerKey(trimmed(rows[0]["ownerUserId"])) != ownerKey(cName) {
		t.Errorf("the cluster owner's own skill: %v", rows)
	}
}

// ---- a move is one critical section ----
//
// Three things make a read-modify-write of one row safe across replicas, and each alone
// was measured insufficient (memql#5431): a Postgres lock around the read and the write,
// a read that skips this node's result cache, and a version stamped after the one it
// read. A test that two characters reporting runs of one skill both count would pass
// without any of them whenever the two happen not to overlap, which on this harness is
// nearly always (a move takes a few milliseconds and the second racer starts after the
// first has finished). So each of the three is held in the one way that can show it:
// the overlap is MADE, or the premise of the ingredient is set up and observed.

// meetingStore holds every read of a skill until the other racer's read has been made as
// well (or a moment has passed, since a gate can keep the other from ever arriving), so
// that two moves that read the same row both go on to write from it unless something
// stops the second from reading.
type meetingStore struct {
	rowStore
	meeting *traceMeeting
}

func (m *meetingStore) query(ctx context.Context, name string, args map[string]any) ([]map[string]any, error) {
	rows, err := m.rowStore.query(ctx, name, args)
	if name == "gamesSkillById" {
		m.meeting.meet()
	}
	return rows, err
}

// Two replicas move one skill at once, and each reads the skill and then waits for the
// other's read before it writes. With the gate the second cannot read until the first has
// written, so it reads the first's result, and both runs count. Without it both read the
// same state and the second write puts the first one back.
func TestLiveTwoReplicasMovingOneSkillAtOnceBothCount(t *testing.T) {
	here := newLiveGames(t)
	there := here.replica(t)
	n := runID()
	owner := "user-games-gate-" + n
	ctx := asOwner(owner)
	skillID, _ := climbToCanary(t, here, ctx, "gate-"+n)

	meeting := newTraceMeeting()
	here.provider.store = &meetingStore{rowStore: here.provider.store, meeting: meeting}
	there.provider.store = &meetingStore{rowStore: there.provider.store, meeting: meeting}

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, lg := range []*liveGames{here, there} {
		wg.Add(1)
		go func(lg *liveGames) {
			defer wg.Done()
			if _, err := lg.execute(ctx, "builtin", "gamesReportSkillOutcome", map[string]any{"skillId": skillID, "ok": true}); err != nil {
				errs <- err
			}
		}(lg)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	skill := here.must(t, memql.ContextWithFreshRead(ctx), "query", "gamesSkillById", map[string]any{"skillId": skillID})[0]
	if asInt(skill["canaryMatches"], 0) != 2 {
		t.Fatalf("two runs reported at once on two replicas counted %v times, want 2", skill["canaryMatches"])
	}
}

// A replica whose result cache has not heard of the other replica's move (a test process
// runs no mesh, so nothing announces it) must still read the row the move left. The
// premise is OBSERVED, not assumed: this replica's cache is warmed until it answers from
// it, its ordinary read is shown to be stale after the other replica's run, and a fresh
// read to see it. Only then does the move's read mean anything.
func TestLiveAReplicaWithAStaleCacheDoesNotLoseTheOtherReplicasRun(t *testing.T) {
	here := newLiveGames(t)
	there := here.replica(t)
	n := runID()
	owner := "user-games-fresh-ladder-" + n
	ctx := asOwner(owner)
	skillID, _ := climbToCanary(t, here, ctx, "fresh-"+n)

	byID := map[string]any{"skillId": skillID}
	there.warmed(t, ctx, "gamesSkillById", byID)
	if _, err := here.call(t, ctx, "builtin", "gamesReportSkillOutcome", map[string]any{"skillId": skillID, "ok": true}); err != nil {
		t.Fatal(err)
	}
	if got := asInt(there.must(t, ctx, "query", "gamesSkillById", byID)[0]["canaryMatches"], -1); got != 0 {
		t.Fatalf("the premise failed: the other replica's cache reads %d clean runs, so the test proves nothing", got)
	}
	if got := asInt(there.must(t, memql.ContextWithFreshRead(ctx), "query", "gamesSkillById", byID)[0]["canaryMatches"], -1); got != 1 {
		t.Fatalf("the control failed: a fresh read sees %d clean runs, want the one the other replica reported", got)
	}

	if _, err := there.call(t, ctx, "builtin", "gamesReportSkillOutcome", map[string]any{"skillId": skillID, "ok": true}); err != nil {
		t.Fatal(err)
	}
	if got := asInt(here.must(t, memql.ContextWithFreshRead(ctx), "query", "gamesSkillById", byID)[0]["canaryMatches"], -1); got != 2 {
		t.Fatalf("a run reported on a replica with a stale cache left %d clean runs counted, want 2", got)
	}
}

// A replica whose clock is behind the other's would file its write BEHIND the version it
// read, and the newest version, which is the one that counts, would be the older state:
// the run it reported would be lost. The version is stamped a microsecond after the one
// read when the clock is not past it.
func TestLiveAReplicaWithALaggingClockStillWritesTheNewestVersion(t *testing.T) {
	here := newLiveGames(t)
	there := here.replica(t)
	n := runID()
	owner := "user-games-clock-" + n
	ctx := asOwner(owner)
	skillID, _ := climbToCanary(t, here, ctx, "clock-"+n)

	byID := map[string]any{"skillId": skillID}
	fresh := memql.ContextWithFreshRead(ctx)
	here.provider.now = func() time.Time { return time.Now().Add(time.Hour) } // an hour ahead of the other replica
	if _, err := here.call(t, ctx, "builtin", "gamesReportSkillOutcome", map[string]any{"skillId": skillID, "ok": true}); err != nil {
		t.Fatal(err)
	}
	ahead := timeOf(here.must(t, fresh, "query", "gamesSkillById", byID)[0]["createdAt"])
	if !ahead.After(time.Now().Add(50 * time.Minute)) {
		t.Fatalf("the setup: the first run's version is stamped %v, want an hour from now", ahead)
	}

	if _, err := there.call(t, ctx, "builtin", "gamesReportSkillOutcome", map[string]any{"skillId": skillID, "ok": true}); err != nil {
		t.Fatal(err)
	}
	skill := here.must(t, fresh, "query", "gamesSkillById", byID)[0]
	if got := asInt(skill["canaryMatches"], -1); got != 2 {
		t.Fatalf("the run reported by the replica whose clock was behind was lost: %d clean runs, want 2", got)
	}
	if got := timeOf(skill["createdAt"]); !got.After(ahead) {
		t.Fatalf("the newest version is stamped %v, want it after the version it read (%v)", got, ahead)
	}
}
