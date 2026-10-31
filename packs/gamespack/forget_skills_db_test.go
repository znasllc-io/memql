package gamespack

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
)

// forget_skills_db_test.go -- a forget also deletes the owner's skills whose words
// name the player. A skill keeps words: the goal it was learned for, the line a
// character says when it runs it, the constants of its template and the values of its
// last binding. The gamertag is found in them as a whole name, in any case, by the rule
// the plan traces are found by; a key of an object, an action's name and a hole's id are
// the runtime's own words and never a person's.

// skillSpec is what a test says about a skill it plants: the words the skill keeps.
type skillSpec struct {
	id       string
	goal     string           // default "do a thing"
	say      string           // the confirmation, if any
	steps    []TemplateStep   // the template's steps; default one step with a constant that names nobody
	bindings []map[string]any // the lastBinding of each ladder move, in order; the last is the skill's
}

// plantSkill stores a skill for owner the way the pack stores one -- the @serverOnly
// writer under internal origin -- and one version more for each binding, written by the
// ladder's writer. It returns the skill's bare id.
func (lg *liveGames) plantSkill(t *testing.T, owner string, spec skillSpec) string {
	t.Helper()
	if spec.goal == "" {
		spec.goal = "do a thing"
	}
	if spec.steps == nil {
		spec.steps = []TemplateStep{{Action: "look", Args: map[string]ArgSpec{"where": {Const: "around"}}}}
	}
	holes, err := holeIDs(spec.steps)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := Template{Steps: spec.steps, Holes: holes}
	tm, err := tmpl.AsMap()
	if err != nil {
		t.Fatal(err)
	}
	actions := make([]any, len(spec.steps))
	for i, s := range spec.steps {
		actions[i] = s.Action
	}
	create := map[string]any{
		"skillId": spec.id, "goalText": spec.goal, "goalSignature": GoalSignature(spec.goal, nil), "actions": actions,
		"template": tm, "templateDigest": tmpl.Digest(),
	}
	if spec.say != "" {
		create["confirmation"] = spec.say
	}
	lg.must(t, auth.ContextWithInternalOrigin(asOwner(owner)), "mutation", "createGamesSkill", create)
	for i, b := range spec.bindings {
		err := lg.provider.store.recordLadder(asOwner(owner), map[string]any{
			"skillId": spec.id, "versionAt": rfc3339(time.Now().Add(time.Duration(i+1) * time.Hour)), "rung": "shadow", "state": "shadow",
			"shadowMatches": 1, "canaryMatches": 0, "distinctBindings": map[string]any{}, "failures": 0, "promotionApprovalId": "",
			"lastReason": "compared", "lastBinding": b,
		})
		if err != nil {
			t.Fatalf("write version %d of skill %s: %v", i+1, spec.id, err)
		}
	}
	return spec.id
}

// vectorsOf counts the vectors a table holds for these rows of a concept.
func (lg *liveGames) vectorsOf(t *testing.T, table, concept string, bareIDs ...string) int {
	t.Helper()
	if len(bareIDs) == 0 {
		return 0
	}
	params := make([]any, len(bareIDs))
	marks := make([]string, len(bareIDs))
	for i, id := range bareIDs {
		params[i] = canonical(concept, id)
		marks[i] = "$" + strconv.Itoa(i+1)
	}
	var n int
	if err := lg.db.DB.QueryRow(`SELECT count(*) FROM `+table+` WHERE id IN (`+strings.Join(marks, ", ")+`)`, params...).Scan(&n); err != nil {
		t.Fatalf("count vectors in %s: %v", table, err)
	}
	return n
}

// constStep is a step with one constant argument.
func constStep(action, key string, value any) TemplateStep {
	return TemplateStep{Action: action, Args: map[string]ArgSpec{key: {Const: value}}}
}

// What forget deletes of the skills, and what it leaves. The owner has a player whose
// gamertag is Tag and a skill for every place a skill keeps words, each naming it as a
// whole name in some case; and the skills that must stay: those whose words contain Tag
// only as part of a longer word (Tagger, stage, tags, autotag, Tag2, _Tag, 2Tag), those
// where it is a KEY or the name of an action, one that names nobody, and another
// owner's that names this very gamertag. Every version of a deleted skill goes, with
// its vectors in every embedder space, and the answer counts the skills.
func TestLiveForgetPlayerDeletesTheSkillsThatNameThem(t *testing.T) {
	lg := newLiveGames(t)
	second := lg.secondSpace(t)
	n := runID()
	owner, other := "user-games-forget-skills-"+n, "user-games-forget-skills-other-"+n
	ctx := asOwner(owner)
	player := "p-tag-" + n
	lg.must(t, ctx, "mutation", "upsertGamesPlayer", map[string]any{"playerId": player, "gamertag": "Tag", "displayName": "Tag", "consented": true})
	jo := "p-jo-" + n
	lg.must(t, ctx, "mutation", "upsertGamesPlayer", map[string]any{"playerId": jo, "gamertag": "Jo", "displayName": "Jo", "consented": true})

	type named struct {
		what string
		spec skillSpec
	}
	hits := []named{
		{"in the goal", skillSpec{goal: "follow Tag home"}},
		{"as the whole goal", skillSpec{goal: "TAG"}},
		{"in the line a character says", skillSpec{say: "Hello, tag!"}},
		{"in a constant of the template", skillSpec{steps: []TemplateStep{constStep("talk", "who", "Tag")}}},
		{"in a constant, as a possessive", skillSpec{steps: []TemplateStep{constStep("visit", "place", "Tag's house")}}},
		{"in an object that is a constant", skillSpec{steps: []TemplateStep{constStep("talk", "who", map[string]any{"names": []any{"Sam", "tag"}})}}},
		{"in a constant of the second step", skillSpec{steps: []TemplateStep{constStep("look", "where", "around"), constStep("wave", "at", "Tag")}}},
		{"in the last binding", skillSpec{bindings: []map[string]any{{"0.who": "Tag"}}}},
		{"in the last binding, nested", skillSpec{bindings: []map[string]any{{"0.who": map[string]any{"deep": []any{"x", "Tag"}}}}}},
		{"after a newline in a constant", skillSpec{steps: []TemplateStep{constStep("say", "text", "hello\nTag, come")}}},
		{"after a tab in the goal", skillSpec{goal: "go\tTag"}},
		{"in a binding that a later one replaced", skillSpec{bindings: []map[string]any{{"0.who": "Tag"}, {"0.who": "Bob"}}}},
		{"in the newest of several versions", skillSpec{bindings: []map[string]any{{"0.who": "Bob"}, {"0.who": "Sam"}, {"0.who": "Tag"}}}},
	}
	keeps := []named{
		{"Tagger in the goal", skillSpec{goal: "Tagger the zombie"}},
		{"stage in what is said", skillSpec{say: "set the stage"}},
		{"tags in a constant", skillSpec{steps: []TemplateStep{constStep("collect", "what", "tags")}}},
		{"Tagged in a nested constant", skillSpec{steps: []TemplateStep{constStep("collect", "what", map[string]any{"l": []any{"Tagged and bagged"}})}}},
		{"autotag in the last binding", skillSpec{bindings: []map[string]any{{"0.who": "autotag"}}}},
		{"tag_team with an underscore after", skillSpec{goal: "start a tag_team"}},
		{"_Tag with an underscore before", skillSpec{say: "meet _Tag at the gate"}},
		{"Tag2 with a digit after", skillSpec{steps: []TemplateStep{constStep("meet", "who", "Tag2")}}},
		{"2Tag with a digit before", skillSpec{bindings: []map[string]any{{"0.who": "2Tag"}}}},
		{"Tagger after a newline", skillSpec{steps: []TemplateStep{constStep("say", "text", "go\nTagger")}}},
		// The runtime's own words are not a person's: a gamertag equal to a key, an action
		// or a hole is found in no skill because of it.
		{"Tag as the name of an action", skillSpec{steps: []TemplateStep{constStep("Tag", "where", "around")}}},
		{"Tag as the name of an argument", skillSpec{steps: []TemplateStep{constStep("look", "Tag", "around")}}},
		{"Tag as a key inside a constant", skillSpec{steps: []TemplateStep{constStep("look", "at", map[string]any{"Tag": 1, "list": []any{map[string]any{"Tag": "x"}}})}}},
		{"Tag as a key of the last binding", skillSpec{bindings: []map[string]any{{"Tag": "x", "deep": map[string]any{"Tag": []any{"y"}}}}}},
		{"a number and a boolean", skillSpec{steps: []TemplateStep{constStep("count", "n", 5), constStep("flag", "on", true)}, bindings: []map[string]any{{"0.n": 5}}}},
		{"nothing of the player", skillSpec{goal: "Light up the area!", say: "Torches coming up!", bindings: []map[string]any{{"0.count": 4}}}},
	}
	var hitIDs, keepIDs []string
	plant := func(owner string, i int, prefix string, nm named) string {
		nm.spec.id = fmt.Sprintf("gs-%s-%d-%s", prefix, i, n)
		lg.plantSkill(t, owner, nm.spec)
		// a vector in the cluster's embedder space, as the learner's embedding makes, and one in
		// a second space, as a cluster that changed embedders has
		if err := lg.provider.space.store(context.Background(), canonical(conceptSkill, nm.spec.id), conceptSkill, nm.spec.goal); err != nil {
			t.Fatalf("embed skill %s: %v", nm.spec.id, err)
		}
		lg.putVector(t, second, canonical(conceptSkill, nm.spec.id), conceptSkill)
		return nm.spec.id
	}
	versions := map[string]int{}
	for i, h := range hits {
		id := plant(owner, i, "yes", h)
		hitIDs = append(hitIDs, id)
		versions[id] = lg.stored(t, conceptSkill, id)
	}
	for i, k := range keeps {
		id := plant(owner, i, "no", k)
		keepIDs = append(keepIDs, id)
		versions[id] = lg.stored(t, conceptSkill, id)
	}
	// Another owner's skill names this very gamertag, and so does a skill named for Jo.
	theirs := plant(other, 0, "theirs", named{"another owner's", skillSpec{goal: "follow Tag home"}})
	joSkill := plant(owner, 0, "jo", named{"names Jo", skillSpec{goal: "follow Jo home"}})
	versions[theirs], versions[joSkill] = lg.stored(t, conceptSkill, theirs), lg.stored(t, conceptSkill, joSkill)
	multi := 0
	for _, h := range hits {
		if len(h.spec.bindings) > 1 {
			multi++
		}
	}
	if multi == 0 || versions[hitIDs[len(hitIDs)-1]] != 4 || versions[hitIDs[len(hitIDs)-2]] != 3 {
		t.Fatalf("the setup: the skills with several versions have %v, want 3 and 4 stored", versions)
	}

	// The reads that serve the skills. They answer from the database; the engine's
	// result caches are what a forget must also be sure not to leave holding them.
	if rows := lg.must(t, ctx, "query", "gamesSkillById", map[string]any{"skillId": hitIDs[0]}); len(rows) != 1 {
		t.Fatalf("before the forget, the skill is read as %d rows, want 1", len(rows))
	}

	res := lg.must(t, ctx, "builtin", "gamesForgetPlayer", map[string]any{"playerId": player})
	deleted := asObject(res[0]["deleted"])
	if asInt(deleted["skills"], -1) != len(hits) || asInt(deleted["players"], -1) != 1 {
		t.Errorf("forget answers %v, want %d skills (the skills and not their versions) and the one player", deleted, len(hits))
	}
	if want := 2 * len(hits); asInt(deleted["vectors"], -1) != want {
		t.Errorf("forget deleted %v vectors, want %d: a vector of each deleted skill from each of the two spaces", deleted["vectors"], want)
	}
	for i, id := range hitIDs {
		if got := lg.stored(t, conceptSkill, id); got != 0 {
			t.Errorf("a skill that names the player %s survived the forget, with %d version(s)", hits[i].what, got)
		}
		if got := lg.vectorsOf(t, lg.table, conceptSkill, id) + lg.vectorsOf(t, second, conceptSkill, id); got != 0 {
			t.Errorf("the vectors of a deleted skill %s survived: %d", hits[i].what, got)
		}
	}
	for i, id := range keepIDs {
		if got := lg.stored(t, conceptSkill, id); got != versions[id] {
			t.Errorf("a skill with %s has %d versions after the forget, was %d: it does not name the player", keeps[i].what, got, versions[id])
		}
		if got := lg.vectorsOf(t, lg.table, conceptSkill, id) + lg.vectorsOf(t, second, conceptSkill, id); got != 2 {
			t.Errorf("the skill with %s lost its vectors: %d left of 2", keeps[i].what, got)
		}
	}
	for what, id := range map[string]string{"another owner's skill": theirs, "a skill that names Jo": joSkill} {
		if got := lg.stored(t, conceptSkill, id); got != versions[id] {
			t.Errorf("%s has %d versions after the forget, was %d", what, got, versions[id])
		}
		if got := lg.vectorsOf(t, lg.table, conceptSkill, id) + lg.vectorsOf(t, second, conceptSkill, id); got != 2 {
			t.Errorf("%s lost its vectors: %d left of 2", what, got)
		}
	}
	if rows := lg.must(t, ctx, "query", "gamesSkillById", map[string]any{"skillId": hitIDs[0]}); len(rows) != 0 {
		t.Errorf("a deleted skill is still read: %v", rows)
	}
	if rows := lg.must(t, ctx, "query", "gamesSkillById", map[string]any{"skillId": keepIDs[0]}); len(rows) != 1 {
		t.Errorf("a skill that was kept reads as %d rows after the forget, want 1", len(rows))
	}
	if rows := lg.must(t, asOwner(other), "query", "gamesSkillById", map[string]any{"skillId": theirs}); len(rows) != 1 {
		t.Errorf("another owner's skill reads as %d rows after the forget, want 1", len(rows))
	}

	// Forgetting a player who is named by no skill deletes no skill, and says so.
	res = lg.must(t, ctx, "builtin", "gamesForgetPlayer", map[string]any{"playerId": jo})
	if got := asInt(asObject(res[0]["deleted"])["skills"], -1); got != 1 {
		t.Errorf("forgetting Jo deleted %d skills, want the one that names Jo", got)
	}
	if got := lg.stored(t, conceptSkill, keepIDs...); got != sumVersions(versions, keepIDs) {
		t.Errorf("forgetting Jo changed the skills that name nobody: %d versions, want %d", got, sumVersions(versions, keepIDs))
	}
}

func sumVersions(versions map[string]int, ids []string) int {
	total := 0
	for _, id := range ids {
		total += versions[id]
	}
	return total
}

// A skill is deleted whole when any version of it names the player: the name may have been
// in a binding the next one overwrote, and an older version left behind would be the
// skill's newest again, with its stale ladder. A skill whose versions never name the
// player keeps all of them.
func TestLiveForgetPlayerDeletesASkillInEveryVersion(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	owner := "user-games-forget-versions-" + n
	ctx := asOwner(owner)
	player := "p-versions-" + n
	lg.must(t, ctx, "mutation", "upsertGamesPlayer", map[string]any{"playerId": player, "gamertag": "Tag", "displayName": "x", "consented": true})
	older := lg.plantSkill(t, owner, skillSpec{id: "gs-older-" + n, bindings: []map[string]any{{"0.who": "Tag"}, {"0.who": "Bob"}, {"0.who": "Sam"}}})
	newest := lg.plantSkill(t, owner, skillSpec{id: "gs-newest-" + n, bindings: []map[string]any{{"0.who": "Bob"}, {"0.who": "Tag"}}})
	clean := lg.plantSkill(t, owner, skillSpec{id: "gs-clean-" + n, bindings: []map[string]any{{"0.who": "Bob"}, {"0.who": "Sam"}}})
	if lg.stored(t, conceptSkill, older) != 4 || lg.stored(t, conceptSkill, newest) != 3 || lg.stored(t, conceptSkill, clean) != 3 {
		t.Fatalf("the setup stored %d, %d and %d versions, want 4, 3 and 3", lg.stored(t, conceptSkill, older), lg.stored(t, conceptSkill, newest), lg.stored(t, conceptSkill, clean))
	}
	res := lg.must(t, ctx, "builtin", "gamesForgetPlayer", map[string]any{"playerId": player})
	if got := asInt(asObject(res[0]["deleted"])["skills"], -1); got != 2 {
		t.Errorf("forget deleted %d skills, want the two that named the player in some version", got)
	}
	if got := lg.stored(t, conceptSkill, older, newest); got != 0 {
		t.Errorf("%d version(s) of the skills that named the player survived", got)
	}
	if got := lg.stored(t, conceptSkill, clean); got != 3 {
		t.Errorf("the skill that never named the player has %d versions, want its 3", got)
	}
}

// A skill is found by the gamertag the player has now or has had, as a trace is.
func TestLiveForgetPlayerFindsSkillsNamingAnEarlierGamertag(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	owner := "user-games-forget-renamed-skill-" + n
	ctx := asOwner(owner)
	player := "p-renamed-" + n
	oldTag, newTag := "OldName"+n, "NewName"+n
	for _, tag := range []string{oldTag, newTag} {
		lg.must(t, ctx, "mutation", "upsertGamesPlayer", map[string]any{"playerId": player, "gamertag": tag, "displayName": "x", "consented": true})
	}
	lg.plantSkill(t, owner, skillSpec{id: "gs-old-" + n, goal: "follow " + oldTag})
	lg.plantSkill(t, owner, skillSpec{id: "gs-new-" + n, say: "hi " + newTag})
	lg.plantSkill(t, owner, skillSpec{id: "gs-else-" + n, goal: "follow SomebodyElse" + n})
	res := lg.must(t, ctx, "builtin", "gamesForgetPlayer", map[string]any{"playerId": player})
	if got := asInt(asObject(res[0]["deleted"])["skills"], -1); got != 2 {
		t.Errorf("forget deleted %d skills, want the two that name the player under either gamertag", got)
	}
	if lg.stored(t, conceptSkill, "gs-old-"+n, "gs-new-"+n) != 0 || lg.stored(t, conceptSkill, "gs-else-"+n) != 1 {
		t.Errorf("the skills were not deleted as they should be")
	}
}

// The gamertag is the player's and the pattern is built from it, so every character a
// pattern or a JSON string treats specially must stand for itself in a skill's words too:
// as a string value of a template or a binding is spelled in jsonb's own text.
func TestLiveForgetPlayerMatchesAGamertagOnlyAsItselfInASkill(t *testing.T) {
	lg := newLiveGames(t)
	for i, c := range []struct {
		name, gamertag string
		hits, decoys   []skillSpec
	}{
		{"a leading dot and an underscore", ".Kid_Tag",
			[]skillSpec{{goal: "follow .Kid_Tag now"}, {say: ".KID_TAG!"}, {steps: []TemplateStep{constStep("talk", "who", "go\n.Kid_Tag")}}},
			[]skillSpec{{goal: "follow aKidxTag"}, {goal: "follow .KidxTag"}, {goal: "follow Kid_Tag"}, {goal: "follow Mr.Kid_Tag"}}},
		{"the pattern characters", `A.+*?[](){}|^$B`,
			[]skillSpec{{goal: `see A.+*?[](){}|^$B now`}, {bindings: []map[string]any{{"0.who": `A.+*?[](){}|^$B`}}}},
			[]skillSpec{{goal: "see AxB now"}, {goal: "see AB now"}}},
		{"a double quote and a backslash", `Say "Hi" C:\Kid`,
			[]skillSpec{{goal: `loop: Say "Hi" C:\Kid again`}, {steps: []TemplateStep{constStep("talk", "who", `Say "Hi" C:\Kid`)}}},
			[]skillSpec{{goal: `loop: Say Hi C:\Kid again`}, {goal: `loop: Say \"Hi\" C:\\Kid again`}}},
		{"letters outside ASCII", "Zo\u00eb\u2013\u00c9mile",
			[]skillSpec{{goal: "greet Zo\u00eb\u2013\u00c9mile now"}, {bindings: []map[string]any{{"0.who": "Zo\u00eb\u2013\u00c9mile"}}}},
			[]skillSpec{{goal: "greet Zoe-Emile now"}}},
		{"a tab", "A\tB", []skillSpec{{goal: "see A\tB now"}}, []skillSpec{{goal: "see A B now"}, {goal: "see AtB now"}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			n := runID()
			owner := fmt.Sprintf("user-games-forget-skill-meta-%d-%s", i, n)
			ctx := asOwner(owner)
			player := "p-meta-" + n
			lg.must(t, ctx, "mutation", "upsertGamesPlayer", map[string]any{"playerId": player, "gamertag": c.gamertag, "displayName": "x", "consented": true})
			var hitIDs, decoyIDs []string
			for j, s := range c.hits {
				s.id = fmt.Sprintf("gs-hit-%d-%s", j, n)
				hitIDs = append(hitIDs, lg.plantSkill(t, owner, s))
			}
			for j, s := range c.decoys {
				s.id = fmt.Sprintf("gs-decoy-%d-%s", j, n)
				decoyIDs = append(decoyIDs, lg.plantSkill(t, owner, s))
			}
			res, err := lg.call(t, ctx, "builtin", "gamesForgetPlayer", map[string]any{"playerId": player})
			if err != nil {
				t.Fatalf("forget a player whose gamertag is %q: %v", c.gamertag, err)
			}
			if got := asInt(asObject(res[0]["deleted"])["skills"], -1); got != len(c.hits) {
				t.Errorf("forget deleted %d skills, want %d", got, len(c.hits))
			}
			for j, id := range hitIDs {
				if lg.stored(t, conceptSkill, id) != 0 {
					t.Errorf("hit %d: a skill that names the gamertag %q survived", j, c.gamertag)
				}
			}
			for j, id := range decoyIDs {
				if lg.stored(t, conceptSkill, id) < 1 {
					t.Errorf("decoy %d (%q) was deleted, though it does not contain the gamertag %q", j, c.decoys[j].goal, c.gamertag)
				}
			}
		})
	}
}

// Forget deletes, so its reach is the whole question. Owner A forgets a player; B has a
// skill that names the same gamertag, and none of it is A's to forget. Each statement is
// the owner's by itself: run as a stranger it deletes nothing, and run as B against A's
// gamertag it reaches B's own skill and none of A's.
func TestLiveForgetPlayerNeverDeletesAnotherOwnersSkills(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	aName, bName, cName := "user-games-forget-skill-a-"+n, "user-games-forget-skill-b-"+n, "user-games-forget-skill-c-"+n
	a := asOwner(aName)
	tagA := "TagA" + n
	lg.must(t, a, "mutation", "upsertGamesPlayer", map[string]any{"playerId": "p-a-" + n, "gamertag": tagA, "displayName": "A's", "consented": true})
	mineA := lg.plantSkill(t, aName, skillSpec{id: "gs-a-" + n, goal: "follow " + tagA})
	theirs := lg.plantSkill(t, bName, skillSpec{id: "gs-b-names-a-" + n, goal: "follow " + tagA, bindings: []map[string]any{{"0.who": tagA}}})
	theirsToo := lg.plantSkill(t, bName, skillSpec{id: "gs-b-" + n, goal: "follow somebody else"})

	// Each statement is the owner's by itself. A transaction that is rolled back.
	tx, err := lg.db.DB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	run := func(owner string) []string {
		t.Helper()
		ids, err := deleteIDs(context.Background(), tx, forgetSkillsSQL, owner, traceMentionPattern(tagA))
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for id := range ids {
			out = append(out, id)
		}
		sort.Strings(out)
		return out
	}
	for _, owner := range []string{ownerKey(cName), ownerKey("user-games-nobody-" + n)} {
		if got := run(owner); len(got) != 0 {
			t.Errorf("run as %s, the statement for skills deleted %v", owner, got)
		}
	}
	if got, want := run(ownerKey(bName)), []string{canonical(conceptSkill, theirs)}; !reflect.DeepEqual(got, want) {
		t.Errorf("run as B against A's gamertag, the statement for skills deleted %v, want only B's own %v", got, want)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if got := lg.stored(t, conceptSkill, mineA, theirs, theirsToo); got != 4 {
		t.Fatalf("the statements run as others left %d versions of the three skills stored, want the 1, 2 and 1 there were", got)
	}

	// A forgets the player: A's skill goes, and B's, which names the same gamertag, stays.
	res := lg.must(t, a, "builtin", "gamesForgetPlayer", map[string]any{"playerId": "p-a-" + n})
	if got := asInt(asObject(res[0]["deleted"])["skills"], -1); got != 1 {
		t.Errorf("A's forget deleted %d skills, want A's one", got)
	}
	if lg.stored(t, conceptSkill, mineA) != 0 || lg.stored(t, conceptSkill, theirs) != 2 || lg.stored(t, conceptSkill, theirsToo) != 1 {
		t.Errorf("after A's forget, A's skill has %d versions and B's have %d and %d, want 0, 2 and 1",
			lg.stored(t, conceptSkill, mineA), lg.stored(t, conceptSkill, theirs), lg.stored(t, conceptSkill, theirsToo))
	}
}

// A row id is one owner's by construction, but the owned tier's write guard lets an
// internal-origin write through onto an id somebody else holds, so a row CAN carry
// versions of two owners (the takeover the pack refuses, written here the way an
// earlier build once allowed it). A forget deletes its own owner's versions, and the
// words that decide are the owner's own: a version of another owner's that names the
// gamertag is not a reason to delete mine, and mine that names it is not a reason to
// delete theirs.
func TestLiveForgetPlayerNeverDeletesAVersionAnotherOwnerWrote(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	aName, bName := "user-games-forget-two-a-"+n, "user-games-forget-two-b-"+n
	a := asOwner(aName)
	tag := "TwoTag" + n
	lg.must(t, a, "mutation", "upsertGamesPlayer", map[string]any{"playerId": "p-two-" + n, "gamertag": tag, "displayName": "x", "consented": true})

	// X: A's version names the gamertag, B's later version of the same id does not.
	// Y: A's version names nobody, B's later version names the gamertag.
	x, y := "gs-two-x-"+n, "gs-two-y-"+n
	lg.plantSkill(t, aName, skillSpec{id: x, goal: "follow " + tag})
	time.Sleep(5 * time.Millisecond)
	lg.plantSkill(t, bName, skillSpec{id: x, goal: "dig a trench"})
	lg.plantSkill(t, aName, skillSpec{id: y, goal: "dig a trench"})
	time.Sleep(5 * time.Millisecond)
	lg.plantSkill(t, bName, skillSpec{id: y, goal: "follow " + tag})
	owned := func(id, owner string) int {
		var c int
		if err := lg.db.DB.QueryRow(`SELECT count(*) FROM "MemoryNodes" WHERE concept = $1 AND id = $2 AND payload->>'ownerUserId' = $3`,
			conceptSkill, canonical(conceptSkill, id), ownerKey(owner)).Scan(&c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	if owned(x, aName) != 1 || owned(x, bName) != 1 || owned(y, aName) != 1 || owned(y, bName) != 1 {
		t.Fatalf("the setup: each id should carry one version of each owner")
	}

	res := lg.must(t, a, "builtin", "gamesForgetPlayer", map[string]any{"playerId": "p-two-" + n})
	if got := asInt(asObject(res[0]["deleted"])["skills"], -1); got != 1 {
		t.Errorf("forget deleted %d skills, want the one whose version of A's names the player", got)
	}
	if owned(x, aName) != 0 || owned(x, bName) != 1 {
		t.Errorf("after the forget, X has %d version(s) of A's and %d of B's, want 0 and 1: A's own version goes and B's stays", owned(x, aName), owned(x, bName))
	}
	if owned(y, aName) != 1 || owned(y, bName) != 1 {
		t.Errorf("after the forget, Y has %d version(s) of A's and %d of B's, want both: nothing of A's names the player", owned(y, aName), owned(y, bName))
	}
}

// A gamertag that is blank has no name to look for, so no trace and no skill is looked for
// (a pattern of nothing would match everything); the answer still has every key.
func TestLiveForgetPlayerWithABlankGamertagDeletesNoSkill(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	owner := "user-games-forget-blank-" + n
	ctx := asOwner(owner)
	player := "p-blank-" + n
	if _, err := lg.call(t, ctx, "mutation", "upsertGamesPlayer", map[string]any{"playerId": player, "gamertag": " ", "displayName": "x", "consented": true}); err != nil {
		t.Skipf("the engine does not store a blank gamertag (%v), so there is no such player to forget", err)
	}
	skill := lg.plantSkill(t, owner, skillSpec{id: "gs-blank-" + n, goal: "follow somebody"})
	res := lg.must(t, ctx, "builtin", "gamesForgetPlayer", map[string]any{"playerId": player})
	deleted := asObject(res[0]["deleted"])
	for _, k := range []string{"players", "episodes", "preferences", "promises", "places", "planTraces", "skills", "vectors"} {
		if _, ok := deleted[k]; !ok {
			t.Errorf("the answer has no %s: %v", k, deleted)
		}
	}
	if asInt(deleted["skills"], -1) != 0 || asInt(deleted["players"], -1) != 1 || lg.stored(t, conceptSkill, skill) != 1 {
		t.Errorf("forgetting a player with a blank gamertag answered %v and left %d versions of a skill that names nobody", deleted, lg.stored(t, conceptSkill, skill))
	}
}
