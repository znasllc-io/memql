package gamespack

import (
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// owner_scope_db_test.go -- every query of the pack that pages answers the CALLER'S rows.
//
// The owned tier's read arm for the cluster's owner returns every owner's rows, and a query
// that pages returns its first page of them. A cluster owner's page was therefore shared with
// everyone else's rows, and its own could fall off it: with more than twenty skills of one
// goal across the cluster, a cluster owner's comparison never found its own. So each query
// that pages names the owner in its filter (`row.ownerUserId == actor.userId`, which needs
// `@actor`) as well as leaving it to the tier. The two queries that answer one row by id do
// not page and do not: their callers compare the owner themselves (moveLadder, forgetPlayer).

// pagedQueries are the queries of queries.memql that carry a `paginate`: the text of each
// block, by name.
func pagedQueries(t *testing.T) map[string]string {
	t.Helper()
	raw, err := fs.ReadFile(Tree(), "queries.memql")
	if err != nil {
		t.Fatal(err)
	}
	header := regexp.MustCompile(`(?m)^query\s+\w+\s+(\w+)\s*\{`)
	out := map[string]string{}
	text := string(raw)
	for _, loc := range header.FindAllStringSubmatchIndex(text, -1) {
		name := text[loc[2]:loc[3]]
		// the preamble: the annotation lines above, up to the blank line before them
		start := strings.LastIndex(text[:loc[0]], "\n\n") + 1
		// the body: to the closing brace in column 0
		end := loc[1] + strings.Index(text[loc[1]:], "\n}\n") + 3
		block := text[start:end]
		if strings.Contains(block, "paginate") {
			out[name] = block
		}
	}
	if len(out) == 0 {
		t.Fatal("found no query that pages, so the scan asserts nothing")
	}
	return out
}

// The audit, pinned: no query that pages is added without the owner in its filter.
func TestEveryPagedQueryNamesTheCallersOwner(t *testing.T) {
	paged := pagedQueries(t)
	want := []string{
		"gamesLearningTraces", "gamesOpenPromises", "gamesPlacesInWorld", "gamesPlayers", "gamesPreferencesForPlayer",
		"gamesPromisesForPlayer", "gamesSessionEpisodes", "gamesSkills", "gamesSkillsAwaitingApproval", "gamesSkillsForSignature",
		"gamesSkillsToSweep", "gamesTrustedSkills", "gamesEpisodesForPlayer",
	}
	var got []string
	for name, block := range paged {
		got = append(got, name)
		if !regexp.MustCompile(`(?m)^@actor\s*$`).MatchString(block) {
			t.Errorf("%s pages and does not declare @actor, which reading actor.userId needs", name)
		}
		if !strings.Contains(block, "row.ownerUserId == actor.userId") {
			t.Errorf("%s pages and its filter does not name the caller's owner: a cluster owner's page is shared with everyone's rows", name)
		}
	}
	sort.Strings(got)
	sort.Strings(want)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("the queries that page are %v, want %v: a new one must be added to this list with its owner in the filter", got, want)
	}
}

// setSkillState writes a skill's ladder state as the pack does, for a test that needs a skill
// awaiting approval or trusted.
func (lg *liveGames) setSkillState(t *testing.T, owner, skillID, rung, state, promotion string) {
	t.Helper()
	args := map[string]any{
		"skillId": skillID, "versionAt": rfc3339(time.Now().Add(time.Minute)), "rung": rung, "state": state,
		"shadowMatches": 5, "canaryMatches": 0, "distinctBindings": map[string]any{}, "failures": 0,
		"promotionApprovalId": promotion, "lastReason": "set by the test", "lastReplayAt": rfc3339(time.Now()),
	}
	if promotion != "" {
		args["promotionProposedAt"] = rfc3339(time.Now())
	}
	if err := lg.provider.store.recordLadder(asOwner(owner), args); err != nil {
		t.Fatal(err)
	}
}

// Two owners, one of them the cluster's, hold rows of every kind that agree on everything but
// the owner: the same player id, character, world and goal. Every query that pages answers
// each of them their own rows and no one else's, the cluster's owner included, whom the tier
// would let read all of them.
func TestLiveEveryPagedQueryAnswersTheCallersRowsOnly(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	aName, cName := "user-games-scope-a-"+n, "user-games-scope-c-"+n
	owners := map[string]struct {
		name string
		key  string
	}{"A": {aName, "a"}, "the cluster's owner": {cName, "c"}}
	shared := "p-shared-" + n // the player id both owners' rows are about
	goal := "scope the area " + n
	sig := GoalSignature(goal, nil)
	now := time.Now().UTC()
	t.Cleanup(func() {
		for _, who := range owners {
			if _, err := lg.db.DB.Exec(`DELETE FROM "MemoryNodes" WHERE concept = $1 AND payload->>'ownerUserId' = $2`, conceptSkill, ownerKey(who.name)); err != nil {
				t.Logf("cleanup: %v", err)
			}
		}
	})
	for _, who := range owners {
		ctx := asOwner(who.name)
		if who.name == cName {
			ctx = clusterOwnerOf(cName)
		}
		k := who.key
		lg.must(t, ctx, "mutation", "upsertGamesPlayer", map[string]any{"playerId": "p-" + k + "-" + n, "gamertag": "Tag" + k + n, "displayName": "Scope " + k, "consented": true})
		lg.writeEpisode(t, who.name, "e-"+k+"-"+n, shared, "scope memory "+k, nil)
		lg.writePreference(t, who.name, "pf-"+k+"-"+n, shared, "scope subject "+k)
		lg.writePromise(t, who.name, "pr-"+k+"-"+n, shared, "scope promise "+k)
		lg.writePlace(t, who.name, "pl-"+k+"-"+n, "scope place "+k, nil)
		lg.writeTrace(t, who.name, traceAbout{id: "t-" + k + "-" + n, goal: "scope trace " + k})
		plain := lg.plantSkill(t, who.name, skillSpec{id: "gs-plain-" + k + "-" + n, goal: goal})
		awaiting := lg.plantSkill(t, who.name, skillSpec{id: "gs-awaiting-" + k + "-" + n, goal: goal, steps: []TemplateStep{constStep("wait", "where", "here")}})
		lg.setSkillState(t, who.name, awaiting, "shadow", "awaiting_approval", promotionIDFor("digest"))
		trusted := lg.plantSkill(t, who.name, skillSpec{id: "gs-trusted-" + k + "-" + n, goal: goal, steps: []TemplateStep{constStep("trust", "where", "here")}})
		lg.setSkillState(t, who.name, trusted, "trusted", "trusted", "")
		_ = plain
	}

	traceSig := strings.Repeat("ab", 32) // the signature writeTrace gives every trace
	queries := []struct {
		name string
		args map[string]any
	}{
		{"gamesPlayers", nil},
		{"gamesPreferencesForPlayer", map[string]any{"playerId": shared}},
		{"gamesOpenPromises", map[string]any{"characterId": "wren", "playerId": shared}},
		{"gamesPromisesForPlayer", map[string]any{"playerId": shared}},
		{"gamesPlacesInWorld", map[string]any{"worldId": "meadow"}},
		{"gamesSessionEpisodes", map[string]any{"characterId": "wren", "since": now.Add(-time.Hour).Format("2006-01-02T15:04:05.000Z")}},
		{"gamesEpisodesForPlayer", map[string]any{"playerId": shared}},
		{"gamesSkills", nil},
		{"gamesSkillsAwaitingApproval", nil},
		{"gamesSkillsForSignature", map[string]any{"goalSignature": sig}},
		{"gamesTrustedSkills", nil},
		{"gamesLearningTraces", map[string]any{"goalSignature": traceSig}},
		{"gamesSkillsToSweep", nil},
	}
	if len(queries) != len(pagedQueries(t)) {
		t.Fatalf("this test reads %d queries and the pack has %d that page", len(queries), len(pagedQueries(t)))
	}
	for who, o := range owners {
		ctx := asOwner(o.name)
		if o.name == cName {
			ctx = clusterOwnerOf(cName)
		}
		for _, q := range queries {
			rows := lg.must(t, ctx, "query", q.name, q.args)
			own, foreign := 0, 0
			for _, r := range rows {
				if ownerKey(trimmed(r["ownerUserId"])) == ownerKey(o.name) {
					own++
				} else {
					foreign++
				}
			}
			// The shared test database holds the rows of every earlier run: the page can be
			// full of the owner's own, so what is asserted is that none of it is another's and
			// that the owner's own is there.
			if foreign != 0 || own == 0 {
				t.Errorf("%s: %s answered %d of the caller's rows and %d of other owners', want some of the first and none of the second", who, q.name, own, foreign)
			}
		}
	}
}

// The cluster's owner compares a plan with the shadow skills of a goal that more than twenty
// skills of other owners share the signature of. The query that lists them answers a page of
// twenty, newest first, and the cluster owner's own skill is the oldest: it fell off the page
// and was never compared. With the owner in the filter the page is the owner's own.
func TestLiveAClusterOwnersComparisonFindsItsOwnSkillAmongOtherOwnersOfTheGoal(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	aName, cName := "user-games-scope-pages-a-"+n, "user-games-scope-pages-c-"+n
	c := clusterOwnerOf(cName)
	goal := "scope the page " + n
	sig := GoalSignature(goal, nil)
	t.Cleanup(func() {
		for _, name := range []string{aName, cName} {
			if _, err := lg.db.DB.Exec(`DELETE FROM "MemoryNodes" WHERE concept = $1 AND payload->>'ownerUserId' = $2`, conceptSkill, ownerKey(name)); err != nil {
				t.Logf("cleanup: %v", err)
			}
		}
	})
	// the cluster owner's own skill first, so that it is the oldest
	mine := lg.plantSkill(t, cName, skillSpec{id: "gs-scope-mine-" + n, goal: goal})
	time.Sleep(20 * time.Millisecond)
	// A, another owner, holds twenty-five skills of the same goal, each of other actions, newer
	for i := 0; i < 25; i++ {
		lg.plantSkill(t, aName, skillSpec{id: fmt.Sprintf("gs-scope-a-%02d-%s", i, n), goal: goal, steps: []TemplateStep{constStep(fmt.Sprintf("act%02d", i), "where", "here")}})
	}
	if rows := lg.must(t, c, "query", "gamesSkillById", map[string]any{"skillId": "gs-scope-a-00-" + n}); len(rows) != 1 {
		t.Fatalf("control: the cluster's owner reads %d of A's skill, want 1 -- without it the page below is not shared", len(rows))
	}

	got := lg.must(t, c, "builtin", "gamesShadowCompare", map[string]any{
		"goalSignature": sig,
		"plan":          []any{map[string]any{"action": "look", "args": map[string]any{"where": "around"}}},
	})[0]
	if asInt(got["compared"], -1) != 1 || asInt(got["matched"], -1) != 1 {
		t.Fatalf("the cluster owner's comparison answered %v, want its own skill found among the %d of other owners' and compared", got, 25)
	}
	if rows := lg.must(t, c, "query", "gamesSkillsForSignature", map[string]any{"goalSignature": sig}); len(rows) != 1 || bare(trimmed(rows[0]["id"])) != mine {
		t.Errorf("the cluster owner's skills of the goal are %v, want only its own %s", rows, mine)
	}
}
