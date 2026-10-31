package gamespack

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
)

// A canary skill serves by signature with the model standing by and is not
// reachable without a model; a trusted one is found by its trigger and by
// meaning, and an unrelated message matches nothing.
func TestLiveSkillIsFoundBySignatureTriggerAndMeaning(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	ctx := asOwner("user-games-lookup-" + n)
	skillID, sig := climbToCanary(t, lg, ctx, "l-"+n)

	served := lg.must(t, ctx, "builtin", "gamesFindSkill", map[string]any{"goalSignature": sig})
	if len(served) != 1 || served[0]["id"] != skillID || served[0]["serve"] != "skill" || served[0]["standby"] != true {
		t.Fatalf("a canary skill serves by signature with the model standing by: %v", served)
	}
	if got := lg.must(t, ctx, "builtin", "gamesFindSkill", map[string]any{"trigger": "initiative"}); len(got) != 0 {
		t.Fatalf("only a trusted skill runs with no model asked; a canary one was found by trigger: %v", got)
	}

	for i := 0; i < 5; i++ {
		lg.must(t, ctx, "builtin", "gamesReportSkillOutcome", map[string]any{"skillId": skillID, "ok": true})
	}
	if got := lg.must(t, ctx, "builtin", "gamesFindSkill", map[string]any{"trigger": "initiative"}); len(got) != 1 || got[0]["id"] != skillID || got[0]["serve"] != "skill" || got[0]["standby"] != false {
		t.Fatalf("a trusted skill must be found by its trigger and serve alone: %v", got)
	}
	if got := lg.must(t, ctx, "builtin", "gamesFindSkill", map[string]any{"text": "light up the area"}); len(got) != 1 || got[0]["id"] != skillID || asFloat(got[0]["similarity"], 0) < defaultMinSimilarity {
		t.Fatalf("a trusted skill must be found by meaning: %v", got)
	}
	if got := lg.must(t, ctx, "builtin", "gamesFindSkill", map[string]any{"text": "go fishing by the river"}); len(got) != 0 {
		t.Fatalf("an unrelated message matched a skill: %v", got)
	}
	if _, err := lg.call(t, ctx, "builtin", "gamesFindSkill", map[string]any{"goalSignature": sig, "trigger": "initiative"}); err == nil || !strings.Contains(err.Error(), "games_invalid_argument") {
		t.Fatalf("two keys must be refused, got %v", err)
	}
}

// ---- helpers for the lookup tests ----
//
// Every test below teaches its goal under a signature of its own (an input key with the
// run's id in it: only the NAMES of a goal's inputs enter a signature, so the words the
// skill is embedded from stay the same), so that a lookup that leaked could only leak a
// skill this run taught, which the test then names -- and not whichever skills other runs
// and other sessions' tests left in the shared database under a goal they have in common.

// lookupSeq is a plan of the given actions, each taking one count.
func lookupSeq(actions []string, count float64) []PlanStep {
	steps := make([]PlanStep, len(actions))
	for i, action := range actions {
		steps[i] = PlanStep{Action: action, Args: map[string]any{"count": count}}
	}
	return steps
}

// lookupTeach teaches ctx's owner a skill for goal from two agreeing successful traces
// recorded under trigger, and returns the skill and the goal's signature.
func lookupTeach(t *testing.T, lg *liveGames, ctx context.Context, goal, inputKey, trigger string, actions []string, prefix string) (skillID, sig string) {
	t.Helper()
	sig = GoalSignature(goal, []string{inputKey})
	for i, count := range []float64{4, 6} {
		tr := map[string]any{
			"traceId": fmt.Sprintf("%s-%d", prefix, i), "characterId": "character-1", "goalText": goal,
			"trigger": trigger, "inputs": map[string]any{inputKey: "here"},
			"steps": stepsArg(lookupSeq(actions, count)), "outcome": "success", "reaction": "neutral",
			"at": rfc3339(time.Now().UTC()), "say": "On it!",
		}
		if got, _ := lg.must(t, ctx, "builtin", "gamesRecordTrace", tr)[0]["learnedSkillId"].(string); got != "" {
			skillID = got
		}
	}
	if skillID == "" {
		t.Fatalf("two agreeing traces of %q taught no skill", goal)
	}
	return skillID, sig
}

// lookupRun reports n runs of a skill.
func lookupRun(t *testing.T, lg *liveGames, ctx context.Context, skillID string, n int, ok bool) {
	t.Helper()
	for i := 0; i < n; i++ {
		lg.must(t, ctx, "builtin", "gamesReportSkillOutcome", map[string]any{"skillId": skillID, "ok": ok})
	}
}

// lookupClimb takes a skill that lookupTeach taught up the ladder the way an owner and a
// runtime do -- five matching comparisons, the owner's approval -- to canary, and from
// there, with five clean runs, to trusted.
func lookupClimb(t *testing.T, lg *liveGames, ctx context.Context, sig, skillID string, actions []string, rung string) {
	t.Helper()
	for i, count := range []float64{4, 4, 4, 4, 6} {
		res := lg.must(t, ctx, "builtin", "gamesShadowCompare", map[string]any{"goalSignature": sig, "plan": planArg(lookupSeq(actions, count))})
		if asInt(res[0]["matched"], 0) != 1 {
			t.Fatalf("comparison %d for skill %s did not match: %v", i+1, skillID, res[0])
		}
	}
	skill := lg.must(t, ctx, "query", "gamesSkillById", map[string]any{"skillId": skillID})[0]
	decided := lg.must(t, ctx, "builtin", "gamesDecideSkillPromotion", map[string]any{"skillId": skillID, "templateDigest": skill["templateDigest"], "approved": true})
	if decided[0]["rung"] != "canary" {
		t.Fatalf("approval must make skill %s canary: %v", skillID, decided[0])
	}
	if rung != "trusted" {
		return
	}
	lookupRun(t, lg, ctx, skillID, 5, true)
	if got := lg.must(t, ctx, "query", "gamesSkillById", map[string]any{"skillId": skillID})[0]["rung"]; got != "trusted" {
		t.Fatalf("five clean canary runs must make skill %s trusted, it is %v", skillID, got)
	}
}

// lookupCleanup takes away what a test taught the owners it names: their skills, the
// vectors of those, and their plan traces. The database keeps what tests write, and a
// database that holds thousands of skills nobody will read again slows every read of the
// skills that somebody will.
func lookupCleanup(t *testing.T, lg *liveGames, owners ...string) {
	t.Helper()
	t.Cleanup(func() {
		for _, owner := range owners {
			key := ownerKey(owner)
			// the vectors first: they are found through the skills
			if _, err := lg.db.DB.Exec(`DELETE FROM `+lg.table+` WHERE id IN (SELECT id FROM "MemoryNodes" WHERE concept = $1 AND payload->>'ownerUserId' = $2)`, conceptSkill, key); err != nil {
				t.Logf("could not take away the test's vectors: %v", err)
			}
			for _, concept := range []string{conceptSkill, conceptPlanTrace} {
				if _, err := lg.db.DB.Exec(`DELETE FROM "MemoryNodes" WHERE concept = $1 AND payload->>'ownerUserId' = $2`, concept, key); err != nil {
					t.Logf("could not take away the test's %s rows: %v", concept, err)
				}
			}
		}
	})
}

// lookupAsk is one way of asking for a skill.
type lookupAsk struct {
	by   string
	args map[string]any
}

// lookupAsks are the three ways, for one goal.
func lookupAsks(sig, trigger, text string) []lookupAsk {
	return []lookupAsk{
		{"signature", map[string]any{"goalSignature": sig}},
		{"trigger", map[string]any{"trigger": trigger}},
		{"meaning", map[string]any{"text": text}},
	}
}

// lookupHolds asks every way as ctx's caller and holds each answer to want: the id of the
// one skill that is answered, several ids when any of them may be (identical goal words
// are equally near), or "" for no skill. A skill's owner, when named, is the caller's.
func lookupHolds(t *testing.T, lg *liveGames, ctx context.Context, label string, asks []lookupAsk, want []string, owner string) {
	t.Helper()
	for i, ask := range asks {
		got := lg.must(t, ctx, "builtin", "gamesFindSkill", ask.args)
		if want[i] == "" {
			if len(got) != 0 {
				t.Errorf("%s, by %s: answered %v, want no skill", label, ask.by, got)
			}
			continue
		}
		if len(got) != 1 || !strings.Contains(" "+want[i]+" ", " "+fmt.Sprint(got[0]["id"])+" ") {
			t.Errorf("%s, by %s: answered %v, want %s", label, ask.by, got, want[i])
			continue
		}
		if got[0]["serve"] != "skill" {
			t.Errorf("%s, by %s: serve = %v, want skill", label, ask.by, got[0]["serve"])
		}
		if owner != "" && got[0]["ownerUserId"] != ownerKey(owner) {
			t.Errorf("%s, by %s: the skill answered belongs to %v, not to %s", label, ask.by, got[0]["ownerUserId"], owner)
		}
	}
}

// ---- a lookup is the caller's alone ----

// Two owners teach the same goal and each gets a skill of their own; the cluster's owner
// has none yet. Asked by signature, by trigger and by meaning, each owner is answered their
// own skill and nobody else's, and the cluster's owner is answered nothing until it has a
// skill of its own, and then its own although the others' skills are more recently used and
// (by signature) of a better rung. The controls show what keeps it so: the queries the
// lookup reads its lists from name the caller's owner (queries.memql), so the cluster's
// owner, whom the owned tier's read arm would let read every owner's rows, reads none of A's
// and B's through them; the one read that is not scoped, by id, does hand it A's skill, and
// the lookup does not answer it. (The lookup holds its lists to the caller as well, as its
// own check on top of the query's; the tests over an in-memory store that hands over every
// owner's rows hold it to that.)
func TestLiveALookupIsTheCallersAlone(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	aName, bName, cName := "user-games-find-a-"+n, "user-games-find-b-"+n, "user-games-find-c-"+n
	a, b, c := asOwner(aName), asOwner(bName), clusterOwnerOf(cName)
	lookupCleanup(t, lg, aName, bName, cName)
	const goal = "Light up the area!"
	inputKey, trigger := "spot-"+n, "tick-"+n
	actions := []string{"collect", "craft", "placeTorches"}

	aSkill, sig := lookupTeach(t, lg, a, goal, inputKey, trigger, actions, "fa-"+n)
	lookupClimb(t, lg, a, sig, aSkill, actions, "trusted")
	bSkill, _ := lookupTeach(t, lg, b, goal, inputKey, trigger, actions, "fb-"+n)
	lookupClimb(t, lg, b, sig, bSkill, actions, "trusted")
	if aSkill == bSkill {
		t.Fatalf("two owners teaching one goal share the skill %s", aSkill)
	}
	asks := lookupAsks(sig, trigger, goal)

	// The controls. Each owner reads their own skill of the goal through the query the lookup
	// reads, and the cluster's owner reads neither A's nor B's through it or through the trusted
	// skills' query: their pages are their own. By id the cluster's owner does read A's.
	if rows := lg.must(t, a, "query", "gamesSkillsForSignature", map[string]any{"goalSignature": sig}); len(rows) != 1 || bare(trimmed(rows[0]["id"])) != aSkill {
		t.Fatalf("control: A reads %v of the goal's skills, want A's own and only it", rows)
	}
	if rows := lg.must(t, c, "query", "gamesSkillsForSignature", map[string]any{"goalSignature": sig}); len(rows) != 0 {
		t.Fatalf("control: the cluster's owner reads %d skills of the goal through a query that names the caller's owner, want none: its page is its own", len(rows))
	}
	for _, row := range lg.must(t, c, "query", "gamesTrustedSkills", nil) {
		if id := bare(trimmed(row["id"])); id == aSkill || id == bSkill {
			t.Fatalf("control: the cluster's owner reads the skill %s of another owner through the trusted skills' query", id)
		}
	}
	if rows := lg.must(t, c, "query", "gamesSkillById", map[string]any{"skillId": aSkill}); len(rows) != 1 {
		t.Fatalf("control: the cluster's owner reads %d of A's skill by id, want 1 -- the read that is not scoped, which is why the lookup holds the skill it answers to its owner", len(rows))
	}

	lookupHolds(t, lg, a, "A", asks, []string{aSkill, aSkill, aSkill}, aName)
	lookupHolds(t, lg, b, "B", asks, []string{bSkill, bSkill, bSkill}, bName)
	lookupHolds(t, lg, c, "the cluster's owner with no skill", asks, []string{"", "", ""}, "")

	// The cluster's owner teaches the goal as well, first of the three to use it...
	cSkill, _ := lookupTeach(t, lg, c, goal, inputKey, trigger, actions, "fc-"+n)
	lookupClimb(t, lg, c, sig, cSkill, actions, "trusted")
	// ...and the others use theirs after it.
	lookupRun(t, lg, a, aSkill, 1, true)
	lookupRun(t, lg, b, bSkill, 1, true)

	lookupHolds(t, lg, a, "A", asks, []string{aSkill, aSkill, aSkill}, aName)
	lookupHolds(t, lg, b, "B", asks, []string{bSkill, bSkill, bSkill}, bName)
	lookupHolds(t, lg, c, "the cluster's owner with a skill", asks, []string{cSkill, cSkill, cSkill}, cName)
}

// ---- a lookup follows the ladder ----

// Shadow skills are found by nothing; a canary skill by signature alone, with the model
// standing by; a trusted skill outranks a canary one of the same goal and is found by
// all three; of two trusted skills the more recently used wins; a demotion stops a
// skill being found the moment it lands; and a lookup writes nothing.
func TestLiveALookupFollowsTheLadder(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	owner := "user-games-follow-" + n
	ctx := asOwner(owner)
	lookupCleanup(t, lg, owner)
	const goal = "Light up the area!"
	inputKey, trigger := "spot-"+n, "tick-"+n
	seqA, seqB := []string{"collect", "craft", "placeTorches"}, []string{"collect", "placeTorches"}

	var sig string
	check := func(label, bySignature, byTrigger, byMeaning string) {
		t.Helper()
		lookupHolds(t, lg, ctx, label, lookupAsks(sig, trigger, goal), []string{bySignature, byTrigger, byMeaning}, owner)
	}

	a, sig := lookupTeach(t, lg, ctx, goal, inputKey, trigger, seqA, "fl-a-"+n)
	check("a skill in shadow", "", "", "")

	lookupClimb(t, lg, ctx, sig, a, seqA, "canary")
	check("a canary skill", a, "", "")
	if got := lg.must(t, ctx, "builtin", "gamesFindSkill", map[string]any{"goalSignature": sig}); len(got) != 1 || got[0]["standby"] != true || got[0]["rung"] != "canary" {
		t.Fatalf("a canary skill serves with the model standing by: %v", got)
	}

	// The same goal done another way: shadow, it changes nothing; trusted, it outranks the
	// canary skill and serves alone.
	b, _ := lookupTeach(t, lg, ctx, goal, inputKey, trigger, seqB, "fl-b-"+n)
	check("a canary skill and one in shadow", a, "", "")
	lookupClimb(t, lg, ctx, sig, b, seqB, "trusted")
	check("a trusted skill beside a canary one", b, b, b)
	if got := lg.must(t, ctx, "builtin", "gamesFindSkill", map[string]any{"goalSignature": sig}); len(got) != 1 || got[0]["standby"] != false || got[0]["rung"] != "trusted" {
		t.Fatalf("a trusted skill serves alone: %v", got)
	}

	// Both trusted: the more recently used. The words the two are embedded from are the
	// same, so by meaning either is as near as the other.
	lookupRun(t, lg, ctx, a, 5, true)
	either := a + " " + b
	check("two trusted skills, A used last", a, a, either)
	lookupRun(t, lg, ctx, b, 1, true)
	check("two trusted skills, B used last", b, b, either)

	// A lookup is a read.
	before := lg.stored(t, conceptSkill, a, b)
	for i := 0; i < 2; i++ {
		check("two trusted skills, B used last", b, b, either)
	}
	if after := lg.stored(t, conceptSkill, a, b); after != before {
		t.Fatalf("lookups wrote: the skills had %d stored versions and have %d", before, after)
	}

	// Two failures send a skill back to shadow, and it stops being found at once.
	lookupRun(t, lg, ctx, b, 2, false)
	check("B demoted", a, a, a)
	lookupRun(t, lg, ctx, a, 2, false)
	check("both demoted", "", "", "")
}

// A retired skill is never found, whatever rung it had.
func TestLiveARetiredSkillIsNeverFound(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	owner := "user-games-retired-find-" + n
	ctx := asOwner(owner)
	lookupCleanup(t, lg, owner)
	const goal = "Light up the area!"
	inputKey, trigger := "spot-"+n, "tick-"+n
	actions := []string{"collect", "craft", "placeTorches"}
	skill, sig := lookupTeach(t, lg, ctx, goal, inputKey, trigger, actions, "fr-"+n)
	lookupClimb(t, lg, ctx, sig, skill, actions, "trusted")
	asks := lookupAsks(sig, trigger, goal)
	lookupHolds(t, lg, ctx, "a trusted skill", asks, []string{skill, skill, skill}, owner)

	lg.provider.now = func() time.Time { return time.Now().Add(31 * 24 * time.Hour) }
	defer func() { lg.provider.now = time.Now }()
	if swept := lg.must(t, ctx, "builtin", "gamesSweepSkills", nil)[0]; asInt(swept["retired"], -1) != 1 {
		t.Fatalf("a skill unused for 31 days must retire: %v", swept)
	}
	lookupHolds(t, lg, ctx, "a retired skill", asks, []string{"", "", ""}, "")
}

// ---- the model is away, slow, or its space is unusable ----

// Only a lookup by meaning needs the model. With the model away, the signature and the
// trigger still answer, and the lookup by meaning fails saying the text could not be
// embedded, with the cause -- the runtime treats that as no match. With no embedder bound
// at all there is nothing to match against, which is no match and no failure.
func TestLiveALookupWhileTheModelIsAway(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	owner := "user-games-away-find-" + n
	ctx := asOwner(owner)
	lookupCleanup(t, lg, owner)
	const goal = "Light up the area!"
	inputKey, trigger := "spot-"+n, "tick-"+n
	actions := []string{"collect", "craft", "placeTorches"}
	skill, sig := lookupTeach(t, lg, ctx, goal, inputKey, trigger, actions, "fw-"+n)
	lookupClimb(t, lg, ctx, sig, skill, actions, "trusted")
	asks := lookupAsks(sig, trigger, goal)
	base := testSpace{db: lg.db.DB, tableName: lg.table}

	lg.provider.space = failingSpace{base}
	lookupHolds(t, lg, ctx, "with the model away", asks[:2], []string{skill, skill}, owner)
	_, err := lg.call(t, ctx, "builtin", "gamesFindSkill", asks[2].args)
	if err == nil || !strings.Contains(err.Error(), "could not be embedded") || !strings.Contains(err.Error(), errModelAway.Error()) {
		t.Fatalf("a lookup by meaning with the model away = %v, want it to say the text could not be embedded, and why", err)
	}

	lg.provider.space = tableNameSpace{base, ""}
	got, err := lg.call(t, ctx, "builtin", "gamesFindSkill", asks[2].args)
	if err != nil || len(got) != 0 {
		t.Fatalf("a lookup by meaning with no embedder bound = %v, %v, want no match and no failure", got, err)
	}

	lg.provider.space = base
	lookupHolds(t, lg, ctx, "with the model back", asks, []string{skill, skill, skill}, owner)
}

// countedSpace counts the texts the model is handed.
type countedSpace struct {
	embeddingSpace
	embedded *atomic.Int32
}

func (s countedSpace) embedQuery(ctx context.Context, text string) ([]float32, error) {
	s.embedded.Add(1)
	return s.embeddingSpace.embedQuery(ctx, text)
}

// A decision waits on a lookup, so a model that is slow rather than away costs it no more
// than the bound recall's wait has, whether or not the embedder watches its context; then
// the lookup fails as it does with the model away. The lookups that need no model do not
// wait for one.
//
// NO DURATION IS MEASURED, so a loaded machine cannot fail it. The error names the bound the
// wait was given (embedRecallQuery says "did not finish within 200ms"), which tells a lookup
// that honours the bound it was given from one that waits for the model's whole patience (it
// answers a match instead, after seconds) and from one that waits some other bound (the
// default, 2s, is named instead). And the lookups that need no model are shown not to have
// handed the model a text, which is what waiting for it would take.
func TestLiveALookupGivesUpOnASlowModel(t *testing.T) {
	const timeout = 200 * time.Millisecond
	lg := newLiveGames(t)
	n := runID()
	owner := "user-games-slow-find-" + n
	ctx := asOwner(owner)
	lookupCleanup(t, lg, owner)
	const goal = "Light up the area!"
	inputKey, trigger := "spot-"+n, "tick-"+n
	actions := []string{"collect", "craft", "placeTorches"}
	skill, sig := lookupTeach(t, lg, ctx, goal, inputKey, trigger, actions, "fs-"+n)
	lookupClimb(t, lg, ctx, sig, skill, actions, "trusted")
	asks := lookupAsks(sig, trigger, goal)
	lg.provider.embedTimeout = timeout

	for _, tc := range []struct {
		name           string
		ignoresContext bool
	}{
		{"an embedder that watches its context", false},
		{"an embedder that does not", true},
	} {
		var embedded atomic.Int32
		lg.provider.space = countedSpace{
			embeddingSpace: slowSpace{testSpace: testSpace{db: lg.db.DB, tableName: lg.table}, ignoresContext: tc.ignoresContext},
			embedded:       &embedded,
		}
		_, err := lg.call(t, ctx, "builtin", "gamesFindSkill", asks[2].args)
		if want := "did not finish within " + timeout.String(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want the lookup to give up and say %q: the bound it was given", tc.name, err, want)
		}
		if got := embedded.Load(); got != 1 {
			t.Errorf("%s: the lookup by meaning handed the model %d texts, want one", tc.name, got)
		}
		lookupHolds(t, lg, ctx, tc.name, asks[:2], []string{skill, skill}, owner)
		if got := embedded.Load(); got != 1 {
			t.Errorf("%s: the lookups that need no model handed it %d more texts, so they waited for one", tc.name, got-1)
		}
	}
}

// The name of the vector table goes into the text of the statement, so one that is no
// vector table's name is refused instead of put there, and the lookup is refused with it:
// no skill is answered through a statement built from it. The first hostile name below is
// harmless on purpose -- a real vector table with the rest of the join, and a comment,
// after it -- so a statement that took it would still run and show itself in the answer.
func TestLiveALookupByMeaningRefusesASpaceThatNamesNoVectorTable(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	owner := "user-games-hostile-find-" + n
	ctx := asOwner(owner)
	lookupCleanup(t, lg, owner)
	const goal = "Light up the area!"
	inputKey, trigger := "spot-"+n, "tick-"+n
	actions := []string{"collect", "craft", "placeTorches"}
	skill, sig := lookupTeach(t, lg, ctx, goal, inputKey, trigger, actions, "fh-"+n)
	lookupClimb(t, lg, ctx, sig, skill, actions, "trusted")
	ask := map[string]any{"text": goal}
	base := testSpace{db: lg.db.DB, tableName: lg.table}

	// The control: with the space as it is, the skill is found, so the refusals below are the
	// name's doing.
	if got := lg.must(t, ctx, "builtin", "gamesFindSkill", ask); len(got) != 1 || got[0]["id"] != skill {
		t.Fatalf("control: with the space as it is, the lookup answered %v, want %s", got, skill)
	}
	for _, tc := range []struct {
		name  string
		space embeddingSpace
		want  string
	}{
		{"a table name that is no vector table", tableNameSpace{base, lg.table + " nv ON nv.id = latest.id AND nv.vector_field = 'content' --"}, "no vector table"},
		{"a table name that is a table of another kind", tableNameSpace{base, "MemoryNodes"}, "no vector table"},
		{"a space that cannot be read", tableErrorSpace{base}, "could not be read"},
	} {
		lg.provider.space = tc.space
		got, err := lg.call(t, ctx, "builtin", "gamesFindSkill", ask)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to say %q", tc.name, err, tc.want)
		}
		if len(got) != 0 {
			t.Errorf("%s: answered %v", tc.name, got)
		}
	}
}

// ---- by meaning: the floor, and what is not a similarity ----

// Cosines of the stand-in embedder, which the test measures instead of assuming:
// "light up the area quickly" is 0.895 from "Light up the area!" and "light up the cave"
// 0.751, so that the default floor of 0.82 takes the first and not the second.
func TestLiveALookupByMeaningHonoursTheFloor(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	owner := "user-games-floor-" + n
	ctx := asOwner(owner)
	lookupCleanup(t, lg, owner)
	const goal = "Light up the area!"
	inputKey, trigger := "spot-"+n, "tick-"+n
	actions := []string{"collect", "craft", "placeTorches"}
	skill, sig := lookupTeach(t, lg, ctx, goal, inputKey, trigger, actions, "ff-"+n)
	lookupClimb(t, lg, ctx, sig, skill, actions, "trusted")

	const near, far = "light up the area quickly", "light up the cave"
	cosine := func(text string) float64 {
		a, b := bagOfWords(goal), bagOfWords(text)
		var dot, na, nb float64
		for i := range a {
			dot += float64(a[i]) * float64(b[i])
			na += float64(a[i]) * float64(a[i])
			nb += float64(b[i]) * float64(b[i])
		}
		return dot / math.Sqrt(na*nb)
	}
	nearCos, farCos := cosine(near), cosine(far)
	if !(nearCos > 0.85 && nearCos < 0.95 && farCos > 0.7 && farCos < 0.8) {
		t.Fatalf("the setup: the stand-in embedder puts %q at %.3f and %q at %.3f, want 0.85 to 0.95 and 0.7 to 0.8", near, nearCos, far, farCos)
	}

	for _, tc := range []struct {
		text  string
		floor any // nil: the default
		want  bool
		cos   float64
	}{
		{near, nil, true, nearCos},
		{near, 0.85, true, nearCos},
		{near, 0.95, false, nearCos},
		{far, nil, false, farCos},
		{far, 0.7, true, farCos},
		{far, 0.8, false, farCos},
		{goal, nil, true, 1},
		{goal, 1.0, true, 1},
	} {
		args := map[string]any{"text": tc.text}
		if tc.floor != nil {
			args["minSimilarity"] = tc.floor
		}
		got := lg.must(t, ctx, "builtin", "gamesFindSkill", args)
		if tc.want {
			if len(got) != 1 || got[0]["id"] != skill {
				t.Errorf("%q at a floor of %v: answered %v, want the skill (similarity %.3f)", tc.text, tc.floor, got, tc.cos)
				continue
			}
			if sim := asFloat(got[0]["similarity"], -1); math.Abs(sim-tc.cos) > 1e-3 {
				t.Errorf("%q at a floor of %v: similarity = %v, want %.4f", tc.text, tc.floor, got[0]["similarity"], tc.cos)
			}
		} else if len(got) != 0 {
			t.Errorf("%q at a floor of %v: answered %v, want no skill (similarity %.3f is under the floor)", tc.text, tc.floor, got, tc.cos)
		}
	}
}

// A stored vector with no direction (a zero vector) has a cosine that is not a number,
// which is neither under the floor nor at it. It must not be taken for a match: the skill
// is not answered for any text, however it is worded.
func TestLiveALookupByMeaningCountsAnUnusableVectorAsNoMatch(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	owner := "user-games-zero-find-" + n
	ctx := asOwner(owner)
	lookupCleanup(t, lg, owner)
	const goal = "Light up the area!"
	inputKey, trigger := "spot-"+n, "tick-"+n
	actions := []string{"collect", "craft", "placeTorches"}
	skill, sig := lookupTeach(t, lg, ctx, goal, inputKey, trigger, actions, "fz-"+n)
	lookupClimb(t, lg, ctx, sig, skill, actions, "trusted")
	ask := map[string]any{"text": goal}

	if got := lg.must(t, ctx, "builtin", "gamesFindSkill", ask); len(got) != 1 || got[0]["id"] != skill {
		t.Fatalf("control: before its vector is lost the skill is found by its own words: %v", got)
	}
	if _, err := lg.db.DB.Exec(`UPDATE `+lg.table+` SET embedding = $2::vector WHERE id = $1 AND vector_field = 'content'`,
		canonical(conceptSkill, skill), vectorLiteral(make([]float32, testDims))); err != nil {
		t.Fatalf("store the zero vector: %v", err)
	}
	for _, text := range []string{goal, "light up the area quickly", "go fishing by the river"} {
		got, err := lg.call(t, ctx, "builtin", "gamesFindSkill", map[string]any{"text": text})
		if err != nil || len(got) != 0 {
			t.Errorf("%q with the skill's vector unusable = %v, %v, want no match and no failure", text, got, err)
		}
	}
}

// byIDStore stands between the lookup and the engine for the one read that follows the
// statement: the skill the statement picked is read again, and what that read answers can
// differ from what the statement judged (a demotion landing in between, or a read that is
// not the caller's), and the read is made FRESH, so that it answers what the database
// judged and not what a cache remembers.
type byIDStore struct {
	rowStore
	mu      sync.Mutex
	rewrite func(rows []map[string]any) []map[string]any
	fresh   []bool
	ids     []string // the skillId of each of those reads, as it was sent
}

func (s *byIDStore) query(ctx context.Context, name string, args map[string]any) ([]map[string]any, error) {
	rows, err := s.rowStore.query(ctx, name, args)
	if name != "gamesSkillById" || err != nil {
		return rows, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fresh = append(s.fresh, memql.FreshReadFromContext(ctx))
	s.ids = append(s.ids, trimmed(args["skillId"]))
	if s.rewrite != nil {
		rows = s.rewrite(rows)
	}
	return rows, nil
}

// The statement judges the skill trusted and the owner its own; the lookup then answers
// the skill as that read finds it, and only if it still is the caller's and trusted.
func TestLiveALookupByMeaningAnswersOnlyWhatThePickedSkillIsNow(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	owner := "user-games-picked-" + n
	ctx := asOwner(owner)
	lookupCleanup(t, lg, owner)
	const goal = "Light up the area!"
	inputKey, trigger := "spot-"+n, "tick-"+n
	actions := []string{"collect", "craft", "placeTorches"}
	skill, sig := lookupTeach(t, lg, ctx, goal, inputKey, trigger, actions, "fp-"+n)
	lookupClimb(t, lg, ctx, sig, skill, actions, "trusted")
	store := &byIDStore{rowStore: lg.provider.store}
	lg.provider.store = store
	defer func() { lg.provider.store = store.rowStore }()
	find := func() []map[string]any {
		return lg.must(t, ctx, "builtin", "gamesFindSkill", map[string]any{"text": goal})
	}

	if got := find(); len(got) != 1 || got[0]["id"] != skill || got[0]["standby"] != false {
		t.Fatalf("control: the skill is found as it is: %v", got)
	}
	if len(store.fresh) != 1 || !store.fresh[0] {
		t.Errorf("the skill the statement picked was read with fresh=%v, want one fresh read", store.fresh)
	}
	if len(store.ids) != 1 || store.ids[0] != skill {
		t.Errorf("the skill the statement picked was read by %q, want the bare id %q the wire spells ids in (the statement answers the canonical one)", store.ids, skill)
	}

	store.rewrite = func(rows []map[string]any) []map[string]any {
		for _, r := range rows {
			r["rung"] = "shadow"
		}
		return rows
	}
	if got := find(); len(got) != 0 {
		t.Errorf("a skill demoted since the statement judged it was answered: %v", got)
	}
	store.rewrite = func(rows []map[string]any) []map[string]any {
		for _, r := range rows {
			r["ownerUserId"] = ownerKey("somebody-else-" + n)
		}
		return rows
	}
	if got := find(); len(got) != 0 {
		t.Errorf("a skill that is not the caller's was answered: %v", got)
	}
	store.rewrite = func([]map[string]any) []map[string]any { return nil }
	if got := find(); len(got) != 0 {
		t.Errorf("a skill that is gone since the statement judged it was answered: %v", got)
	}
}

// ---- refusals, through the engine ----

// The refusals reach a caller whole -- the code first, the argument named -- and a refused
// lookup reads nothing and moves nothing.
func TestLiveALookupRefusalsReachTheCallerWithTheirCodes(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	owner := "user-games-refuse-find-" + n
	ctx := asOwner(owner)
	lookupCleanup(t, lg, owner)
	const goal = "Light up the area!"
	inputKey, trigger := "spot-"+n, "tick-"+n
	actions := []string{"collect", "craft", "placeTorches"}
	skill, sig := lookupTeach(t, lg, ctx, goal, inputKey, trigger, actions, "fx-"+n)
	before := lg.stored(t, conceptSkill, skill)

	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{}, "games_invalid_argument: pass exactly one of goalSignature, trigger and text"},
		{map[string]any{"goalSignature": sig, "trigger": trigger}, "games_invalid_argument: pass exactly one of goalSignature, trigger and text"},
		{map[string]any{"trigger": trigger, "text": goal}, "games_invalid_argument: pass exactly one of goalSignature, trigger and text"},
		{map[string]any{"goalSignature": "nope"}, "games_invalid_argument: goalSignature must be 64 lower-case hexadecimal digits"},
		{map[string]any{"goalSignature": strings.ToUpper(sig)}, "games_invalid_argument: goalSignature must be 64 lower-case hexadecimal digits"},
		{map[string]any{"text": goal, "minSimilarity": 0}, "games_invalid_argument: minSimilarity must be a number above 0 and at most 1"},
		{map[string]any{"text": goal, "minSimilarity": 1.5}, "games_invalid_argument: minSimilarity must be a number above 0 and at most 1"},
	} {
		if _, err := lg.call(t, ctx, "builtin", "gamesFindSkill", tc.args); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("gamesFindSkill(%v) = %v, want it to lead with %q", tc.args, err, tc.want)
		}
	}
	// Only a lookup by meaning reads the floor: beside another key it is not read, and the
	// lookup is made as if it were not there.
	for _, args := range []map[string]any{{"trigger": trigger, "minSimilarity": -1}, {"goalSignature": sig, "minSimilarity": "high"}} {
		if got, err := lg.call(t, ctx, "builtin", "gamesFindSkill", args); err != nil || len(got) != 0 {
			t.Errorf("gamesFindSkill(%v) = %v, %v, want no skill (the one taught is in shadow) and no refusal: the floor is read by a lookup by meaning only", args, got, err)
		}
	}
	if after := lg.stored(t, conceptSkill, skill); after != before {
		t.Fatalf("refused lookups changed the skill: %d stored versions, was %d", after, before)
	}
}

// ---- the lists are cached reads; the skill answered is not ----

// A skill that fails twice is sent back to shadow, and a lookup must not run it again. The
// lists a lookup picks from are the engine's cached reads, and a write made through one
// replica reaches another replica's cache when the invalidation broadcast does (a test process
// runs no mesh, so nothing announces it): until then the other replica's lists still say the
// skill is trusted. The premise is OBSERVED, not assumed: the other replica's cache is warmed
// until it answers from it -- the two lists AND the skill read by id -- its ordinary reads are
// shown to be stale after the demotion and a fresh read to be current. Only then does the
// lookup's answer mean anything: it is not given from the stale lists, by signature or by
// trigger, and the skill it reads again is read past the stale cached read by id as well,
// which is what freshSkill's ContextWithFreshRead is for: without it the re-read would find
// the cached "trusted" and the lookup would answer the demoted skill all the same.
func TestLiveAReplicaWithAStaleCacheDoesNotAnswerADemotedSkill(t *testing.T) {
	here := newLiveGames(t)
	there := here.replica(t)
	n := runID()
	owner := "user-games-stale-find-" + n
	ctx := asOwner(owner)
	lookupCleanup(t, here, owner)
	const goal = "Light up the area!"
	inputKey, trigger := "spot-"+n, "tick-"+n
	actions := []string{"collect", "craft", "placeTorches"}
	skill, sig := lookupTeach(t, here, ctx, goal, inputKey, trigger, actions, "fv-"+n)
	lookupClimb(t, here, ctx, sig, skill, actions, "trusted")
	asks := lookupAsks(sig, trigger, goal)
	bySig, none := map[string]any{"goalSignature": sig}, map[string]any(nil)

	// The control: the other replica answers the skill while it is trusted.
	lookupHolds(t, there, ctx, "the other replica, before the demotion", asks, []string{skill, skill, skill}, owner)
	byID := map[string]any{"skillId": skill}
	there.warmed(t, ctx, "gamesSkillsForSignature", bySig)
	there.warmed(t, ctx, "gamesTrustedSkills", none)
	there.warmed(t, ctx, "gamesSkillById", byID)

	lookupRun(t, here, ctx, skill, 2, false)
	if rung := here.must(t, memql.ContextWithFreshRead(ctx), "query", "gamesSkillById", map[string]any{"skillId": skill})[0]["rung"]; rung != "shadow" {
		t.Fatalf("the setup: two failures left the skill %v, want shadow", rung)
	}

	rungIn := func(rows []map[string]any) any {
		for _, r := range rows {
			if bare(trimmed(r["id"])) == skill {
				return r["rung"]
			}
		}
		return nil
	}
	if got := rungIn(there.must(t, ctx, "query", "gamesSkillsForSignature", bySig)); got != "trusted" {
		t.Fatalf("the premise failed: the other replica's list of the goal's skills says %v, not the stale trusted, so the test proves nothing", got)
	}
	if got := rungIn(there.must(t, ctx, "query", "gamesTrustedSkills", none)); got != "trusted" {
		t.Fatalf("the premise failed: the other replica's list of the trusted skills says %v, not the stale trusted, so the test proves nothing", got)
	}
	if got := rungIn(there.must(t, ctx, "query", "gamesSkillById", byID)); got != "trusted" {
		t.Fatalf("the premise failed: the other replica's cached read of the skill by id says %v, not the stale trusted, so the test proves nothing of the fresh read", got)
	}
	if got := there.must(t, memql.ContextWithFreshRead(ctx), "query", "gamesSkillById", byID)[0]["rung"]; got != "shadow" {
		t.Fatalf("the control failed: a fresh read on the other replica says %v, want shadow", got)
	}

	lookupHolds(t, there, ctx, "the other replica, after the demotion", asks, []string{"", "", ""}, "")
}

// Skills of one goal that are all trusted embed the same words, so they are equally near any
// text. The statement breaks the tie by the skill's id, so the same skill answers every time:
// the runtime that is answered one of them reports its runs against it, and a lookup that
// alternated would spread the ladder's evidence over skills that did not earn it.
func TestLiveALookupByMeaningAnswersTheSameSkillOfEqualDistance(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	owner := "user-games-tie-find-" + n
	ctx := asOwner(owner)
	lookupCleanup(t, lg, owner)
	const goal = "Light up the area!"
	inputKey, trigger := "spot-"+n, "tick-"+n
	first, sig := lookupTeach(t, lg, ctx, goal, inputKey, trigger, []string{"collect", "craft", "placeTorches"}, "fq-"+n)
	lookupClimb(t, lg, ctx, sig, first, []string{"collect", "craft", "placeTorches"}, "trusted")
	second, _ := lookupTeach(t, lg, ctx, goal, inputKey, trigger, []string{"collect", "placeTorches"}, "fr-"+n)
	lookupClimb(t, lg, ctx, sig, second, []string{"collect", "placeTorches"}, "trusted")
	if first == second {
		t.Fatalf("two action sequences of one goal share the skill %s", first)
	}
	want := first
	if second < want { // a skill's id is gs- and 24 lower-case hexadecimal digits: byte order is the database's
		want = second
	}
	for i := 0; i < 8; i++ {
		got := lg.must(t, ctx, "builtin", "gamesFindSkill", map[string]any{"text": goal})
		if len(got) != 1 || got[0]["id"] != want {
			t.Fatalf("lookup %d by meaning answered %v, want the skill with the lowest id of the two equally near, %s", i+1, got, want)
		}
	}
}
