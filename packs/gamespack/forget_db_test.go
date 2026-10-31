package gamespack

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

// ---- helpers ----

// traceAbout is a plan trace to store, with the places a gamertag turns up in
// one: the goal, what the character said, the goal's inputs and the arguments of
// its one step. Everything else about it is the same for every trace.
type traceAbout struct {
	id        string
	character string // default wren
	goal      string // default look around
	say       string
	skill     string // the skill the plan ran, if it ran one
	inputs    map[string]any
	args      map[string]any
}

// writeTrace stores a plan trace as the owner. A plan trace is @serverWritten --
// only the pack's Go half writes one, under internal origin and the owner's
// actor -- so the test goes through the @serverOnly writer the same way.
func (lg *liveGames) writeTrace(t *testing.T, owner string, tr traceAbout) {
	t.Helper()
	if tr.character == "" {
		tr.character = "wren"
	}
	if tr.goal == "" {
		tr.goal = "look around"
	}
	args := map[string]any{
		"traceId": tr.id, "characterId": tr.character, "goalText": tr.goal,
		"goalSignature": strings.Repeat("ab", 32), "trigger": "initiative",
		"steps":   []any{map[string]any{"action": "look", "args": orEmpty(tr.args), "ok": true, "durationMs": 5}},
		"actions": []any{"look"}, "outcome": "success", "reaction": "neutral",
		"at": rfc3339(time.Now().UTC()),
	}
	if tr.say != "" {
		args["say"] = tr.say
	}
	if tr.skill != "" {
		args["skillId"] = tr.skill
	}
	if tr.inputs != nil {
		args["inputs"] = tr.inputs
	}
	lg.must(t, auth.ContextWithInternalOrigin(asOwner(owner)), "mutation", "createGamesPlanTrace", args)
}

func orEmpty(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// storedErr counts the stored VERSIONS of the rows of one concept with these bare
// ids: a row written twice counts twice, and a row that was deleted counts none. It
// reads through the pool, so through a connection of its own.
func (lg *liveGames) storedErr(concept string, bareIDs ...string) (int, error) {
	if len(bareIDs) == 0 {
		return 0, nil
	}
	params := make([]any, 0, len(bareIDs)+1)
	marks := make([]string, 0, len(bareIDs))
	for _, b := range bareIDs {
		params = append(params, canonical(concept, b))
		marks = append(marks, "$"+strconv.Itoa(len(params)))
	}
	params = append(params, concept)
	var n int
	q := `SELECT count(*) FROM "MemoryNodes" WHERE id IN (` + strings.Join(marks, ", ") + `) AND concept = $` + strconv.Itoa(len(params))
	err := lg.db.DB.QueryRow(q, params...).Scan(&n)
	return n, err
}

// stored is storedErr that fails the test on an error.
func (lg *liveGames) stored(t *testing.T, concept string, bareIDs ...string) int {
	t.Helper()
	n, err := lg.storedErr(concept, bareIDs...)
	if err != nil {
		t.Fatalf("count stored %s: %v", concept, err)
	}
	return n
}

// secondSpace is a second embedder space, with a vector table of its own. A cluster
// that changed embedders has a table for every model it has used, and forget has to
// empty all of them.
func (lg *liveGames) secondSpace(t *testing.T) string {
	t.Helper()
	table, err := memql.EnsureEmbeddingVectorTable(context.Background(), lg.db.DB, "games-test-second-space", 8)
	if err != nil {
		t.Fatalf("EnsureEmbeddingVectorTable: %v", err)
	}
	return table
}

// putVector stores a vector for a row in a table of the test's choosing.
func (lg *liveGames) putVector(t *testing.T, table, nodeID, concept string) {
	t.Helper()
	_, err := lg.db.DB.Exec(`INSERT INTO `+table+` (id, concept, vector_field, embedding, created_at, updated_at)
VALUES ($1, $2, 'content', $3::vector, NOW(), NOW())
ON CONFLICT (id, vector_field) DO UPDATE SET embedding = EXCLUDED.embedding, updated_at = NOW()`,
		nodeID, concept, "[1,0,0,0,0,0,0,0.5]")
	if err != nil {
		t.Fatalf("store a vector in %s: %v", table, err)
	}
}

// vectorsIn counts the vectors a table holds for these episodes.
func (lg *liveGames) vectorsIn(t *testing.T, table string, episodeIDs ...string) int {
	t.Helper()
	if len(episodeIDs) == 0 {
		return 0
	}
	params := make([]any, len(episodeIDs))
	marks := make([]string, len(episodeIDs))
	for i, id := range episodeIDs {
		params[i] = canonical(conceptEpisode, id)
		marks[i] = "$" + strconv.Itoa(i+1)
	}
	var n int
	if err := lg.db.DB.QueryRow(`SELECT count(*) FROM `+table+` WHERE id IN (`+strings.Join(marks, ", ")+`)`, params...).Scan(&n); err != nil {
		t.Fatalf("count vectors in %s: %v", table, err)
	}
	return n
}

// evictions is what a forget asks the engine to evict from its result caches.
type evictions struct {
	concepts []string // in the order they were asked
	stored   []int    // what was still stored when each was asked, -1 if it could not be counted
}

// recordInvalidations makes the provider say what it asks the engine to evict from its
// result caches, and still asks the engine. At each eviction it also asks stillStored,
// which counts through the pool, so through a connection of its own: rows that a
// transaction has deleted and not committed are still there for it, and rows that
// were committed are gone. So an eviction asked before the commit counts them
// still stored, and one asked after counts none. That is the difference between a
// cache that is dropped and stays dropped, and one that a reader refills from the
// database in the moment between the eviction and the commit, and keeps until it expires.
func (lg *liveGames) recordInvalidations(stillStored func() (int, error)) *evictions {
	ev := &evictions{}
	inner := lg.provider.invalidate
	lg.provider.invalidate = func(concept string) {
		n := -1
		if stillStored != nil {
			if got, err := stillStored(); err == nil {
				n = got
			}
		}
		ev.concepts = append(ev.concepts, concept)
		ev.stored = append(ev.stored, n)
		inner(concept)
	}
	return ev
}

// allZero reports whether every count is zero (and there is at least one).
func allZero(counts []int) bool {
	for _, n := range counts {
		if n != 0 {
			return false
		}
	}
	return len(counts) > 0
}

// theSevenConcepts are the concepts a forget deletes rows of, sorted.
func theSevenConcepts() []string {
	concepts := []string{conceptPlayer, conceptEpisode, conceptPreference, conceptPromise, conceptPlace, conceptPlanTrace, conceptSkill}
	sort.Strings(concepts)
	return concepts
}

// ---- the first test: everything about the player, and nothing else ----

// What forget removes, and what it leaves. Two players of one owner, each with
// every kind of row about them -- a row written twice, so that it has two versions,
// a plan trace that names them, a skill whose words name them (written twice, with a
// vector in the cluster's space), and a vector in each of two embedder spaces -- and
// Sam is forgotten. All of Sam goes, in every version and in both spaces, and all of
// Jo stays. The reads Sam's rows were served from are not left holding them.
func TestLiveForgetPlayerDeletesEverythingAboutThemAndNothingElse(t *testing.T) {
	lg := newLiveGames(t)
	second := lg.secondSpace(t)
	n := runID()
	owner := "user-games-forget-" + n
	ctx := asOwner(owner)
	now := time.Now().UTC()
	sam, jo := "p-sam-"+n, "p-jo-"+n

	type rowsOf struct{ player, preference, promise, episode, trace, skill string }
	ids := map[string]rowsOf{}
	for _, p := range []string{sam, jo} {
		r := rowsOf{player: p, preference: "pref-" + p, promise: "prom-" + p, episode: "e-" + p, trace: "t-" + p, skill: "gs-" + p}
		ids[p] = r
		tag := "Tag" + p
		lg.must(t, ctx, "mutation", "upsertGamesPlayer", map[string]any{"playerId": p, "gamertag": tag, "displayName": p, "consented": true})
		for _, valence := range []float64{0.9, 0.4} { // two versions
			lg.must(t, ctx, "mutation", "upsertGamesPreference", map[string]any{"preferenceId": r.preference, "playerId": p, "subject": "diamonds", "valence": valence, "confidence": 0.7})
		}
		lg.must(t, ctx, "mutation", "upsertGamesPromise", map[string]any{"promiseId": r.promise, "characterId": "wren", "playerId": p, "text": "build a den", "status": "open", "madeAt": rfc3339(now)})
		lg.must(t, ctx, "mutation", "setGamesPromiseStatus", map[string]any{"promiseId": r.promise, "status": "kept"})
		for _, summary := range []string{"played with " + p, "played with " + p + " again"} { // two versions
			lg.must(t, ctx, "builtin", "gamesRecordEpisodes", map[string]any{"episodes": episodesArg(episode(r.episode, "wren", p, summary, now, 4))})
		}
		lg.putVector(t, second, canonical(conceptEpisode, r.episode), conceptEpisode)
		lg.writeTrace(t, owner, traceAbout{id: r.trace, goal: "follow " + tag})
		lg.plantSkill(t, owner, skillSpec{id: r.skill, goal: "follow " + tag, bindings: []map[string]any{{"0.who": p}}})
		if err := lg.provider.space.store(context.Background(), canonical(conceptSkill, r.skill), conceptSkill, "follow "+tag); err != nil {
			t.Fatal(err)
		}
	}
	lg.writeTrace(t, owner, traceAbout{id: "t-nobody-" + n, goal: "build a den"})

	// what is stored for one player: versions of each kind of row, and vectors in each space
	stored := func(p string) map[string]int {
		r := ids[p]
		return map[string]int{
			"player": lg.stored(t, conceptPlayer, r.player), "preference": lg.stored(t, conceptPreference, r.preference),
			"promise": lg.stored(t, conceptPromise, r.promise), "episode": lg.stored(t, conceptEpisode, r.episode),
			"trace": lg.stored(t, conceptPlanTrace, r.trace), "skill": lg.stored(t, conceptSkill, r.skill),
			"vectors1": lg.vectorsIn(t, lg.table, r.episode), "vectors2": lg.vectorsIn(t, second, r.episode),
			"skillVectors": lg.vectorsOf(t, lg.table, conceptSkill, r.skill),
		}
	}
	whole := map[string]int{"player": 1, "preference": 2, "promise": 2, "episode": 2, "trace": 1, "skill": 2, "vectors1": 1, "vectors2": 1, "skillVectors": 1}
	for _, p := range []string{sam, jo} {
		if got := stored(p); !reflect.DeepEqual(got, whole) {
			t.Fatalf("before the forget, %s has %v stored, want %v", p, got, whole)
		}
	}
	// The reads that serve Sam's rows. They answer from the database; the engine's
	// result caches are what a forget must also be sure not to leave holding Sam's
	// rows, and the evictions it asks for are recorded.
	reads := map[string]map[string]any{
		"gamesPlayerById": {"playerId": sam}, "gamesEpisodesForPlayer": {"playerId": sam},
		"gamesPreferencesForPlayer": {"playerId": sam}, "gamesPromisesForPlayer": {"playerId": sam},
	}
	for name, args := range reads {
		if rows := lg.must(t, ctx, "query", name, args); len(rows) != 1 {
			t.Fatalf("before the forget, %s answers %d rows about Sam, want 1", name, len(rows))
		}
	}
	samStored := func() (int, error) {
		r, total := ids[sam], 0
		for _, c := range []struct{ concept, id string }{
			{conceptPlayer, r.player}, {conceptPreference, r.preference}, {conceptPromise, r.promise},
			{conceptEpisode, r.episode}, {conceptPlanTrace, r.trace}, {conceptSkill, r.skill},
		} {
			n, err := lg.storedErr(c.concept, c.id)
			if err != nil {
				return 0, err
			}
			total += n
		}
		return total, nil
	}
	evicted := lg.recordInvalidations(samStored)

	res := lg.must(t, ctx, "builtin", "gamesForgetPlayer", map[string]any{"playerId": sam})
	if res[0]["playerId"] != sam {
		t.Errorf("the answer names the player %v, want the bare id %s", res[0]["playerId"], sam)
	}
	want := map[string]int{"players": 1, "episodes": 1, "preferences": 1, "promises": 1, "planTraces": 1, "skills": 1, "vectors": 3}
	got := map[string]int{}
	for k := range want {
		got[k] = asInt(asObject(res[0]["deleted"])[k], -1)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("forget answers %v, want %v: the rows and not their versions, and a vector from each space", got, want)
	}
	if got := stored(sam); !reflect.DeepEqual(got, map[string]int{"player": 0, "preference": 0, "promise": 0, "episode": 0, "trace": 0, "skill": 0, "vectors1": 0, "vectors2": 0, "skillVectors": 0}) {
		t.Errorf("after the forget, Sam still has %v stored", got)
	}
	for name, args := range reads {
		if rows := lg.must(t, ctx, "query", name, args); len(rows) != 0 {
			t.Errorf("%s still answers %d row(s) about the forgotten player: %v", name, len(rows), rows)
		}
	}
	// A committed forget evicts the result caches of the seven concepts it deleted
	// rows of (a raw delete publishes no event, so nothing else would), once each,
	// and only once the deletes are committed: asked before, the eviction would find
	// Sam's rows still stored for any other connection.
	if !allZero(evicted.stored) {
		t.Errorf("the evictions were asked while %v of Sam's rows were still stored for another connection, want none: they come after the commit", evicted.stored)
	}
	if sort.Strings(evicted.concepts); !reflect.DeepEqual(evicted.concepts, theSevenConcepts()) {
		t.Errorf("a committed forget evicted %v, want each of %v once", evicted.concepts, theSevenConcepts())
	}

	// Jo, the owner's other player, is whole, to the last version and vector.
	if got := stored(jo); !reflect.DeepEqual(got, whole) {
		t.Errorf("forgetting Sam changed what is stored about Jo: %v, want %v", got, whole)
	}
	if got := lg.stored(t, conceptPlanTrace, "t-nobody-"+n); got != 1 {
		t.Errorf("forgetting Sam deleted the trace that names nobody")
	}
	if rows := lg.must(t, ctx, "query", "gamesEpisodesForPlayer", map[string]any{"playerId": jo}); len(rows) != 1 {
		t.Errorf("forgetting Sam touched what is read about Jo: %v", rows)
	}

	// Forgotten is forgotten: there is no player to forget again, and a stranger has
	// none to forget at all.
	*evicted = evictions{}
	if _, err := lg.call(t, ctx, "builtin", "gamesForgetPlayer", map[string]any{"playerId": sam}); err == nil || !strings.HasPrefix(err.Error(), "games_player_not_found: ") {
		t.Errorf("forgetting Sam again: %v, want games_player_not_found", err)
	}
	if _, err := lg.call(t, ctx, "builtin", "gamesForgetPlayer", map[string]any{"playerId": "p-nobody-" + n}); err == nil || !strings.HasPrefix(err.Error(), "games_player_not_found: ") {
		t.Errorf("forgetting a player that never was: %v, want games_player_not_found", err)
	}
	if _, err := lg.call(t, asOwner("user-games-stranger-"+n), "builtin", "gamesForgetPlayer", map[string]any{"playerId": jo}); err == nil || !strings.HasPrefix(err.Error(), "games_player_not_found: ") {
		t.Errorf("another owner forgetting Jo: %v, want games_player_not_found", err)
	}
	if got := stored(jo); !reflect.DeepEqual(got, whole) {
		t.Errorf("the refused forgets changed what is stored about Jo: %v, want %v", got, whole)
	}
	if len(evicted.concepts) != 0 {
		t.Errorf("forgets that were refused evicted %v, want nothing: they deleted nothing", evicted.concepts)
	}
}

// ---- a plan trace is matched on the gamertag as a whole name ----

// A plan trace carries no player id, so the only way to find the ones about a
// player is to look for the gamertag in their text. A substring would take the
// gamertag Tag out of "Tagger", "stage" and "tags", and delete traces about
// other players, other characters and nothing at all; the gamertag has to be
// there as a name, and in any case: "follow Tag now", the JSON string "Tag",
// "TAG!" and "Tag's house" all name the player.
func TestLiveForgetPlayerMatchesAPlanTraceOnTheWholeGamertag(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	owner := "user-games-forget-token-" + n
	ctx := asOwner(owner)
	player := "p-tag-" + n
	lg.must(t, ctx, "mutation", "upsertGamesPlayer", map[string]any{"playerId": player, "gamertag": "Tag", "displayName": "Tag", "consented": true})

	type named struct {
		what string
		tr   traceAbout
	}
	mentions := []named{
		{"in the goal", traceAbout{goal: "follow Tag now"}},
		{"as the whole goal", traceAbout{goal: "Tag"}},
		{"in what was said, in capitals", traceAbout{say: "HELLO TAG!"}},
		{"in what was said, in lower case", traceAbout{say: "tag, you are it"}},
		{"as a JSON string in the inputs", traceAbout{inputs: map[string]any{"target": "Tag"}}},
		{"as a JSON string in a step's arguments", traceAbout{args: map[string]any{"who": "tag"}}},
		{"as a possessive", traceAbout{goal: "visit Tag's house"}},
		{"between punctuation", traceAbout{goal: "meet (Tag), then Tag."}},
		{"as a JSON key", traceAbout{inputs: map[string]any{"Tag": 1}}},
		// In the JSON text of a trace a control character is spelled with a backslash
		// and a letter or four hexadecimal digits, and a name that follows it must
		// still be found: the letter that ends the spelling is not part of the name.
		{"after a newline in what was said", traceAbout{say: "Hello\nTag, come"}},
		{"after a tab in the goal", traceAbout{goal: "go\tTag"}},
		{"after a newline in a step's arguments", traceAbout{args: map[string]any{"who": "stand\nTag"}}},
		{"after a carriage return in an input", traceAbout{inputs: map[string]any{"target": "meet\rTag"}}},
		{"after a backspace", traceAbout{goal: "go\bTag"}},
		{"after a form feed", traceAbout{goal: "go\fTag"}},
		{"after the character U+0001", traceAbout{goal: "go\x01Tag"}},
		{"after the character U+001F", traceAbout{goal: "go\x1fTag"}},
		{"at the start of a value, after a newline", traceAbout{say: "\nTag"}},
		{"before a newline", traceAbout{goal: "see Tag\nthen go"}},
	}
	others := []named{
		{"Tagger", traceAbout{goal: "Tagger the zombie"}},
		{"stage", traceAbout{say: "set the stage"}},
		{"tags", traceAbout{goal: "collect tags"}},
		{"Tagged", traceAbout{goal: "Tagged and bagged"}},
		{"tagline", traceAbout{goal: "write a tagline"}},
		{"autotag", traceAbout{goal: "autotag the chests"}},
		{"tag_team with an underscore after", traceAbout{goal: "start a tag_team"}},
		{"_Tag with an underscore before", traceAbout{goal: "meet _Tag at the gate"}},
		{"Tag2 with a digit after", traceAbout{goal: "meet Tag2 at the gate"}},
		{"2Tag with a digit before", traceAbout{goal: "meet 2Tag at the gate"}},
		{"Tagger as a JSON string", traceAbout{inputs: map[string]any{"target": "Tagger"}}},
		{"stage as a step argument", traceAbout{args: map[string]any{"where": "stage"}}},
		{"Tagger after a newline", traceAbout{goal: "go\nTagger"}},
		{"stage after a tab", traceAbout{goal: "go\tstage"}},
		{"Tag2 after the character U+0001", traceAbout{goal: "go\x01Tag2"}},
		{"nobody", traceAbout{goal: "build a den"}},
	}
	var mentionIDs, otherIDs []string
	for i, m := range mentions {
		m.tr.id = fmt.Sprintf("t-yes-%d-%s", i, n)
		lg.writeTrace(t, owner, m.tr)
		mentionIDs = append(mentionIDs, m.tr.id)
	}
	for i, o := range others {
		o.tr.id = fmt.Sprintf("t-no-%d-%s", i, n)
		lg.writeTrace(t, owner, o.tr)
		otherIDs = append(otherIDs, o.tr.id)
	}
	if got := lg.stored(t, conceptPlanTrace, append(append([]string{}, mentionIDs...), otherIDs...)...); got != len(mentions)+len(others) {
		t.Fatalf("%d traces stored, want %d", got, len(mentions)+len(others))
	}

	res := lg.must(t, ctx, "builtin", "gamesForgetPlayer", map[string]any{"playerId": player})
	if got := asInt(asObject(res[0]["deleted"])["planTraces"], -1); got != len(mentions) {
		t.Errorf("forget deleted %d plan traces, want the %d that name the player: %v", got, len(mentions), res[0]["deleted"])
	}
	for i, m := range mentions {
		if got := lg.stored(t, conceptPlanTrace, mentionIDs[i]); got != 0 {
			t.Errorf("a trace naming the player %s survived the forget", m.what)
		}
	}
	for i, o := range others {
		if got := lg.stored(t, conceptPlanTrace, otherIDs[i]); got != 1 {
			t.Errorf("a trace with %q in it was deleted, though it does not name the player", o.what)
		}
	}
}

// ---- a gamertag matches only itself ----

// The gamertag is the player's, and the pattern that finds it is built from it, so
// every character a pattern, the JSON text of a trace or a SQL string treats
// specially must stand for itself: a leading "." (some gamertags carry a prefix
// that is no part of a name), the underscore (a wildcard in LIKE), each character
// of the pattern language, the quote and the backslash (escaped in the JSON text of
// a trace), a tab, an apostrophe and a letter outside ASCII. For each, the trace
// that names the player is deleted and the traces a pattern that did not take the
// character literally would also have matched are not.
func TestLiveForgetPlayerMatchesAGamertagOnlyAsItself(t *testing.T) {
	lg := newLiveGames(t)
	type kase struct {
		name     string
		gamertag string
		hits     []traceAbout // name the player
		decoys   []traceAbout // do not
	}
	cases := []kase{
		{"a leading dot and an underscore", ".Kid_Tag",
			[]traceAbout{{goal: "follow .Kid_Tag now"}, {inputs: map[string]any{"target": ".KID_TAG"}}, {goal: "follow\n.Kid_Tag"}},
			[]traceAbout{{goal: "follow aKidxTag"}, {goal: "follow .KidxTag"}, {goal: "follow .Kid-Tag"}, {goal: "follow Kid_Tag"}, {goal: "follow .Kid_Tagger"}, {goal: "follow Mr.Kid_Tag"}}},
	}
	for _, m := range `\.[](){}*+?|^$` {
		c := kase{name: "the pattern character " + string(m), gamertag: "A" + string(m) + "B",
			hits: []traceAbout{{goal: "see A" + string(m) + "B now"}}}
		switch m {
		case '.':
			c.decoys = []traceAbout{{goal: "see AxB now"}}
		case '*', '+', '?':
			c.decoys = []traceAbout{{goal: "see AB now"}, {goal: "see B now"}}
		case '|':
			c.decoys = []traceAbout{{goal: "see A now"}, {goal: "see B now"}}
		}
		cases = append(cases, c)
	}
	cases = append(cases,
		kase{"a double quote", `Say "Hi"`,
			[]traceAbout{{goal: `loop: Say "Hi" again`}, {inputs: map[string]any{"who": `Say "Hi"`}}},
			[]traceAbout{{goal: `loop: Say Hi again`}, {goal: `loop: Say \"Hi\" again`}}},
		kase{"backslashes", `C:\Users\Kid`,
			[]traceAbout{{goal: `open C:\Users\Kid please`}, {args: map[string]any{"path": `C:\Users\Kid`}}},
			[]traceAbout{{goal: `open C:\\Users\\Kid please`}, {goal: `open C:/Users/Kid please`}}},
		kase{"an apostrophe", "O'Neil",
			[]traceAbout{{goal: "wave at O'Neil"}},
			[]traceAbout{{goal: "wave at ONeil"}, {goal: "wave at O''Neil"}}},
		kase{"a tab", "A\tB",
			[]traceAbout{{goal: "see A\tB now"}},
			[]traceAbout{{goal: "see A B now"}, {goal: "see AtB now"}}},
		kase{"letters outside ASCII", "Zo\u00eb\u2013\u00c9mile",
			[]traceAbout{{goal: "greet Zo\u00eb\u2013\u00c9mile now"}},
			[]traceAbout{{goal: "greet Zoe-Emile now"}}},
	)

	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n := runID()
			owner := fmt.Sprintf("user-games-forget-meta-%d-%s", i, n)
			ctx := asOwner(owner)
			player := "p-meta-" + n
			lg.must(t, ctx, "mutation", "upsertGamesPlayer", map[string]any{"playerId": player, "gamertag": c.gamertag, "displayName": "x", "consented": true})
			var hitIDs, decoyIDs []string
			for j, tr := range c.hits {
				tr.id = fmt.Sprintf("t-hit-%d-%s", j, n)
				lg.writeTrace(t, owner, tr)
				hitIDs = append(hitIDs, tr.id)
			}
			for j, tr := range c.decoys {
				tr.id = fmt.Sprintf("t-decoy-%d-%s", j, n)
				lg.writeTrace(t, owner, tr)
				decoyIDs = append(decoyIDs, tr.id)
			}
			res, err := lg.call(t, ctx, "builtin", "gamesForgetPlayer", map[string]any{"playerId": player})
			if err != nil {
				t.Fatalf("forget a player whose gamertag is %q: %v", c.gamertag, err)
			}
			if got := asInt(asObject(res[0]["deleted"])["players"], 0); got != 1 {
				t.Fatalf("the player was not deleted: %v", res[0]["deleted"])
			}
			for j, id := range hitIDs {
				if lg.stored(t, conceptPlanTrace, id) != 0 {
					t.Errorf("hit %d: a trace that names the gamertag %q survived", j, c.gamertag)
				}
			}
			for j, id := range decoyIDs {
				if lg.stored(t, conceptPlanTrace, id) != 1 {
					t.Errorf("decoy %d (%q) was deleted, though it does not contain the gamertag %q", j, c.decoys[j].goal, c.gamertag)
				}
			}
		})
	}
}

// ---- only what a trace says about people counts ----

// Every trace the owner holds carries the owner's id, the id of the character, the
// skill it ran, the outcome, the reaction and the time, and none of them names a
// player. A gamertag that happens to equal a word in one of them -- a child called
// Wren in a world where the pal is called wren, a word of the owner's own id, a
// player called Skill, "success", the year -- would otherwise match EVERY trace the
// owner has and delete the evidence for every skill still to be learned. Those
// fields are left out of what is matched; what a character did and said is not.
func TestLiveForgetPlayerIgnoresTheParticularsOfATraceThatNameNoOnePlayer(t *testing.T) {
	lg := newLiveGames(t)
	year := strconv.Itoa(time.Now().UTC().Year())
	for _, tc := range []struct {
		name      string
		gamertag  string
		character string // the character of every trace, default wren
		skill     string // the skill every trace ran, if any
	}{
		{"a word of the owner's own id", "struct", "", ""},
		{"the id of the character", "Wren", "wren", ""},
		{"the skill a trace ran", "Skill", "", "sk-plain"},
		{"the outcome", "Success", "", ""},
		{"the reaction", "neutral", "", ""},
		{"the year of the time", year, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := runID()
			owner := "user-games-forget-struct-" + n
			ctx := asOwner(owner)
			player := "p-struct-" + n
			lg.must(t, ctx, "mutation", "upsertGamesPlayer", map[string]any{"playerId": player, "gamertag": tc.gamertag, "displayName": "x", "consented": true})
			plain := []string{"t-plain-1-" + n, "t-plain-2-" + n}
			lg.writeTrace(t, owner, traceAbout{id: plain[0], character: tc.character, skill: tc.skill, goal: "build a den"})
			lg.writeTrace(t, owner, traceAbout{id: plain[1], character: tc.character, skill: tc.skill, goal: "mine some stone", args: map[string]any{"x": 3}})
			named := "t-named-" + n
			lg.writeTrace(t, owner, traceAbout{id: named, character: tc.character, skill: tc.skill, goal: "wait for " + tc.gamertag + " at the gate"})

			res := lg.must(t, ctx, "builtin", "gamesForgetPlayer", map[string]any{"playerId": player})
			if got := asInt(asObject(res[0]["deleted"])["planTraces"], -1); got != 1 {
				t.Errorf("forget deleted %d plan traces, want only the one that names the player: %v", got, res[0]["deleted"])
			}
			if lg.stored(t, conceptPlanTrace, named) != 0 {
				t.Errorf("the trace that names the player survived")
			}
			if got := lg.stored(t, conceptPlanTrace, plain...); got != len(plain) {
				t.Errorf("%d of the %d traces that name no one were left, the rest deleted because the gamertag %q is %s", got, len(plain), tc.gamertag, tc.name)
			}
		})
	}
}

// ---- a player who was renamed ----

// A player's gamertag changes (the runtime upserts the player again), and a plan
// trace written before the change names the old one. The player row is kept in
// every version, so the names the player has held are known; forgetting the
// player on the current name alone would leave the traces from before the change.
func TestLiveForgetPlayerFindsTracesWrittenUnderAnEarlierGamertag(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	owner := "user-games-forget-renamed-" + n
	ctx := asOwner(owner)
	player := "p-renamed-" + n
	oldTag, newTag := "OldName"+n, "NewName"+n
	upsert := func(tag string) {
		lg.must(t, ctx, "mutation", "upsertGamesPlayer", map[string]any{"playerId": player, "gamertag": tag, "displayName": "x", "consented": true})
	}
	upsert(oldTag)
	upsert(newTag)
	if got := lg.stored(t, conceptPlayer, player); got != 2 {
		t.Fatalf("the renamed player has %d stored versions, want 2", got)
	}
	lg.writeTrace(t, owner, traceAbout{id: "t-old-" + n, goal: "follow " + oldTag})
	lg.writeTrace(t, owner, traceAbout{id: "t-new-" + n, goal: "follow " + newTag})
	lg.writeTrace(t, owner, traceAbout{id: "t-else-" + n, goal: "follow SomebodyElse" + n})

	res := lg.must(t, ctx, "builtin", "gamesForgetPlayer", map[string]any{"playerId": player})
	deleted := asObject(res[0]["deleted"])
	if asInt(deleted["planTraces"], -1) != 2 || asInt(deleted["players"], -1) != 1 {
		t.Errorf("forget deleted %v, want both traces that name the player and the one player", deleted)
	}
	if got := lg.stored(t, conceptPlanTrace, "t-old-"+n, "t-new-"+n); got != 0 {
		t.Errorf("%d trace(s) naming the player under an earlier or the current gamertag survived", got)
	}
	if got := lg.stored(t, conceptPlanTrace, "t-else-"+n); got != 1 {
		t.Errorf("the trace that names somebody else was deleted")
	}
	if got := lg.stored(t, conceptPlayer, player); got != 0 {
		t.Errorf("%d version(s) of the player row survived", got)
	}
}

// ---- the pattern's escaping is the database's ----

// jsonStringBody is what the server's own escaping writes between the quotes of a
// JSON string, which is how a gamertag stands in the text of a trace. The two are
// held to each other here, for the characters the escaping reads and for those it
// does not (a letter outside ASCII, the DEL character, a line separator, "/", an
// HTML-looking string), so that a gamertag Go and the server would spell
// differently is found here and not in a trace that is kept.
func TestLiveJSONStringBodyIsWhatTheDatabaseWrites(t *testing.T) {
	lg := newLiveGames(t)
	for _, s := range []string{
		"", "Tag", `"`, `\`, `a"b\c`, "\b\f\n\r\t", "\x01\x1f", "\x7f", "caf\xc3\xa9", "\xe2\x80\xa8\xe2\x80\xa9",
		"\xf0\x90\x90\x80", "</script>&amp;", "a/b", "\x5cu0041", `\"`, "tab\there", "Zo\xc3\xab\xe2\x80\x93\xc3\x89mile",
	} {
		var asString, asKey string
		if err := lg.db.DB.QueryRow(`SELECT to_jsonb($1::text)::text, jsonb_build_object($1::text, 1)::text`, s).Scan(&asString, &asKey); err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		if want := `"` + jsonStringBody(s) + `"`; asString != want {
			t.Errorf("%q: the database writes the string %s, jsonStringBody gives %s", s, asString, want)
		}
		if want := `{"` + jsonStringBody(s) + `": 1}`; asKey != want {
			t.Errorf("%q: the database writes the key as %s, jsonStringBody gives %s", s, asKey, want)
		}
	}
}

// ---- another owner's rows ----

// Forget deletes, so its reach is the whole question. Owner A has a player with
// everything about them; owner B has their own player, and also rows that name A's
// player by id (an owner can write any id into a playerId) and a trace that names
// A's gamertag. None of B's rows is A's to forget, and B has no right to forget A's.
//
//   - B asks to forget A's player: refused, as a signed-in user and as the cluster's
//     owner, whom the owned tier lets READ A's player (so only the pack's own check
//     stops that call; the control shows the read), and A's rows are all there.
//   - Each statement is scoped to its owner by itself, whatever the Go code before
//     it decides: run as a stranger it deletes nothing, and run as B against A's
//     player id and gamertag it reaches B's rows and none of A's.
//   - A forgets the player: all of A's rows about them go -- the positive control,
//     without which the rest proves nothing -- and every row of B's is still there,
//     the ones that name A's player included.
func TestLiveForgetPlayerNeverReachesAnotherOwnersRows(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	aName, bName, cName := "user-games-forget-a-"+n, "user-games-forget-b-"+n, "user-games-forget-c-"+n
	a, b := asOwner(aName), asOwner(bName)
	bAsClusterOwner := auth.ContextWithAccess(asOwner(bName), &auth.AccessContext{UserId: bName, Role: auth.RoleOwner})
	now := time.Now().UTC()
	pa, pb := "p-a-"+n, "p-b-"+n
	tagA, tagB := "TagA"+n, "TagB"+n

	// A's: a player, and a row of every kind about them
	lg.must(t, a, "mutation", "upsertGamesPlayer", map[string]any{"playerId": pa, "gamertag": tagA, "displayName": "A's", "consented": true})
	lg.must(t, a, "mutation", "upsertGamesPreference", map[string]any{"preferenceId": "pref-a-" + n, "playerId": pa, "subject": "diamonds", "valence": 0.9, "confidence": 0.7})
	lg.must(t, a, "mutation", "upsertGamesPromise", map[string]any{"promiseId": "prom-a-" + n, "characterId": "wren", "playerId": pa, "text": "build a den", "status": "open", "madeAt": rfc3339(now)})
	lg.must(t, a, "builtin", "gamesRecordEpisodes", map[string]any{"episodes": episodesArg(episode("e-a-"+n, "wren", pa, "A's memory of "+pa, now, 4))})
	lg.writeTrace(t, aName, traceAbout{id: "t-a-" + n, goal: "follow " + tagA})
	// ...and what names A's player without being about them, of every kind that can
	lg.writeEpisode(t, aName, "e-a-names-"+n, "", "A met "+tagA, nil)
	lg.writePreference(t, aName, "pref-a-names-"+n, "p-else-"+n, tagA+"'s pickaxe")
	lg.writePromise(t, aName, "prom-a-names-"+n, "p-else-"+n, "find "+tagA+"'s sword")
	lg.writePlace(t, aName, "pl-a-names-"+n, tagA+"'s base", nil)
	// B's: their own player, the same kinds of row about them, and rows that name A's player
	lg.must(t, b, "mutation", "upsertGamesPlayer", map[string]any{"playerId": pb, "gamertag": tagB, "displayName": "B's", "consented": true})
	lg.must(t, b, "mutation", "upsertGamesPreference", map[string]any{"preferenceId": "pref-b-" + n, "playerId": pb, "subject": "diamonds", "valence": 0.9, "confidence": 0.7})
	lg.must(t, b, "mutation", "upsertGamesPromise", map[string]any{"promiseId": "prom-b-" + n, "characterId": "wren", "playerId": pb, "text": "build a den", "status": "open", "madeAt": rfc3339(now)})
	lg.must(t, b, "builtin", "gamesRecordEpisodes", map[string]any{"episodes": episodesArg(
		episode("e-b-"+n, "wren", pb, "B's memory of "+pb, now, 4),
		episode("e-b-names-a-"+n, "wren", pa, "B's memory of a player id that is also A's", now, 4),
	)})
	lg.must(t, b, "mutation", "upsertGamesPreference", map[string]any{"preferenceId": "pref-b-names-a-" + n, "playerId": pa, "subject": "diamonds", "valence": 0.9, "confidence": 0.7})
	lg.must(t, b, "mutation", "upsertGamesPromise", map[string]any{"promiseId": "prom-b-names-a-" + n, "characterId": "wren", "playerId": pa, "text": "build a den", "status": "open", "madeAt": rfc3339(now)})
	lg.writeTrace(t, bName, traceAbout{id: "t-b-" + n, goal: "follow " + tagB})
	lg.writeTrace(t, bName, traceAbout{id: "t-b-names-a-" + n, goal: "follow " + tagA})
	// ...and B's rows that name A's gamertag in their text, one of every kind
	lg.writeEpisode(t, bName, "e-b-text-a-"+n, "", "B met "+tagA, nil)
	lg.writePreference(t, bName, "pref-b-text-a-"+n, "p-else-"+n, tagA+"'s pickaxe")
	lg.writePromise(t, bName, "prom-b-text-a-"+n, "p-else-"+n, "find "+tagA+"'s sword")
	lg.writePlace(t, bName, "pl-b-text-a-"+n, tagA+"'s base", nil)

	type group struct {
		concept string
		ids     []string
	}
	aRows := []group{{conceptPlayer, []string{pa}}, {conceptPreference, []string{"pref-a-" + n, "pref-a-names-" + n}}, {conceptPromise, []string{"prom-a-" + n, "prom-a-names-" + n}},
		{conceptEpisode, []string{"e-a-" + n, "e-a-names-" + n}}, {conceptPlanTrace, []string{"t-a-" + n}}, {conceptPlace, []string{"pl-a-names-" + n}}}
	bRows := []group{{conceptPlayer, []string{pb}}, {conceptPreference, []string{"pref-b-" + n, "pref-b-names-a-" + n, "pref-b-text-a-" + n}}, {conceptPromise, []string{"prom-b-" + n, "prom-b-names-a-" + n, "prom-b-text-a-" + n}},
		{conceptEpisode, []string{"e-b-" + n, "e-b-names-a-" + n, "e-b-text-a-" + n}}, {conceptPlanTrace, []string{"t-b-" + n, "t-b-names-a-" + n}}, {conceptPlace, []string{"pl-b-text-a-" + n}}}
	snapshot := func(groups ...[]group) map[string]int {
		out := map[string]int{}
		for _, gs := range groups {
			for _, g := range gs {
				for _, id := range g.ids {
					out[g.concept+" "+id] = lg.stored(t, g.concept, id)
					if g.concept == conceptEpisode {
						out["vector "+id] = lg.vectorsFor(t, id)
					}
				}
			}
		}
		return out
	}
	allThere := func(groups ...[]group) map[string]int {
		out := map[string]int{}
		for _, gs := range groups {
			for _, g := range gs {
				for _, id := range g.ids {
					out[g.concept+" "+id] = 1
					if g.concept == conceptEpisode {
						out["vector "+id] = 1
					}
				}
			}
		}
		return out
	}
	before := snapshot(aRows, bRows)
	if want := allThere(aRows, bRows); !reflect.DeepEqual(before, want) {
		t.Fatalf("the setup stored %v, want %v", before, want)
	}

	// B cannot forget A's player.
	if rows := lg.must(t, bAsClusterOwner, "query", "gamesPlayerById", map[string]any{"playerId": pa}); len(rows) != 1 {
		t.Fatalf("control: the cluster's owner reads %d of A's player, want 1 -- without it the next check does not test the pack's own", len(rows))
	}
	if rows := lg.must(t, b, "query", "gamesPlayerById", map[string]any{"playerId": pa}); len(rows) != 0 {
		t.Fatalf("control: B reads A's player: %v", rows)
	}
	for name, ctx := range map[string]context.Context{"a signed-in user": b, "the cluster's owner": bAsClusterOwner} {
		if _, err := lg.call(t, ctx, "builtin", "gamesForgetPlayer", map[string]any{"playerId": pa}); err == nil || !strings.HasPrefix(err.Error(), "games_player_not_found: ") {
			t.Errorf("%s forgetting A's player: %v, want games_player_not_found", name, err)
		}
	}
	if got := snapshot(aRows, bRows); !reflect.DeepEqual(got, before) {
		t.Fatalf("a refused forget changed what is stored: %v, was %v", got, before)
	}

	// Each statement is the owner's by itself. A transaction that is rolled back.
	tx, err := lg.db.DB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	deletes := func(query string, args ...any) []string {
		t.Helper()
		rows, err := tx.Query(query, args...)
		if err != nil {
			t.Fatalf("%.60s: %v", query, err)
		}
		defer rows.Close()
		cols, _ := rows.Columns()
		var ids []string
		for rows.Next() {
			dest := make([]any, len(cols))
			for i := range dest {
				dest[i] = new(string)
			}
			if err := rows.Scan(dest...); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, *dest[0].(*string))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return ids
	}
	playerA, patternA := canonical(conceptPlayer, pa), traceMentionPattern(tagA)
	for _, owner := range []string{ownerKey(cName), ownerKey("user-games-nobody-" + n)} {
		for _, concept := range []string{conceptEpisode, conceptPreference, conceptPromise} {
			if got := deletes(forgetByPlayerSQL, concept, owner, playerA); len(got) != 0 {
				t.Errorf("run as %s, the statement for %s deleted %v", owner, concept, got)
			}
		}
		if got := deletes(forgetTracesSQL, owner, patternA); len(got) != 0 {
			t.Errorf("run as %s, the statement for traces deleted %v", owner, got)
		}
		for name, query := range namedStatements {
			if got := deletes(query, owner, patternA); len(got) != 0 {
				t.Errorf("run as %s, the statement for %s that name the player deleted %v", owner, name, got)
			}
		}
		if got := deletes(forgetPlayerRowSQL, playerA, owner); len(got) != 0 {
			t.Errorf("run as %s, the statement for the player row deleted %v", owner, got)
		}
	}
	bKey := ownerKey(bName)
	for concept, want := range map[string]string{
		conceptEpisode: canonical(conceptEpisode, "e-b-names-a-"+n), conceptPreference: canonical(conceptPreference, "pref-b-names-a-"+n), conceptPromise: canonical(conceptPromise, "prom-b-names-a-"+n),
	} {
		if got := deletes(forgetByPlayerSQL, concept, bKey, playerA); !reflect.DeepEqual(got, []string{want}) {
			t.Errorf("run as B against A's player id, the statement for %s deleted %v, want only B's own %s", concept, got, want)
		}
	}
	if got := deletes(forgetTracesSQL, bKey, patternA); !reflect.DeepEqual(got, []string{canonical(conceptPlanTrace, "t-b-names-a-"+n)}) {
		t.Errorf("run as B against A's gamertag, the statement for traces deleted %v, want only B's own", got)
	}
	for name, want := range map[string]string{
		"episodes": canonical(conceptEpisode, "e-b-text-a-"+n), "preferences": canonical(conceptPreference, "pref-b-text-a-"+n),
		"promises": canonical(conceptPromise, "prom-b-text-a-"+n), "places": canonical(conceptPlace, "pl-b-text-a-"+n),
	} {
		if got := deletes(namedStatements[name], bKey, patternA); !reflect.DeepEqual(got, []string{want}) {
			t.Errorf("run as B against A's gamertag, the statement for the %s that name the player deleted %v, want only B's own %s", name, got, want)
		}
	}
	if got := deletes(forgetPlayerRowSQL, playerA, bKey); len(got) != 0 {
		t.Errorf("run as B, the statement for the player row deleted A's player: %v", got)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if got := snapshot(aRows, bRows); !reflect.DeepEqual(got, before) {
		t.Fatalf("the statements run as others left %v stored, was %v", got, before)
	}

	// A forgets the player: everything of A's about them goes, and nothing of B's.
	res := lg.must(t, a, "builtin", "gamesForgetPlayer", map[string]any{"playerId": pa})
	want := map[string]int{"players": 1, "episodes": 2, "preferences": 2, "promises": 2, "places": 1, "planTraces": 1, "vectors": 2}
	got := map[string]int{}
	for k := range want {
		got[k] = asInt(asObject(res[0]["deleted"])[k], -1)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("A's forget deleted %v, want %v: A's rows, and not B's that name the same player", got, want)
	}
	gone := map[string]int{}
	for k := range allThere(aRows) {
		gone[k] = 0
	}
	if got := snapshot(aRows); !reflect.DeepEqual(got, gone) {
		t.Errorf("after A's forget, %v of A's is stored, want none", got)
	}
	if got := snapshot(bRows); !reflect.DeepEqual(got, allThere(bRows)) {
		t.Errorf("after A's forget, B has %v stored, want every row and vector as it was", got)
	}
}

// namedStatements are the four statements that find the rows that name the player in their
// text, by the kind of row they delete.
var namedStatements = map[string]string{
	"episodes": forgetNamedEpisodesSQL, "preferences": forgetNamedPreferencesSQL,
	"promises": forgetNamedPromisesSQL, "places": forgetNamedPlacesSQL,
}

// ---- all or nothing ----

var errInjected = errors.New("injected failure")

// A forget that stopped half way would leave the caller believing a child was
// forgotten while their words were still stored, so it is one transaction. The
// forget is made to fail after each of its statements in turn, and what is stored
// is exactly what was before: the statements that had run are undone. It also
// fails, and deletes nothing, when it is handed a vector table it cannot name,
// because an erasure that skipped that table would say it was done with vectors
// left in it. The control is the same forget with nothing failing, whose
// evictions come after the commit.
func TestLiveForgetPlayerIsAllOrNothing(t *testing.T) {
	lg := newLiveGames(t)
	second := lg.secondSpace(t)
	n := runID()
	owner := "user-games-forget-atomic-" + n
	ctx := asOwner(owner)
	now := time.Now().UTC()
	player, tag := "p-atomic-"+n, "AtomicTag"+n
	lg.must(t, ctx, "mutation", "upsertGamesPlayer", map[string]any{"playerId": player, "gamertag": tag, "displayName": "x", "consented": true})
	lg.must(t, ctx, "mutation", "upsertGamesPreference", map[string]any{"preferenceId": "pref-" + n, "playerId": player, "subject": "diamonds", "valence": 0.9, "confidence": 0.7})
	lg.must(t, ctx, "mutation", "upsertGamesPromise", map[string]any{"promiseId": "prom-" + n, "characterId": "wren", "playerId": player, "text": "build a den", "status": "open", "madeAt": rfc3339(now)})
	lg.must(t, ctx, "builtin", "gamesRecordEpisodes", map[string]any{"episodes": episodesArg(episode("e-"+n, "wren", player, "played with "+player, now, 4))})
	lg.putVector(t, second, canonical(conceptEpisode, "e-"+n), conceptEpisode)
	lg.writeTrace(t, owner, traceAbout{id: "t-" + n, goal: "follow " + tag})
	lg.plantSkill(t, owner, skillSpec{id: "gs-" + n, goal: "follow " + tag})
	if err := lg.provider.space.store(context.Background(), canonical(conceptSkill, "gs-"+n), conceptSkill, "follow "+tag); err != nil {
		t.Fatal(err)
	}
	// ...and what names the player without being about them, one of each kind
	lg.writeEpisode(t, owner, "en-"+n, "p-else-"+n, "met "+tag, nil)
	lg.writePreference(t, owner, "pfn-"+n, "p-else-"+n, tag+"'s pickaxe")
	lg.writePromise(t, owner, "prn-"+n, "p-else-"+n, "find "+tag+"'s sword")
	lg.writePlace(t, owner, "pl-"+n, tag+"'s base", nil)

	snapshot := func() map[string]int {
		return map[string]int{
			"player": lg.stored(t, conceptPlayer, player), "preference": lg.stored(t, conceptPreference, "pref-"+n),
			"promise": lg.stored(t, conceptPromise, "prom-"+n), "episode": lg.stored(t, conceptEpisode, "e-"+n),
			"trace": lg.stored(t, conceptPlanTrace, "t-"+n), "skill": lg.stored(t, conceptSkill, "gs-"+n),
			"vectors1": lg.vectorsIn(t, lg.table, "e-"+n), "vectors2": lg.vectorsIn(t, second, "e-"+n),
			"skillVectors": lg.vectorsOf(t, lg.table, conceptSkill, "gs-"+n),
			"namedEpisode": lg.stored(t, conceptEpisode, "en-"+n), "namedPreference": lg.stored(t, conceptPreference, "pfn-"+n),
			"namedPromise": lg.stored(t, conceptPromise, "prn-"+n), "place": lg.stored(t, conceptPlace, "pl-"+n),
			"namedEpisodeVector": lg.vectorsOf(t, lg.table, conceptEpisode, "en-"+n),
		}
	}
	before := snapshot()
	if want := map[string]int{"player": 1, "preference": 1, "promise": 1, "episode": 1, "trace": 1, "skill": 1, "vectors1": 1, "vectors2": 1, "skillVectors": 1,
		"namedEpisode": 1, "namedPreference": 1, "namedPromise": 1, "place": 1, "namedEpisodeVector": 1}; !reflect.DeepEqual(before, want) {
		t.Fatalf("the setup stored %v, want %v", before, want)
	}

	playerStored := func() (int, error) {
		total := 0
		for _, c := range []struct{ concept, id string }{
			{conceptPlayer, player}, {conceptPreference, "pref-" + n}, {conceptPromise, "prom-" + n},
			{conceptEpisode, "e-" + n}, {conceptPlanTrace, "t-" + n}, {conceptSkill, "gs-" + n},
			{conceptEpisode, "en-" + n}, {conceptPreference, "pfn-" + n}, {conceptPromise, "prn-" + n}, {conceptPlace, "pl-" + n},
		} {
			got, err := lg.storedErr(c.concept, c.id)
			if err != nil {
				return 0, err
			}
			total += got
		}
		return total, nil
	}
	evicted := lg.recordInvalidations(playerStored)
	steps := []string{"players", "episodes", "preferences", "promises", "namedEpisodes", "namedPreferences", "namedPromises", "places", "planTraces", "skills", "vectors"}
	for i, failAt := range steps {
		var ran []string
		lg.provider.afterForgetStep = func(step string) error {
			ran = append(ran, step)
			if step == failAt {
				return errInjected
			}
			return nil
		}
		_, err := lg.call(t, ctx, "builtin", "gamesForgetPlayer", map[string]any{"playerId": player})
		if err == nil || !strings.Contains(err.Error(), errInjected.Error()) {
			t.Fatalf("a forget made to fail after %s: %v, want the failure", failAt, err)
		}
		if !reflect.DeepEqual(ran, steps[:i+1]) {
			t.Fatalf("a forget made to fail after %s ran %v, want %v: the statements before the failure must have run for the rollback to have undone them", failAt, ran, steps[:i+1])
		}
		if got := snapshot(); !reflect.DeepEqual(got, before) {
			t.Errorf("a forget that failed after %s left %v stored, was %v", failAt, got, before)
		}
		if rows := lg.must(t, ctx, "query", "gamesPlayerById", map[string]any{"playerId": player}); len(rows) != 1 {
			t.Errorf("a forget that failed after %s left the player unreadable: %v", failAt, rows)
		}
		if len(evicted.concepts) != 0 {
			t.Errorf("a forget that failed after %s evicted %v, want nothing: it deleted nothing", failAt, evicted.concepts)
		}
	}
	lg.provider.afterForgetStep = nil

	// A vector table it cannot name: the name is handed to it last, so that a forget
	// that deletes table by table has emptied the others by the time it reaches it.
	lg.provider.vectorTables = func(ctx context.Context, tx *sql.Tx) ([]string, error) {
		tables, err := memql.EmbeddingVectorTables(ctx, tx)
		return append(tables, `node_vectors; DROP TABLE "MemoryNodes"`), err
	}
	_, err := lg.call(t, ctx, "builtin", "gamesForgetPlayer", map[string]any{"playerId": player})
	if err == nil || !strings.HasPrefix(err.Error(), "games: forget vectors: ") || !strings.Contains(err.Error(), "no vector table's name") {
		t.Errorf("a forget handed a table it cannot name: %v, want it to fail as an internal error that names the table", err)
	}
	if got := snapshot(); !reflect.DeepEqual(got, before) {
		t.Errorf("a forget handed a table it cannot name left %v stored, was %v: nothing may be deleted, the rows before the vectors included", got, before)
	}
	if rows := lg.must(t, ctx, "query", "gamesPlayerById", map[string]any{"playerId": player}); len(rows) != 1 {
		t.Errorf("a forget handed a table it cannot name left the player unreadable: %v", rows)
	}
	if len(evicted.concepts) != 0 {
		t.Errorf("a forget handed a table it cannot name evicted %v, want nothing: it deleted nothing", evicted.concepts)
	}
	lg.provider.vectorTables = nil

	res := lg.must(t, ctx, "builtin", "gamesForgetPlayer", map[string]any{"playerId": player})
	if got := asInt(asObject(res[0]["deleted"])["vectors"], -1); got != 4 {
		t.Errorf("the control forget deleted %v, want four vectors: the episode's in two spaces, the other episode's and the skill's", res[0]["deleted"])
	}
	if got, want := snapshot(), map[string]int{"player": 0, "preference": 0, "promise": 0, "episode": 0, "trace": 0, "skill": 0, "vectors1": 0, "vectors2": 0, "skillVectors": 0,
		"namedEpisode": 0, "namedPreference": 0, "namedPromise": 0, "place": 0, "namedEpisodeVector": 0}; !reflect.DeepEqual(got, want) {
		t.Errorf("the forget that nothing stopped left %v stored", got)
	}
	if sort.Strings(evicted.concepts); !reflect.DeepEqual(evicted.concepts, theSevenConcepts()) {
		t.Errorf("the forget that nothing stopped evicted %v, want each of %v once", evicted.concepts, theSevenConcepts())
	}
	if !allZero(evicted.stored) {
		t.Errorf("the forget that nothing stopped asked for its evictions while %v of the player's rows were still stored for another connection, want none: they come after the commit", evicted.stored)
	}
}
