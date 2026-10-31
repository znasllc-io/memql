package gamespack

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// forget_named_db_test.go -- a forget also deletes the owner's episodes (whatever their
// player), preferences, promises and places that name the gamertag. They have free text that a
// person or a character wrote -- an episode's summary, a preference's subject, a promise's
// text, a place's name -- and the gamertag is found in it as a whole name, in any case, by the
// rule the plan traces and the skills are found by. The rest of a row is identifiers and enums
// and is not read: ids, a character, a world, a kind, the character that found a place
// (discoveredBy), the ids a preference was inferred from (evidence), a time, a status. A
// gamertag that equals or begins an id (a player tagged like a pal, or like the prefix of every
// session id) would delete every row that has one.

// memoryRows are the rows a test writes for one owner, to see what a forget takes and leaves.
type memoryRows struct {
	episodes, preferences, promises, places []string // bare ids
}

func (m memoryRows) all() []struct{ concept, id string } {
	var out []struct{ concept, id string }
	for _, e := range m.episodes {
		out = append(out, struct{ concept, id string }{conceptEpisode, e})
	}
	for _, e := range m.preferences {
		out = append(out, struct{ concept, id string }{conceptPreference, e})
	}
	for _, e := range m.promises {
		out = append(out, struct{ concept, id string }{conceptPromise, e})
	}
	for _, e := range m.places {
		out = append(out, struct{ concept, id string }{conceptPlace, e})
	}
	return out
}

// writeEpisode records one episode as the owner. character and world are the runtime's
// words; they default to wren and meadow.
func (lg *liveGames) writeEpisode(t *testing.T, owner, id, player, summary string, tweak func(map[string]any)) {
	t.Helper()
	e := episode(id, "wren", player, summary, time.Now().UTC(), 4)
	if tweak != nil {
		tweak(e)
	}
	lg.must(t, asOwner(owner), "builtin", "gamesRecordEpisodes", map[string]any{"episodes": episodesArg(e)})
}

func (lg *liveGames) writePreference(t *testing.T, owner, id, player, subject string) {
	t.Helper()
	lg.writePreferenceWith(t, owner, id, player, subject, nil)
}

// writePreferenceWith writes a preference with the evidence it was inferred from, one string for
// each element (nil writes none).
func (lg *liveGames) writePreferenceWith(t *testing.T, owner, id, player, subject string, evidence []string) {
	t.Helper()
	args := map[string]any{"preferenceId": id, "playerId": player, "subject": subject, "valence": 0.5, "confidence": 0.6}
	if evidence != nil {
		list := make([]any, len(evidence))
		for i, e := range evidence {
			list[i] = e
		}
		args["evidence"] = list
	}
	lg.must(t, asOwner(owner), "mutation", "upsertGamesPreference", args)
}

func (lg *liveGames) writePromise(t *testing.T, owner, id, player, text string) {
	t.Helper()
	lg.must(t, asOwner(owner), "mutation", "upsertGamesPromise", map[string]any{"promiseId": id, "characterId": "wren", "playerId": player, "text": text, "status": "open", "madeAt": rfc3339(time.Now().UTC())})
}

func (lg *liveGames) writePlace(t *testing.T, owner, id, name string, tweak func(map[string]any)) {
	t.Helper()
	args := map[string]any{"placeId": id, "worldId": "meadow", "name": name, "kind": "base", "position": map[string]any{"x": 1, "y": 2, "z": 3}, "discoveredBy": "wren", "at": rfc3339(time.Now().UTC())}
	if tweak != nil {
		tweak(args)
	}
	lg.must(t, asOwner(owner), "mutation", "upsertGamesPlace", args)
}

// The owner has two players, Tag and Jo, and the rows of every kind that Jo's memories hold
// about Tag: an episode about Jo, a pal's own episode about nobody, a preference of Jo's, a
// promise to Jo, a place, all of which name Tag as a whole name, in some case; and the rows
// that do not: Tagger, stage, tags, Tagged, autotag, tag_team, _Tag, Tag2, 2Tag, and a row
// whose character, world, kind, finder, evidence or player id is Tag (identifiers, not read).
// Forgetting Tag deletes the first kind, in every version, with the vectors of the episodes,
// and leaves the second kind and every row of another owner's that names Tag.
func TestLiveForgetPlayerDeletesTheMemoriesThatNameThem(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	owner, other := "user-games-forget-named-"+n, "user-games-forget-named-other-"+n
	ctx := asOwner(owner)
	tag, jo := "p-tag-"+n, "p-jo-"+n
	lg.must(t, ctx, "mutation", "upsertGamesPlayer", map[string]any{"playerId": tag, "gamertag": "Tag", "displayName": "Tag", "consented": true})
	lg.must(t, ctx, "mutation", "upsertGamesPlayer", map[string]any{"playerId": jo, "gamertag": "Jo", "displayName": "Jo", "consented": true})
	id := func(kind string, i int) string { return fmt.Sprintf("%s-%d-%s", kind, i, n) }

	// ---- what names Tag
	var hits memoryRows
	hit := func(list *[]string, kind string, i int) string {
		v := id(kind, i)
		*list = append(*list, v)
		return v
	}
	lg.writeEpisode(t, owner, hit(&hits.episodes, "e-yes", 0), jo, "Tag built a den with Jo", nil)
	lg.writeEpisode(t, owner, hit(&hits.episodes, "e-yes", 1), "", "TAG waved at the gate", nil)
	lg.writeEpisode(t, owner, hit(&hits.episodes, "e-yes", 2), jo, "went past tag's house", nil)
	lg.writeEpisode(t, owner, hit(&hits.episodes, "e-yes", 3), tag, "played with them", nil) // about Tag: found by the player's id
	// a row rewritten: it names Tag in its first version only
	rewritten := hit(&hits.episodes, "e-yes", 4)
	lg.writeEpisode(t, owner, rewritten, jo, "Tag helped build", nil)
	lg.writeEpisode(t, owner, rewritten, jo, "someone helped build", nil)
	// a row found twice: its first version is about Tag, its second names Tag and is about Jo; it
	// is one row, counted once
	both := hit(&hits.episodes, "e-yes", 5)
	lg.writeEpisode(t, owner, both, tag, "waved at the gate", nil)
	lg.writeEpisode(t, owner, both, jo, "Tag waved again", nil)
	lg.writePreference(t, owner, hit(&hits.preferences, "pf-yes", 0), jo, "playing with Tag")
	rewrittenPref := hit(&hits.preferences, "pf-yes", 1)
	lg.writePreference(t, owner, rewrittenPref, jo, "Tag's pickaxe")
	lg.writePreference(t, owner, rewrittenPref, jo, "pickaxes")
	lg.writePreference(t, owner, hit(&hits.preferences, "pf-yes", 2), tag, "diamonds") // about Tag
	named := hit(&hits.promises, "pr-yes", 0)
	lg.writePromise(t, owner, named, jo, "I will find Tag's sword")
	lg.must(t, ctx, "mutation", "setGamesPromiseStatus", map[string]any{"promiseId": named, "status": "kept"}) // a second version
	lg.writePromise(t, owner, hit(&hits.promises, "pr-yes", 1), tag, "build a den")                            // to Tag
	lg.writePlace(t, owner, hit(&hits.places, "pl-yes", 0), "Tag's farm", nil)
	lg.writePlace(t, owner, hit(&hits.places, "pl-yes", 1), "tag HQ", nil)

	// ---- what does not
	var keeps memoryRows
	keep := func(list *[]string, kind string, i int) string {
		v := id(kind, i)
		*list = append(*list, v)
		return v
	}
	decoys := []string{"Tagger the zombie", "set the stage", "collect tags", "Tagged and bagged", "autotag the chests", "start a tag_team", "meet _Tag at the gate", "meet Tag2 at the gate", "meet 2Tag at the gate", "Tagger after\nTagger"}
	for i, d := range decoys {
		lg.writeEpisode(t, owner, keep(&keeps.episodes, "e-no", i), jo, d, nil)
	}
	// Tag is the runtime's word here (a pal called Tag, a world called Tag, a player whose id is
	// Tag), and not the row's text
	lg.writeEpisode(t, owner, keep(&keeps.episodes, "e-no", len(decoys)), jo, "nobody is here", func(e map[string]any) {
		e["characterId"], e["worldId"], e["playerId"] = "Tag", "Tag", "Tag"
	})
	for i, d := range decoys[:4] {
		lg.writePreference(t, owner, keep(&keeps.preferences, "pf-no", i), jo, d)
		lg.writePromise(t, owner, keep(&keeps.promises, "pr-no", i), jo, d)
		lg.writePlace(t, owner, keep(&keeps.places, "pl-no", i), d, nil)
	}
	// a place of kind Tag in a world called Tag, found by a pal: the runtime's words, not read
	lg.writePlace(t, owner, keep(&keeps.places, "pl-no", 4), "the old farm", func(a map[string]any) {
		a["kind"], a["worldId"] = "Tag", "Tag"
	})
	// a place found by a pal called Tag, in either case: an id, not the place's text, and not read
	lg.writePlace(t, owner, keep(&keeps.places, "pl-no", 5), "a cave", func(a map[string]any) { a["discoveredBy"] = "Tag" })
	lg.writePlace(t, owner, keep(&keeps.places, "pl-no", 6), "a hill", func(a map[string]any) { a["discoveredBy"] = "TAG" })
	for i, finder := range []string{"Tagger", "stage", "tag_team"} {
		lg.writePlace(t, owner, keep(&keeps.places, "pl-no", 7+i), "somewhere", func(a map[string]any) { a["discoveredBy"] = finder })
	}
	lg.writePlace(t, owner, keep(&keeps.places, "pl-no", 10), "nowhere", func(a map[string]any) { delete(a, "discoveredBy") })
	lg.writePromise(t, owner, keep(&keeps.promises, "pr-no", 4), jo, "nobody here")
	lg.must(t, ctx, "mutation", "upsertGamesPromise", map[string]any{
		"promiseId": keep(&keeps.promises, "pr-no", 5), "characterId": "Tag", "playerId": "Tag", "text": "nobody here", "status": "open", "madeAt": rfc3339(time.Now().UTC()),
	})
	// a preference of a player whose id is Tag, and preferences inferred from ids that are Tag, or
	// begin with it, or hold it between two dashes, in any case: ids, not the preference's text, and
	// not read
	lg.writePreferenceWith(t, owner, keep(&keeps.preferences, "pf-no", 4), "Tag", "nothing at all", []string{"e-12-" + n})
	lg.writePreferenceWith(t, owner, keep(&keeps.preferences, "pf-no", 5), jo, "talking", []string{"Tag", "tag-0123", "e-TAG-4", "Tagger", "set the stage"})
	lg.writePreferenceWith(t, owner, keep(&keeps.preferences, "pf-no", 6), jo, "listening", []string{})

	// another owner holds rows that name Tag, and they are not this owner's to forget
	var theirs memoryRows
	lg.writeEpisode(t, other, "e-other-"+n, "", "Tag was here", nil)
	theirs.episodes = append(theirs.episodes, "e-other-"+n)
	lg.writePreference(t, other, "pf-other-"+n, jo, "Tag's pickaxe")
	theirs.preferences = append(theirs.preferences, "pf-other-"+n)
	lg.writePromise(t, other, "pr-other-"+n, jo, "I will find Tag's sword")
	theirs.promises = append(theirs.promises, "pr-other-"+n)
	lg.writePlace(t, other, "pl-other-"+n, "Tag's farm", nil)
	theirs.places = append(theirs.places, "pl-other-"+n)

	versions := map[string]int{}
	for _, group := range []memoryRows{hits, keeps, theirs} {
		for _, r := range group.all() {
			versions[r.concept+" "+r.id] = lg.stored(t, r.concept, r.id)
		}
	}
	if versions[conceptEpisode+" "+rewritten] != 2 || versions[conceptPreference+" "+rewrittenPref] != 2 || versions[conceptPromise+" "+named] != 2 {
		t.Fatalf("the setup: the rows written twice have %v versions", versions)
	}
	vectors := func(ids []string) int { return lg.vectorsOf(t, lg.table, conceptEpisode, ids...) }
	if got := vectors(hits.episodes); got != len(hits.episodes) {
		t.Fatalf("the setup: %d vectors for %d episodes", got, len(hits.episodes))
	}
	// the reads that serve them, so that a cache could hold what is deleted
	for name, args := range map[string]map[string]any{
		"gamesPlacesInWorld": {"worldId": "meadow"}, "gamesEpisodesForPlayer": {"playerId": jo},
		"gamesPreferencesForPlayer": {"playerId": jo}, "gamesPromisesForPlayer": {"playerId": jo},
	} {
		lg.must(t, ctx, "query", name, args)
	}

	res := lg.must(t, ctx, "builtin", "gamesForgetPlayer", map[string]any{"playerId": tag})
	deleted := asObject(res[0]["deleted"])
	// episodes, preferences and promises count what was about the player and what names the
	// gamertag, once each; places, which are about nobody, count the ones that name it
	want := map[string]int{"players": 1, "episodes": len(hits.episodes), "preferences": len(hits.preferences), "promises": len(hits.promises), "places": len(hits.places), "planTraces": 0, "skills": 0, "vectors": len(hits.episodes)}
	got := map[string]int{}
	for k := range want {
		got[k] = asInt(deleted[k], -1)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("forget answers %v, want %v: the rows and not their versions, each once", got, want)
	}
	for _, r := range hits.all() {
		if v := lg.stored(t, r.concept, r.id); v != 0 {
			t.Errorf("%s %s survived the forget with %d version(s), though it names the player (or is about them)", r.concept, r.id, v)
		}
	}
	for what, group := range map[string]memoryRows{"this owner's rows that name nobody": keeps, "another owner's rows": theirs} {
		for _, r := range group.all() {
			if v := lg.stored(t, r.concept, r.id); v != versions[r.concept+" "+r.id] {
				t.Errorf("%s: %s %s has %d version(s) after the forget, was %d", what, r.concept, r.id, v, versions[r.concept+" "+r.id])
			}
		}
	}
	if got := vectors(append(append([]string{}, keeps.episodes...), theirs.episodes...)); got != len(keeps.episodes)+len(theirs.episodes) {
		t.Errorf("the episodes that stayed have %d vectors, want one each (%d)", got, len(keeps.episodes)+len(theirs.episodes))
	}
	if got := vectors(hits.episodes); got != 0 {
		t.Errorf("%d vector(s) of the deleted episodes survived", got)
	}
	// the reads no longer hold them
	// (the last place that stayed is in the world called Tag, not the meadow)
	if rows := lg.must(t, ctx, "query", "gamesPlacesInWorld", map[string]any{"worldId": "meadow"}); len(rows) != len(keeps.places)-1 {
		t.Errorf("gamesPlacesInWorld answers %d places after the forget, want the %d of the meadow that stayed", len(rows), len(keeps.places)-1)
	}
	var left []string
	for _, row := range lg.must(t, ctx, "query", "gamesEpisodesForPlayer", map[string]any{"playerId": jo}) {
		left = append(left, bare(trimmed(row["id"])))
	}
	sort.Strings(left)
	// (the last of the episodes that stayed is about the player whose id is Tag, not Jo)
	if want := sortedCopy(keeps.episodes[:len(keeps.episodes)-1]); !reflect.DeepEqual(left, want) {
		t.Errorf("gamesEpisodesForPlayer answers %v after the forget, want %v", left, want)
	}
}

// A gamertag the player had, and has not now, is found too, as in a trace; and a gamertag
// with no name in it, or a name no row holds, deletes nothing but the player.
func TestLiveForgetPlayerFindsMemoriesNamingAnEarlierGamertag(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	owner := "user-games-forget-named-renamed-" + n
	ctx := asOwner(owner)
	player := "p-renamed-" + n
	oldTag, newTag := "OldName"+n, "NewName"+n
	for _, tag := range []string{oldTag, newTag} {
		lg.must(t, ctx, "mutation", "upsertGamesPlayer", map[string]any{"playerId": player, "gamertag": tag, "displayName": "x", "consented": true})
	}
	lg.writeEpisode(t, owner, "e-old-"+n, "", "met "+oldTag, nil)
	lg.writePlace(t, owner, "pl-new-"+n, newTag+"'s base", nil)
	lg.writePromise(t, owner, "pr-else-"+n, "p-else-"+n, "meet SomebodyElse"+n)
	res := lg.must(t, ctx, "builtin", "gamesForgetPlayer", map[string]any{"playerId": player})
	deleted := asObject(res[0]["deleted"])
	if asInt(deleted["episodes"], -1) != 1 || asInt(deleted["places"], -1) != 1 || asInt(deleted["promises"], -1) != 0 {
		t.Errorf("forget answered %v, want the episode and the place that name the player under either gamertag", deleted)
	}
	if lg.stored(t, conceptEpisode, "e-old-"+n) != 0 || lg.stored(t, conceptPlace, "pl-new-"+n) != 0 || lg.stored(t, conceptPromise, "pr-else-"+n) != 1 {
		t.Errorf("the rows were not deleted as they should be")
	}
}

// Every character a pattern, JSON text or a SQL string treats specially stands for itself
// in these texts too.
func TestLiveForgetPlayerMatchesAGamertagOnlyAsItselfInAMemory(t *testing.T) {
	lg := newLiveGames(t)
	for i, c := range []struct {
		name, gamertag string
		hits, decoys   []string
	}{
		{"a leading dot and an underscore", ".Kid_Tag", []string{"follow .Kid_Tag now", ".KID_TAG!", "go\n.Kid_Tag"}, []string{"follow aKidxTag", "follow .KidxTag", "follow Kid_Tag", "follow Mr.Kid_Tag"}},
		{"the pattern characters", `A.+*?[](){}|^$B`, []string{`see A.+*?[](){}|^$B now`}, []string{"see AxB now", "see AB now"}},
		{"a double quote and a backslash", `Say "Hi" C:\Kid`, []string{`loop: Say "Hi" C:\Kid again`}, []string{`loop: Say Hi C:\Kid again`, `loop: Say \"Hi\" C:\\Kid again`}},
		{"letters outside ASCII", "Zo\u00eb\u2013\u00c9mile", []string{"greet Zo\u00eb\u2013\u00c9mile now"}, []string{"greet Zoe-Emile now"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			n := runID()
			owner := fmt.Sprintf("user-games-forget-named-meta-%d-%s", i, n)
			ctx := asOwner(owner)
			player := "p-meta-" + n
			lg.must(t, ctx, "mutation", "upsertGamesPlayer", map[string]any{"playerId": player, "gamertag": c.gamertag, "displayName": "x", "consented": true})
			var hitIDs, decoyIDs []string
			// the text goes into every place a name is read: an episode's summary, a preference's
			// subject, a promise's text, a place's name
			write := func(kind string, j int, text string) []string {
				ids := []string{fmt.Sprintf("e-%s-%d-%s", kind, j, n), fmt.Sprintf("pf-%s-%d-%s", kind, j, n), fmt.Sprintf("pr-%s-%d-%s", kind, j, n), fmt.Sprintf("pl-%s-%d-%s", kind, j, n)}
				lg.writeEpisode(t, owner, ids[0], "", text, nil)
				lg.writePreference(t, owner, ids[1], "p-other-"+n, text)
				lg.writePromise(t, owner, ids[2], "p-other-"+n, text)
				lg.writePlace(t, owner, ids[3], text, nil)
				return ids
			}
			for j, text := range c.hits {
				hitIDs = append(hitIDs, write("hit", j, text)...)
			}
			for j, text := range c.decoys {
				decoyIDs = append(decoyIDs, write("decoy", j, text)...)
			}
			res, err := lg.call(t, ctx, "builtin", "gamesForgetPlayer", map[string]any{"playerId": player})
			if err != nil {
				t.Fatalf("forget a player whose gamertag is %q: %v", c.gamertag, err)
			}
			if got := asInt(asObject(res[0]["deleted"])["places"], -1); got != len(c.hits) {
				t.Errorf("forget deleted %d places, want %d", got, len(c.hits))
			}
			concept := func(id string) string {
				switch {
				case strings.HasPrefix(id, "e-"):
					return conceptEpisode
				case strings.HasPrefix(id, "pf-"):
					return conceptPreference
				case strings.HasPrefix(id, "pr-"):
					return conceptPromise
				}
				return conceptPlace
			}
			for _, id := range hitIDs {
				if lg.stored(t, concept(id), id) != 0 {
					t.Errorf("%s, which names the gamertag %q, survived", id, c.gamertag)
				}
			}
			for _, id := range decoyIDs {
				if lg.stored(t, concept(id), id) != 1 {
					t.Errorf("%s was deleted, though it does not contain the gamertag %q", id, c.gamertag)
				}
			}
		})
	}
}

// A row id is one owner's by construction, but the owned tier's write guard waves the cluster's
// owner through onto an id somebody else holds, so an id CAN carry versions of two owners (the
// takeover the pack refuses to write, made here the way an earlier build allowed it). A forget
// deletes its caller's own versions, and the words that decide are the caller's own: a version of
// another owner's that names the gamertag is no reason to delete mine, and mine that names it is
// no reason to delete theirs. The same for each of the four kinds.
func TestLiveForgetPlayerNeverDeletesAMemoryVersionAnotherOwnerWrote(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	aName, bName := "user-games-forget-memtwo-a-"+n, "user-games-forget-memtwo-b-"+n
	a, b := asOwner(aName), clusterOwnerOf(bName)
	tag := "MemTwo" + n
	player := "p-memtwo-" + n
	lg.must(t, a, "mutation", "upsertGamesPlayer", map[string]any{"playerId": player, "gamertag": tag, "displayName": "x", "consented": true})
	now := time.Now().UTC()
	writers := []struct {
		concept string
		write   func(ctx context.Context, id, text string)
	}{
		{conceptEpisode, func(ctx context.Context, id, text string) {
			lg.must(t, ctx, "builtin", "gamesRecordEpisodes", map[string]any{"episodes": episodesArg(episode(id, "wren", "", text, now, 4))})
		}},
		{conceptPreference, func(ctx context.Context, id, text string) {
			lg.must(t, ctx, "mutation", "upsertGamesPreference", map[string]any{"preferenceId": id, "playerId": "p-else-" + n, "subject": text, "valence": 0.5, "confidence": 0.6})
		}},
		{conceptPromise, func(ctx context.Context, id, text string) {
			lg.must(t, ctx, "mutation", "upsertGamesPromise", map[string]any{"promiseId": id, "characterId": "wren", "playerId": "p-else-" + n, "text": text, "status": "open", "madeAt": rfc3339(now)})
		}},
		{conceptPlace, func(ctx context.Context, id, text string) {
			lg.must(t, ctx, "mutation", "upsertGamesPlace", map[string]any{"placeId": id, "worldId": "meadow", "name": text, "kind": "base", "position": map[string]any{"x": 1, "y": 2, "z": 3}, "discoveredBy": "wren", "at": rfc3339(now)})
		}},
	}
	owned := func(concept, id, owner string) int {
		var c int
		if err := lg.db.DB.QueryRow(`SELECT count(*) FROM "MemoryNodes" WHERE concept = $1 AND id = $2 AND payload->>'ownerUserId' = $3`,
			concept, canonical(concept, id), ownerKey(owner)).Scan(&c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	type pair struct{ concept, x, y string }
	var pairs []pair
	for _, w := range writers {
		kind := strings.TrimPrefix(w.concept, "v1:games:")
		x, y := "mt-x-"+kind+"-"+n, "mt-y-"+kind+"-"+n
		// X: A's version names the gamertag, B's later version of the same id does not.
		// Y: A's version names nobody, B's later version names the gamertag.
		w.write(a, x, "met "+tag)
		time.Sleep(5 * time.Millisecond)
		w.write(b, x, "went home")
		w.write(a, y, "went home")
		time.Sleep(5 * time.Millisecond)
		w.write(b, y, "met "+tag)
		if owned(w.concept, x, aName) != 1 || owned(w.concept, x, bName) != 1 || owned(w.concept, y, aName) != 1 || owned(w.concept, y, bName) != 1 {
			t.Fatalf("the setup: each %s id should carry one version of each owner", kind)
		}
		pairs = append(pairs, pair{w.concept, x, y})
	}

	res := lg.must(t, a, "builtin", "gamesForgetPlayer", map[string]any{"playerId": player})
	deleted := asObject(res[0]["deleted"])
	for _, key := range []string{"episodes", "preferences", "promises", "places"} {
		if got := asInt(deleted[key], -1); got != 1 {
			t.Errorf("forget deleted %d %s, want the one whose version of A's names the player (%v)", got, key, deleted)
		}
	}
	for _, p := range pairs {
		if owned(p.concept, p.x, aName) != 0 || owned(p.concept, p.x, bName) != 1 {
			t.Errorf("%s: after the forget X has %d version(s) of A's and %d of B's, want 0 and 1: A's own version goes and B's stays", p.concept, owned(p.concept, p.x, aName), owned(p.concept, p.x, bName))
		}
		if owned(p.concept, p.y, aName) != 1 || owned(p.concept, p.y, bName) != 1 {
			t.Errorf("%s: after the forget Y has %d version(s) of A's and %d of B's, want both: nothing of A's names the player", p.concept, owned(p.concept, p.y, aName), owned(p.concept, p.y, bName))
		}
	}
}

// A place records the character that found it (discoveredBy), which is an id like a character
// anywhere else, and the runtime writes only pal ids there. A player tagged like a pal (Wren, and
// the pal wren every place here is found by unless a test says otherwise) must not take every
// place the pal found along: the finder is not read, in any spelling. What names the player in the
// place's own name still goes.
func TestLiveForgetPlayerDoesNotReadWhoFoundAPlace(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	owner, other := "user-games-forget-found-"+n, "user-games-forget-found-other-"+n
	ctx := asOwner(owner)
	player := "p-found-" + n
	lg.must(t, ctx, "mutation", "upsertGamesPlayer", map[string]any{"playerId": player, "gamertag": "Wren", "displayName": "Wren", "consented": true})
	id := func(kind string, i int) string { return fmt.Sprintf("%s-%d-%s", kind, i, n) }

	var hits, keeps, theirs memoryRows
	// the pal wren found these (the writer's default), and their names name nobody
	for i, name := range []string{"the old farm", "a cave", "north hill"} {
		lg.writePlace(t, owner, id("pl-no", i), name, nil)
		keeps.places = append(keeps.places, id("pl-no", i))
	}
	// the same pal in other spellings, a pal with a longer name, and no finder at all
	for i, finder := range []string{"Wren", "WREN", "the Wren", "Wren!", "wrench", "Wrens"} {
		lg.writePlace(t, owner, id("pl-no-by", i), "a place nobody named", func(a map[string]any) { a["discoveredBy"] = finder })
		keeps.places = append(keeps.places, id("pl-no-by", i))
	}
	lg.writePlace(t, owner, id("pl-no-by", 6), "a place nobody named", func(a map[string]any) { delete(a, "discoveredBy") })
	keeps.places = append(keeps.places, id("pl-no-by", 6))
	// what names the player in its own name goes, whoever found it
	lg.writePlace(t, owner, id("pl-yes", 0), "Wren's farm", nil)
	lg.writePlace(t, owner, id("pl-yes", 1), "wren HQ", func(a map[string]any) { a["discoveredBy"] = "somebody else" })
	hits.places = []string{id("pl-yes", 0), id("pl-yes", 1)}
	// another owner's, with the same finder and the same name
	lg.writePlace(t, other, id("pl-other", 0), "the old farm", nil)
	lg.writePlace(t, other, id("pl-other", 1), "Wren's farm", nil)
	theirs.places = []string{id("pl-other", 0), id("pl-other", 1)}

	res := lg.must(t, ctx, "builtin", "gamesForgetPlayer", map[string]any{"playerId": player})
	if got := asInt(asObject(res[0]["deleted"])["places"], -1); got != len(hits.places) {
		t.Errorf("forget deleted %d places, want the %d that name the player: not the ones the pal wren found (%v)", got, len(hits.places), res[0]["deleted"])
	}
	for _, r := range hits.all() {
		if lg.stored(t, r.concept, r.id) != 0 {
			t.Errorf("%s %s names the player and survived", r.concept, r.id)
		}
	}
	for what, group := range map[string]memoryRows{"found by a pal called like the player": keeps, "another owner's": theirs} {
		for _, r := range group.all() {
			if v := lg.stored(t, r.concept, r.id); v != 1 {
				t.Errorf("%s: %s %s has %d version(s) after the forget, want 1", what, r.concept, r.id, v)
			}
		}
	}
}

// A preference keeps the ids of the episodes it was inferred from (evidence), and the runtime's
// ids are a short prefix, a dash and a hash (ses-<32 hex>). A player tagged Ses, in any case, is
// a whole name in every one of them and would take every preference that has evidence: the
// evidence is not read. What names the player in the preference's subject still goes.
func TestLiveForgetPlayerDoesNotReadWhatAPreferenceWasInferredFrom(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	owner, other := "user-games-forget-evidence-"+n, "user-games-forget-evidence-other-"+n
	ctx := asOwner(owner)
	player, jo := "p-evidence-"+n, "p-jo-"+n
	lg.must(t, ctx, "mutation", "upsertGamesPlayer", map[string]any{"playerId": player, "gamertag": "Ses", "displayName": "Ses", "consented": true})
	id := func(kind string, i int) string { return fmt.Sprintf("%s-%d-%s", kind, i, n) }
	session := func(i int) string { return fmt.Sprintf("ses-%032x", i+1) } // the shape of the runtime's ids

	var hits, keeps, theirs memoryRows
	for i, evidence := range [][]string{
		{session(0)}, {session(1), session(2)}, {strings.ToUpper(session(3))}, {"Ses"}, {"the ses", "e-5"}, {"e-5", "Ses-7", "tag"}, {},
	} {
		lg.writePreferenceWith(t, owner, id("pf-no", i), jo, "something nobody named", evidence)
		keeps.preferences = append(keeps.preferences, id("pf-no", i))
	}
	lg.writePreferenceWith(t, owner, id("pf-no", 7), jo, "something nobody named", nil)
	keeps.preferences = append(keeps.preferences, id("pf-no", 7))
	// what names the player in its subject goes, and so does a preference of the player's
	lg.writePreferenceWith(t, owner, id("pf-yes", 0), jo, "Ses's pickaxe", []string{session(4)})
	lg.writePreferenceWith(t, owner, id("pf-yes", 1), jo, "playing with SES", nil)
	lg.writePreferenceWith(t, owner, id("pf-yes", 2), player, "diamonds", []string{session(5)})
	hits.preferences = []string{id("pf-yes", 0), id("pf-yes", 1), id("pf-yes", 2)}
	// another owner's, with the same evidence and the same subject
	lg.writePreferenceWith(t, other, id("pf-other", 0), jo, "something nobody named", []string{session(0)})
	lg.writePreferenceWith(t, other, id("pf-other", 1), jo, "Ses's pickaxe", []string{session(4)})
	theirs.preferences = []string{id("pf-other", 0), id("pf-other", 1)}

	res := lg.must(t, ctx, "builtin", "gamesForgetPlayer", map[string]any{"playerId": player})
	if got := asInt(asObject(res[0]["deleted"])["preferences"], -1); got != len(hits.preferences) {
		t.Errorf("forget deleted %d preferences, want the %d that name the player or are about them: not the ones inferred from ids that contain the name (%v)", got, len(hits.preferences), res[0]["deleted"])
	}
	for _, r := range hits.all() {
		if lg.stored(t, r.concept, r.id) != 0 {
			t.Errorf("%s %s names the player and survived", r.concept, r.id)
		}
	}
	for what, group := range map[string]memoryRows{"inferred from ids called like the player": keeps, "another owner's": theirs} {
		for _, r := range group.all() {
			if v := lg.stored(t, r.concept, r.id); v != 1 {
				t.Errorf("%s: %s %s has %d version(s) after the forget, want 1", what, r.concept, r.id, v)
			}
		}
	}
}

// A pal, a world, a kind, a finder and the ids a preference was inferred from are identifiers and
// enums, and are not read: a gamertag that equals or begins one would delete every row of that pal
// or that world, or every row with such an id. Rows that really name the player go in the same
// forget, which is what makes the control mean something.
func TestLiveForgetPlayerKeepsTheRowsOfAPalAndAWorldCalledLikeThePlayer(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	owner := "user-games-forget-pal-" + n
	ctx := asOwner(owner)
	player, jo := "p-pal-"+n, "p-jo-"+n
	lg.must(t, ctx, "mutation", "upsertGamesPlayer", map[string]any{"playerId": player, "gamertag": "Tag", "displayName": "Tag", "consented": true})
	id := func(kind string, i int) string { return fmt.Sprintf("%s-%d-%s", kind, i, n) }

	// the pal Tag remembers things, and promised things, in the world Tag, and found a place there
	var keeps, hits memoryRows
	lg.writeEpisode(t, owner, id("e-no", 0), jo, "nobody is here", func(e map[string]any) { e["characterId"] = "Tag" })
	lg.writeEpisode(t, owner, id("e-no", 1), jo, "nobody is here", func(e map[string]any) { e["worldId"] = "Tag" })
	lg.writeEpisode(t, owner, id("e-no", 2), jo, "nobody is here", func(e map[string]any) { e["characterId"], e["worldId"] = "Tag", "Tag" })
	keeps.episodes = []string{id("e-no", 0), id("e-no", 1), id("e-no", 2)}
	lg.must(t, ctx, "mutation", "upsertGamesPromise", map[string]any{
		"promiseId": id("pr-no", 0), "characterId": "Tag", "playerId": jo, "text": "nobody here", "status": "open", "madeAt": rfc3339(time.Now().UTC()),
	})
	keeps.promises = []string{id("pr-no", 0)}
	lg.writePlace(t, owner, id("pl-no", 0), "nowhere", func(a map[string]any) { a["worldId"], a["kind"] = "Tag", "Tag" })
	lg.writePlace(t, owner, id("pl-no", 1), "a cave", func(a map[string]any) { a["worldId"], a["discoveredBy"] = "Tag", "Tag" })
	keeps.places = []string{id("pl-no", 0), id("pl-no", 1)}
	lg.writePreferenceWith(t, owner, id("pf-no", 0), jo, "nothing at all", []string{"Tag", "tag-0123"})
	keeps.preferences = []string{id("pf-no", 0)}

	// and what names the player, in the same forget
	lg.writeEpisode(t, owner, id("e-yes", 0), jo, "Tag waved", func(e map[string]any) { e["characterId"], e["worldId"] = "Tag", "Tag" })
	lg.writePromise(t, owner, id("pr-yes", 0), jo, "meet Tag")
	lg.writePreferenceWith(t, owner, id("pf-yes", 0), jo, "Tag's pickaxe", []string{"Tag"})
	lg.writePlace(t, owner, id("pl-yes", 0), "Tag's farm", func(a map[string]any) { a["worldId"], a["discoveredBy"] = "Tag", "Tag" })
	hits.episodes, hits.promises, hits.preferences, hits.places = []string{id("e-yes", 0)}, []string{id("pr-yes", 0)}, []string{id("pf-yes", 0)}, []string{id("pl-yes", 0)}

	res := lg.must(t, ctx, "builtin", "gamesForgetPlayer", map[string]any{"playerId": player})
	deleted := asObject(res[0]["deleted"])
	for _, key := range []string{"episodes", "preferences", "promises", "places"} {
		if got := asInt(deleted[key], -1); got != 1 {
			t.Errorf("forget deleted %d %s, want the one that names the player (%v)", got, key, deleted)
		}
	}
	for _, r := range hits.all() {
		if v := lg.stored(t, r.concept, r.id); v != 0 {
			t.Errorf("%s %s names the player and survived", r.concept, r.id)
		}
	}
	for _, r := range keeps.all() {
		if v := lg.stored(t, r.concept, r.id); v != 1 {
			t.Errorf("%s %s has an identifier called like the player and %d version(s) after the forget, want 1", r.concept, r.id, v)
		}
	}
}
