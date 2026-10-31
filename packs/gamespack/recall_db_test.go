package gamespack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// byRank orders recall rows by their rank, which is the contract; the order
// rows arrive in is the transport's.
func byRank(items []map[string]any) []map[string]any {
	out := append([]map[string]any(nil), items...)
	sort.SliceStable(out, func(i, j int) bool { return asInt(out[i]["rank"], 0) < asInt(out[j]["rank"], 0) })
	return out
}

// recordEpisodes writes and embeds; recall ranks by the contract formula and
// scopes to the character's own episodes plus episodes about the player.
func TestLiveRecallRanksAndScopes(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	owner := "user-games-recall-" + n
	ctx := asOwner(owner)
	now := time.Now().UTC().Truncate(time.Second)
	player := "p-sam-" + n

	lg.must(t, ctx, "mutation", "upsertGamesPlayer", map[string]any{"playerId": player, "gamertag": "Sam" + n, "displayName": "Sam", "consented": true})
	res := lg.must(t, ctx, "builtin", "gamesRecordEpisodes", map[string]any{"episodes": episodesArg(
		episode("e-diamonds-"+n, "wren", player, "Sam found diamonds in the deep cave", now.Add(-1*time.Hour), 8),
		episode("e-bread-"+n, "wren", player, "Sam ate some bread at the farm", now.Add(-200*time.Hour), 2),
		episode("e-otis-"+n, "otis", player, "Sam showed Otis the diamonds", now.Add(-2*time.Hour), 5),
		episode("e-other-"+n, "otis", "", "Otis watched the sunset alone", now.Add(-1*time.Hour), 9),
	)})
	if got := asInt(res[0]["written"], 0); got != 4 {
		t.Fatalf("written = %d, want 4: %v", got, res[0])
	}
	if got := asInt(res[0]["embedded"], 0); got != 4 {
		t.Fatalf("embedded = %d, want 4: %v", got, res[0])
	}

	items := lg.must(t, ctx, "builtin", "gamesRecall", map[string]any{
		"characterId": "wren", "playerId": player, "worldId": "meadow",
		"text": "diamonds cave", "asOf": rfc3339(now), "k": 8,
	})
	var episodes []map[string]any
	for _, it := range byRank(items) {
		if it["kind"] == "episode" {
			episodes = append(episodes, it)
		}
	}
	if len(episodes) != 3 {
		t.Fatalf("recall returned %d episodes, want wren's two plus otis's one about Sam (not otis's sunset): %v", len(episodes), episodes)
	}
	if episodes[0]["text"] != "Sam found diamonds in the deep cave" {
		t.Fatalf("the relevant, recent, important episode must rank first, got %v", episodes[0]["text"])
	}
	for i := 1; i < len(episodes); i++ {
		if asFloat(episodes[i-1]["score"], 0) < asFloat(episodes[i]["score"], 0) {
			t.Fatalf("episodes are not in score order: %v", episodes)
		}
	}
	// The statement and the formula agree.
	for _, ep := range episodes {
		row := asObject(ep["row"])
		age := now.Sub(timeOf(row["at"])).Hours()
		want := recallScore(asFloat(ep["relevance"], 0), age, asFloat(row["importance"], 0), defaultRecallWeights())
		if math.Abs(asFloat(ep["score"], 0)-want) > 1e-6 {
			t.Fatalf("score %v disagrees with recallScore %v for %v", ep["score"], want, ep["text"])
		}
		if ep["relevanceAvailable"] != true {
			t.Fatalf("relevance was available for %v", ep["text"])
		}
	}
}

// With no vector table the answer is still ranked, by recency and importance.
func TestLiveRecallWithoutAnEmbedderStillAnswers(t *testing.T) {
	lg := newLiveGames(t)
	lg.provider.space = testSpace{db: lg.db.DB, tableName: ""}
	n := runID()
	ctx := asOwner("user-games-norel-" + n)
	now := time.Now().UTC().Truncate(time.Second)
	lg.must(t, ctx, "builtin", "gamesRecordEpisodes", map[string]any{"episodes": episodesArg(
		episode("e-old-"+n, "wren", "", "an old memory", now.Add(-300*time.Hour), 9),
		episode("e-new-"+n, "wren", "", "a new memory", now.Add(-1*time.Hour), 3),
	)})
	items := lg.must(t, ctx, "builtin", "gamesRecall", map[string]any{
		"characterId": "wren", "worldId": "meadow", "text": "anything", "asOf": rfc3339(now),
	})
	items = byRank(items)
	if len(items) < 2 || items[0]["text"] != "a new memory" || items[0]["relevanceAvailable"] != false {
		t.Fatalf("without an embedder recall must rank by recency and importance and say relevance was unavailable: %v", items)
	}
}

// The embedder is bound but the model is away: recall still answers, ranked
// by recency and importance, and says relevance was unavailable. The same
// episodes rank the other way round while the model is there.
func TestLiveRecallWhileTheModelIsAwayStillAnswers(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	ctx := asOwner("user-games-away-" + n)
	now := time.Now().UTC().Truncate(time.Second)
	lg.must(t, ctx, "builtin", "gamesRecordEpisodes", map[string]any{"episodes": episodesArg(
		episode("e-cave-"+n, "wren", "", "wren explored a cave", now.Add(-200*time.Hour), 9),
		episode("e-farm-"+n, "wren", "", "wren fed the chickens", now.Add(-1*time.Hour), 3),
	)})
	recallArgs := map[string]any{"characterId": "wren", "worldId": "meadow", "text": "cave", "asOf": rfc3339(now)}

	there := byRank(lg.must(t, ctx, "builtin", "gamesRecall", recallArgs))
	if len(there) < 2 || there[0]["id"] != "e-cave-"+n || there[0]["relevanceAvailable"] != true {
		t.Fatalf("with the model there, the relevant episode must rank first: %v", there)
	}

	lg.provider.space = failingSpace{testSpace{db: lg.db.DB, tableName: lg.table}}
	away := byRank(lg.must(t, ctx, "builtin", "gamesRecall", recallArgs))
	if len(away) < 2 || away[0]["id"] != "e-farm-"+n {
		t.Fatalf("with the model away, recency and importance must decide: %v", away)
	}
	for _, it := range away {
		if it["kind"] == "episode" && (it["relevanceAvailable"] != false || asFloat(it["relevance"], -1) != 0) {
			t.Fatalf("with the model away every episode must say relevance was unavailable: %v", it)
		}
	}
}

func TestLiveEpisodesAreTheOwnersAlone(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	a, b := asOwner("user-games-a-"+n), asOwner("user-games-b-"+n)
	lg.must(t, a, "builtin", "gamesRecordEpisodes", map[string]any{"episodes": episodesArg(episode("e-a-"+n, "wren", "", "a secret", time.Now().UTC(), 5))})
	countEpisodes := func(items []map[string]any) int {
		n := 0
		for _, it := range items {
			if it["kind"] == "episode" {
				n++
			}
		}
		return n
	}
	recallArgs := map[string]any{"characterId": "wren", "worldId": "meadow", "text": "a secret"}
	if got := countEpisodes(lg.must(t, a, "builtin", "gamesRecall", recallArgs)); got != 1 {
		t.Fatalf("owner A recalled %d of their own episodes, want 1 -- without this the next check proves nothing", got)
	}
	if got := countEpisodes(lg.must(t, b, "builtin", "gamesRecall", recallArgs)); got != 0 {
		t.Fatalf("owner B recalled %d of owner A's episodes", got)
	}
}

// ---- the statement holds for every episode the write path admits ----

// episodeRows keeps the episode rows of a recall answer, in rank order.
func episodeRows(items []map[string]any) []map[string]any {
	var out []map[string]any
	for _, it := range byRank(items) {
		if it["kind"] == "episode" {
			out = append(out, it)
		}
	}
	return out
}

// A half-life of zero or less turns the recency term off, as recallScore says;
// it is not a division by zero. Every episode then has recency 0 and ranks on
// what is left, here importance alone, so the old, important episode is first.
func TestLiveRecallWithANonPositiveHalfLifeRanksWithoutRecency(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	ctx := asOwner("user-games-halflife-" + n)
	now := time.Now().UTC().Truncate(time.Second)
	lg.must(t, ctx, "builtin", "gamesRecordEpisodes", map[string]any{"episodes": episodesArg(
		episode("e-old-"+n, "wren", "", "wren mapped the mountain", now.Add(-300*time.Hour), 9),
		episode("e-new-"+n, "wren", "", "wren fed the chickens", now.Add(-1*time.Hour), 3),
	)})
	for _, halfLife := range []float64{0, -72} {
		t.Run(strconv.FormatFloat(halfLife, 'f', -1, 64), func(t *testing.T) {
			items, err := lg.call(t, ctx, "builtin", "gamesRecall", map[string]any{
				"characterId": "wren", "worldId": "meadow", "text": "mountain", "asOf": rfc3339(now),
				"halfLifeHours": halfLife, "wRelevance": 0,
			})
			if err != nil {
				t.Fatalf("a half-life of %v must turn recency off, not fail the recall: %v", halfLife, err)
			}
			episodes := episodeRows(items)
			if len(episodes) != 2 || episodes[0]["id"] != "e-old-"+n {
				t.Fatalf("with recency off the important episode must rank first: %v", episodes)
			}
			w := recallWeights{relevance: 0, recency: 0.3, importance: 0.2, halfLifeHours: halfLife}
			for _, ep := range episodes {
				if asFloat(ep["recency"], -1) != 0 {
					t.Errorf("recency = %v for %v, want 0 while the half-life is %v", ep["recency"], ep["text"], halfLife)
				}
				age := now.Sub(timeOf(asObject(ep["row"])["at"])).Hours()
				want := recallScore(asFloat(ep["relevance"], 0), age, asFloat(asObject(ep["row"])["importance"], 0), w)
				if math.Abs(asFloat(ep["score"], 0)-want) > 1e-6 {
					t.Errorf("score %v disagrees with recallScore %v for %v", ep["score"], want, ep["text"])
				}
			}
		})
	}
}

// An episode's time is the game's, and a game's clock can be anywhere: a runtime
// whose clock was wrong when it recorded, a world with a calendar of its own, or
// a half-life so short that three months is two thousand of them. An episode
// that far from asOf has no recency to speak of. It is still ranked, and it must
// not fail the owner's whole recall for as long as the episode lives.
func TestLiveRecallRanksAnEpisodeFromLongAgoInsteadOfFailing(t *testing.T) {
	lg := newLiveGames(t)
	now := time.Now().UTC().Truncate(time.Second)
	for _, tc := range []struct {
		name     string
		age      time.Duration
		halfLife float64 // 0: the default
	}{
		{"seventeen years old at the default half-life", 17 * 365 * 24 * time.Hour, 0},
		{"ninety days old at a half-life of one hour", 90 * 24 * time.Hour, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := runID()
			ctx := asOwner("user-games-longago-" + n)
			lg.must(t, ctx, "builtin", "gamesRecordEpisodes", map[string]any{"episodes": episodesArg(
				episode("e-ancient-"+n, "wren", "", "wren drew the first map", now.Add(-tc.age), 9),
				episode("e-recent-"+n, "wren", "", "wren fed the chickens", now.Add(-1*time.Hour), 3),
			)})
			args := map[string]any{"characterId": "wren", "worldId": "meadow", "text": "map", "asOf": rfc3339(now)}
			w := defaultRecallWeights()
			if tc.halfLife != 0 {
				args["halfLifeHours"] = tc.halfLife
				w.halfLifeHours = tc.halfLife
			}
			items, err := lg.call(t, ctx, "builtin", "gamesRecall", args)
			if err != nil {
				t.Fatalf("an episode %s must be ranked, not fail the recall: %v", tc.name, err)
			}
			episodes := episodeRows(items)
			if len(episodes) != 2 {
				t.Fatalf("recall returned %d episodes, want both: %v", len(episodes), episodes)
			}
			sawAncient := false
			for _, ep := range episodes {
				row := asObject(ep["row"])
				age := now.Sub(timeOf(row["at"])).Hours()
				want := recallScore(asFloat(ep["relevance"], 0), age, asFloat(row["importance"], 0), w)
				if math.Abs(asFloat(ep["score"], 0)-want) > 1e-6 {
					t.Errorf("score %v disagrees with recallScore %v for %v", ep["score"], want, ep["text"])
				}
				if ep["id"] == "e-ancient-"+n {
					sawAncient = true
					if asFloat(ep["recency"], 1) > 1e-9 {
						t.Errorf("an episode %s has recency %v, want none to speak of", tc.name, ep["recency"])
					}
				}
			}
			if !sawAncient {
				t.Errorf("the episode %s is not in the answer: %v", tc.name, episodes)
			}
		})
	}
}

// The write path admits the years 0000 to 9999 and PostgreSQL has no year 0000:
// an episode at 0000-01-01 is stored, and a statement that casts every `at`
// would fail for the owner who holds one until it expires, naming nothing the
// owner can act on. Its recency is 0, and it is ranked with the rest.
func TestLiveRecallRanksAnEpisodeWhoseTimeTheDatabaseCannotReadInsteadOfFailing(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	ctx := asOwner("user-games-year0-" + n)
	now := time.Now().UTC().Truncate(time.Second)
	res := lg.must(t, ctx, "builtin", "gamesRecordEpisodes", map[string]any{"episodes": episodesArg(
		episode("e-year0-"+n, "wren", "", "wren remembered the very first day", time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC), 9),
		episode("e-recent-"+n, "wren", "", "wren fed the chickens", now.Add(-1*time.Hour), 3),
	)})
	if asInt(res[0]["written"], 0) != 2 {
		t.Fatalf("the write path must admit year 0000 for this test to mean anything: %v", res[0])
	}
	items, err := lg.call(t, ctx, "builtin", "gamesRecall", map[string]any{
		"characterId": "wren", "worldId": "meadow", "text": "first day", "asOf": rfc3339(now),
	})
	if err != nil {
		t.Fatalf("an episode whose time cannot be read must be ranked, not fail the recall: %v", err)
	}
	episodes := episodeRows(items)
	if len(episodes) != 2 {
		t.Fatalf("recall returned %d episodes, want both: %v", len(episodes), episodes)
	}
	noRecency := defaultRecallWeights()
	noRecency.halfLifeHours = 0
	checked := false
	for _, ep := range episodes {
		if ep["id"] != "e-year0-"+n {
			continue
		}
		checked = true
		if asFloat(ep["recency"], -1) != 0 {
			t.Errorf("recency = %v for an episode with no readable time, want 0", ep["recency"])
		}
		want := recallScore(asFloat(ep["relevance"], 0), 0, asFloat(asObject(ep["row"])["importance"], 0), noRecency)
		if math.Abs(asFloat(ep["score"], 0)-want) > 1e-6 {
			t.Errorf("score %v, want %v: relevance and importance, no recency", ep["score"], want)
		}
	}
	if !checked {
		t.Errorf("the episode with no readable time is not in the answer: %v", episodes)
	}
}

// ---- what recall leaves in, and what follows the episodes ----

// An episode recorded while the model was away has no vector until
// gamesEmbedPending gives it one, and a recall that runs before then must still
// return it, ranked on recency and importance with relevance 0: the join to the
// vectors is a LEFT JOIN. An inner join would make an episode disappear from
// memory for exactly as long as the model is away. Whether relevance was
// available is said per row: the embedded episode in the same answer says yes,
// the one with no vector says no.
func TestLiveRecallReturnsAnEpisodeStillWaitingForItsVector(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	ctx := asOwner("user-games-waiting-" + n)
	now := time.Now().UTC().Truncate(time.Second)

	lg.provider.space = failingSpace{testSpace{db: lg.db.DB, tableName: lg.table}}
	lg.must(t, ctx, "builtin", "gamesRecordEpisodes", map[string]any{"episodes": episodesArg(
		episode("e-waiting-"+n, "wren", "", "wren found a lantern", now.Add(-1*time.Hour), 9),
	)})
	lg.provider.space = testSpace{db: lg.db.DB, tableName: lg.table}
	lg.must(t, ctx, "builtin", "gamesRecordEpisodes", map[string]any{"episodes": episodesArg(
		episode("e-embedded-"+n, "wren", "", "wren fed the chickens", now.Add(-100*time.Hour), 2),
	)})
	if got := lg.vectorsFor(t, "e-waiting-"+n, "e-embedded-"+n); got != 1 {
		t.Fatalf("%d vectors stored, want only the embedded episode's -- without this the next check proves nothing", got)
	}

	episodes := episodeRows(lg.must(t, ctx, "builtin", "gamesRecall", map[string]any{
		"characterId": "wren", "worldId": "meadow", "text": "lantern", "asOf": rfc3339(now),
	}))
	if len(episodes) != 2 {
		t.Fatalf("recall returned %d episodes, want the one waiting for its vector as well as the embedded one: %v", len(episodes), episodes)
	}
	seen := map[string]bool{}
	for _, ep := range episodes {
		switch ep["id"] {
		case "e-waiting-" + n:
			row := asObject(ep["row"])
			want := recallScore(0, now.Sub(timeOf(row["at"])).Hours(), 9, defaultRecallWeights())
			if asFloat(ep["relevance"], -1) != 0 || math.Abs(asFloat(ep["score"], 0)-want) > 1e-6 {
				t.Fatalf("the episode with no vector has relevance %v and score %v, want relevance 0 and score %v", ep["relevance"], ep["score"], want)
			}
			if ep["relevanceAvailable"] != false {
				t.Errorf("the episode with no vector says relevanceAvailable %v, want false: it was given no relevance", ep["relevanceAvailable"])
			}
		case "e-embedded-" + n:
			if ep["relevanceAvailable"] != true {
				t.Errorf("the embedded episode says relevanceAvailable %v, want true: the space is present and it has a vector", ep["relevanceAvailable"])
			}
		default:
			continue
		}
		seen[trimmed(ep["id"])] = true
	}
	if !seen["e-waiting-"+n] || !seen["e-embedded-"+n] {
		t.Fatalf("both episodes must be in the answer, saw %v of %v", seen, episodes)
	}
}

// After the episodes come the player's most confident preferences, the
// character's open promises to the player and the world's places, each kind
// capped by its own argument, every row ranked after the one before it and
// every id bare. A recall that names no player appends neither preferences nor
// promises.
func TestLiveRecallAppendsPreferencesPromisesAndPlaces(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	ctx := asOwner("user-games-append-" + n)
	now := time.Now().UTC().Truncate(time.Second)
	player, world := "p-ada-"+n, "world-"+n

	lg.must(t, ctx, "mutation", "upsertGamesPlayer", map[string]any{"playerId": player, "gamertag": "Ada" + n, "displayName": "Ada", "consented": true})
	lg.must(t, ctx, "builtin", "gamesRecordEpisodes", map[string]any{"episodes": episodesArg(
		episode("e-append-"+n, "wren", player, "Ada waved at wren", now.Add(-1*time.Hour), 5),
	)})
	for i, confidence := range []float64{0.6, 0.9, 0.7, 0.8} { // the top three are subjects 1, 3 and 2
		lg.must(t, ctx, "mutation", "upsertGamesPreference", map[string]any{
			"preferenceId": fmt.Sprintf("pref-%d-%s", i, n), "playerId": player,
			"subject": fmt.Sprintf("subject %d", i), "valence": 0.5, "confidence": confidence,
		})
	}
	// Six open promises by wren, the newest first, and two that are not wren's to
	// keep: one already kept, one made by another character.
	for i := 0; i < 6; i++ {
		lg.must(t, ctx, "mutation", "upsertGamesPromise", map[string]any{
			"promiseId": fmt.Sprintf("prom-%d-%s", i, n), "characterId": "wren", "playerId": player,
			"text": fmt.Sprintf("promise %d", i), "status": "open", "madeAt": rfc3339(now.Add(-time.Duration(i) * time.Minute)),
		})
	}
	for _, p := range []struct{ id, character, text, status string }{
		{"kept", "wren", "plant a tree", "kept"},
		{"theirs", "otis", "build a bridge", "open"},
	} {
		lg.must(t, ctx, "mutation", "upsertGamesPromise", map[string]any{
			"promiseId": "prom-" + p.id + "-" + n, "characterId": p.character, "playerId": player,
			"text": p.text, "status": p.status, "madeAt": rfc3339(now),
		})
	}
	for i := 0; i < 12; i++ { // the ten most recently confirmed are places 0 to 9
		lg.must(t, ctx, "mutation", "upsertGamesPlace", map[string]any{
			"placeId": fmt.Sprintf("place-%d-%s", i, n), "worldId": world, "name": fmt.Sprintf("place %d", i),
			"kind": "base", "position": map[string]any{"x": i}, "at": rfc3339(now.Add(-time.Duration(i) * time.Minute)),
		})
	}
	lg.must(t, ctx, "mutation", "upsertGamesPlace", map[string]any{
		"placeId": "place-elsewhere-" + n, "worldId": "elsewhere-" + n, "name": "place elsewhere",
		"kind": "base", "position": map[string]any{"x": 0}, "at": rfc3339(now),
	})

	// ranked lists the answer in rank order as "kind:text", and checks that the
	// ranks are 1..N and every id is bare.
	ranked := func(t *testing.T, args map[string]any) []string {
		t.Helper()
		args["characterId"], args["worldId"], args["text"], args["asOf"] = "wren", world, "waved", rfc3339(now)
		var out []string
		for i, it := range byRank(lg.must(t, ctx, "builtin", "gamesRecall", args)) {
			if got := asInt(it["rank"], 0); got != i+1 {
				t.Fatalf("row %d has rank %d, want %d: ranks are 1..N in order", i, got, i+1)
			}
			id := trimmed(it["id"])
			if id == "" || strings.Contains(id, ":") || trimmed(asObject(it["row"])["id"]) != id {
				t.Fatalf("row %d has id %q and row.id %v, want one bare id on both", i, id, asObject(it["row"])["id"])
			}
			if it["kind"] == "preference" && asFloat(it["score"], -1) != asFloat(asObject(it["row"])["confidence"], -2) {
				t.Fatalf("a preference's score is its confidence, got %v for %v", it["score"], it["row"])
			}
			out = append(out, fmt.Sprintf("%v:%v", it["kind"], it["text"]))
		}
		return out
	}
	listed := func(kind string, from, to int) []string {
		var out []string
		for i := from; i < to; i++ {
			out = append(out, fmt.Sprintf("%s:%s %d", kind, kind, i))
		}
		return out
	}
	join := func(parts ...[]string) []string {
		var out []string
		for _, p := range parts {
			out = append(out, p...)
		}
		return out
	}

	episodeRow := []string{"episode:Ada waved at wren"}
	check := func(label string, args map[string]any, want []string) {
		t.Helper()
		if got := ranked(t, args); !sameStrings(got, want) {
			t.Fatalf("%s:\n got %q\nwant %q", label, got, want)
		}
	}
	check("the defaults are 3 preferences, 5 promises and 10 places",
		map[string]any{"playerId": player},
		join(episodeRow, []string{"preference:subject 1", "preference:subject 3", "preference:subject 2"},
			listed("promise", 0, 5), listed("place", 0, 10)))
	check("preferences 1, promises 2, places 0",
		map[string]any{"playerId": player, "preferences": 1, "promises": 2, "places": 0},
		join(episodeRow, []string{"preference:subject 1"}, listed("promise", 0, 2)))
	check("preferences 0, promises 0, places 2",
		map[string]any{"playerId": player, "preferences": 0, "promises": 0, "places": 2},
		join(episodeRow, listed("place", 0, 2)))
	check("naming no player appends nothing about one, and wren's own episode and the places still come",
		map[string]any{},
		join(episodeRow, listed("place", 0, 10)))
}

// A vector pointing away from the query has a negative cosine, which counts as
// zero relevance, as recallScore says: the statement must not let it pull the
// score below what recency and importance give. The row was still given a
// relevance, of 0, so it says one was available. The test embedder only makes
// non-negative vectors, so the opposite one is stored directly.
func TestLiveRecallCountsANegativeCosineAsZeroRelevance(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	ctx := asOwner("user-games-cosine-" + n)
	now := time.Now().UTC().Truncate(time.Second)
	lg.must(t, ctx, "builtin", "gamesRecordEpisodes", map[string]any{"episodes": episodesArg(
		episode("e-toward-"+n, "wren", "", "wren explored a cave", now.Add(-1*time.Hour), 5),
		episode("e-away-"+n, "wren", "", "wren explored a cave", now.Add(-1*time.Hour), 5),
	)})
	opposite := bagOfWords("cave explored")
	for i := range opposite {
		opposite[i] = -opposite[i]
	}
	if _, err := lg.db.DB.Exec(`UPDATE `+lg.table+` SET embedding = $2::vector WHERE id = $1 AND vector_field = 'content'`,
		canonical(conceptEpisode, "e-away-"+n), vectorLiteral(opposite)); err != nil {
		t.Fatalf("store the opposite vector: %v", err)
	}

	episodes := episodeRows(lg.must(t, ctx, "builtin", "gamesRecall", map[string]any{
		"characterId": "wren", "worldId": "meadow", "text": "cave explored", "asOf": rfc3339(now),
	}))
	if len(episodes) != 2 {
		t.Fatalf("recall returned %d episodes, want 2: %v", len(episodes), episodes)
	}
	for _, ep := range episodes {
		relevance := asFloat(ep["relevance"], -1)
		switch ep["id"] {
		case "e-toward-" + n:
			if relevance <= 0 {
				t.Errorf("the episode pointing toward the query has relevance %v, want some -- without this the next check proves nothing", relevance)
			}
		case "e-away-" + n:
			if relevance != 0 {
				t.Errorf("the episode pointing away from the query has relevance %v, want 0", relevance)
			}
			if ep["relevanceAvailable"] != true {
				t.Errorf("the episode pointing away from the query says relevanceAvailable %v, want true: it has a vector, and a relevance of 0 is not the absence of one", ep["relevanceAvailable"])
			}
		}
		row := asObject(ep["row"])
		want := recallScore(relevance, now.Sub(timeOf(row["at"])).Hours(), asFloat(row["importance"], 0), defaultRecallWeights())
		if math.Abs(asFloat(ep["score"], 0)-want) > 1e-6 {
			t.Errorf("score %v disagrees with recallScore %v for %v", ep["score"], want, ep["id"])
		}
	}
}

// The caller's weights decide the order, each in its own place in the score, and
// k bounds the episodes. Four episodes differ in age and importance; importance
// alone and recency alone pick different winners.
func TestLiveRecallHonoursTheCallersWeightsAndK(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	ctx := asOwner("user-games-weights-" + n)
	now := time.Now().UTC().Truncate(time.Second)
	lg.must(t, ctx, "builtin", "gamesRecordEpisodes", map[string]any{"episodes": episodesArg(
		episode("e-fresh-"+n, "wren", "", "the freshest", now.Add(-1*time.Hour), 3),
		episode("e-vital-"+n, "wren", "", "the most important", now.Add(-500*time.Hour), 9),
		episode("e-middle-"+n, "wren", "", "in between", now.Add(-50*time.Hour), 6),
		episode("e-recent-"+n, "wren", "", "second freshest", now.Add(-2*time.Hour), 1),
	)})
	recall := func(args map[string]any) []map[string]any {
		args["characterId"], args["worldId"], args["text"], args["asOf"] = "wren", "meadow", "anything", rfc3339(now)
		return episodeRows(lg.must(t, ctx, "builtin", "gamesRecall", args))
	}
	ids := func(eps []map[string]any) []string {
		var out []string
		for _, ep := range eps {
			out = append(out, trimmed(ep["id"]))
		}
		return out
	}

	byImportance := recall(map[string]any{"wRelevance": 0, "wRecency": 0, "wImportance": 1, "k": 3})
	if got, want := ids(byImportance), []string{"e-vital-" + n, "e-middle-" + n, "e-fresh-" + n}; !sameStrings(got, want) {
		t.Fatalf("by importance alone, k 3: got %v, want %v", got, want)
	}
	if got := asFloat(byImportance[0]["score"], -1); math.Abs(got-0.9) > 1e-9 {
		t.Fatalf("by importance alone the most important episode scores importance/10, got %v, want 0.9", got)
	}
	byRecency := recall(map[string]any{"wRelevance": 0, "wRecency": 1, "wImportance": 0, "k": 2})
	if got, want := ids(byRecency), []string{"e-fresh-" + n, "e-recent-" + n}; !sameStrings(got, want) {
		t.Fatalf("by recency alone, k 2: got %v, want %v", got, want)
	}
	if got := recall(map[string]any{"k": 0}); len(got) != 1 {
		t.Fatalf("k 0 is at least one episode, got %d", len(got))
	}
	if got := recall(map[string]any{}); len(got) != 4 {
		t.Fatalf("the default k holds all four episodes, got %d", len(got))
	}
}

// An episode dated after asOf (a runtime a little ahead of the clock recall is
// asked from) counts as new: recency 1, never above it.
func TestLiveRecallCountsAnEpisodeFromTheFutureAsNew(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	ctx := asOwner("user-games-future-" + n)
	now := time.Now().UTC().Truncate(time.Second)
	lg.must(t, ctx, "builtin", "gamesRecordEpisodes", map[string]any{"episodes": episodesArg(
		episode("e-future-"+n, "wren", "", "wren is ahead of the clock", now.Add(10*time.Hour), 4),
	)})
	episodes := episodeRows(lg.must(t, ctx, "builtin", "gamesRecall", map[string]any{
		"characterId": "wren", "worldId": "meadow", "text": "anything", "asOf": rfc3339(now),
	}))
	if len(episodes) != 1 {
		t.Fatalf("recall returned %d episodes, want 1: %v", len(episodes), episodes)
	}
	if got := asFloat(episodes[0]["recency"], -1); math.Abs(got-1) > 1e-12 {
		t.Fatalf("recency = %v for an episode ten hours in the future, want exactly 1", got)
	}
	want := recallScore(asFloat(episodes[0]["relevance"], 0), -10, 4, defaultRecallWeights())
	if got := asFloat(episodes[0]["score"], 0); math.Abs(got-want) > 1e-9 {
		t.Fatalf("score %v disagrees with recallScore %v", got, want)
	}
}

// ---- asOf and the wait for the query vector ----

// asOf is the game's present moment. One that cannot be read as a moment is
// refused before any query, with the stable code, instead of quietly measuring
// from the server's clock; one that can is read as the instant it names. The
// game's present is in 2020 here, years from the server's clock, so an asOf that
// was ignored cannot pass for one that was read.
func TestLiveRecallRefusesAnAsOfItCannotMeasureFrom(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	ctx := asOwner("user-games-asof-" + n)
	then := time.Date(2020, 3, 1, 4, 30, 0, 0, time.UTC) // the game's present
	lg.must(t, ctx, "builtin", "gamesRecordEpisodes", map[string]any{"episodes": episodesArg(
		episode("e-asof-"+n, "wren", "", "wren waited by the gate", then.Add(-72*time.Hour), 5),
	)})
	recall := func(asOf any) ([]map[string]any, error) {
		return lg.call(t, ctx, "builtin", "gamesRecall", map[string]any{
			"characterId": "wren", "worldId": "meadow", "text": "gate", "asOf": asOf,
		})
	}

	for _, tc := range []struct {
		name string
		asOf any
	}{
		{"words", "not a time"},
		{"a space where the T belongs", "2026-10-18 12:00:00"},
		{"a number", 12345},
		{"year 0000", "0000-01-01T00:00:00Z"},
	} {
		t.Run("refuses "+tc.name, func(t *testing.T) {
			items, err := recall(tc.asOf)
			if err == nil || !strings.HasPrefix(err.Error(), "games_invalid_argument: asOf ") {
				t.Fatalf("asOf %v: error = %v (answered %d rows), want games_invalid_argument: asOf ...", tc.asOf, err, len(items))
			}
		})
	}
	for _, tc := range []struct {
		name string
		asOf string
	}{
		{"an offset", "2020-03-01T10:00:00+05:30"},
		{"the stored form", "2020-03-01T04:30:00.000Z"},
		{"a plain Z", "2020-03-01T04:30:00Z"},
	} {
		t.Run("reads "+tc.name, func(t *testing.T) {
			items, err := recall(tc.asOf)
			if err != nil {
				t.Fatalf("asOf %s must answer: %v", tc.asOf, err)
			}
			episodes := episodeRows(items)
			if len(episodes) != 1 {
				t.Fatalf("recall returned %d episodes, want 1: %v", len(episodes), episodes)
			}
			if got := asFloat(episodes[0]["recency"], -1); math.Abs(got-0.5) > 1e-6 {
				t.Fatalf("recency = %v, want 0.5: the episode is one half-life (72 hours) before the instant %s names", got, tc.asOf)
			}
		})
	}
}

// slowModelPatience is how long slowSpace holds a query before it gives up on
// its own, so a recall that does not bound its wait fails a test instead of
// hanging it.
const slowModelPatience = 5 * time.Second

// slowSpace is the embedder when the model is slow rather than away: the space
// exists, and embedQuery does not answer until the test loses patience, when it
// does, with a vector.
type slowSpace struct {
	testSpace
	ignoresContext bool // a provider that does not watch the context it is given
}

func (s slowSpace) embedQuery(ctx context.Context, text string) ([]float32, error) {
	patience := time.NewTimer(slowModelPatience)
	defer patience.Stop()
	if s.ignoresContext {
		<-patience.C
		return bagOfWords(text), nil
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-patience.C:
		return bagOfWords(text), nil
	}
}

// A decision waits on a recall, so a model that is slow rather than away must
// cost it no more than its bound. The vector of the query is waited for that long
// and no longer, whether the embedder watches its context or not, and then recall
// answers as it does with the model away: ranked on recency and importance,
// relevance 0, relevanceAvailable false on every episode.
func TestLiveRecallGivesUpOnASlowModelAndAnswersWithoutRelevance(t *testing.T) {
	const timeout = 200 * time.Millisecond
	for _, tc := range []struct {
		name           string
		ignoresContext bool
	}{
		{"an embedder that watches its context", false},
		{"an embedder that does not", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lg := newLiveGames(t)
			n := runID()
			ctx := asOwner("user-games-slow-" + n)
			now := time.Now().UTC().Truncate(time.Second)
			lg.must(t, ctx, "builtin", "gamesRecordEpisodes", map[string]any{"episodes": episodesArg(
				episode("e-cave-"+n, "wren", "", "wren explored a cave", now.Add(-200*time.Hour), 9),
				episode("e-farm-"+n, "wren", "", "wren fed the chickens", now.Add(-1*time.Hour), 3),
			)})
			lg.provider.embedTimeout = timeout
			lg.provider.space = slowSpace{testSpace: testSpace{db: lg.db.DB, tableName: lg.table}, ignoresContext: tc.ignoresContext}

			start := time.Now()
			items := lg.must(t, ctx, "builtin", "gamesRecall", map[string]any{
				"characterId": "wren", "worldId": "meadow", "text": "cave", "asOf": rfc3339(now),
			})
			if elapsed := time.Since(start); elapsed > 5*timeout {
				t.Errorf("recall took %v with a wait bound of %v, want about the bound", elapsed, timeout)
			}
			episodes := episodeRows(items)
			if len(episodes) != 2 || episodes[0]["id"] != "e-farm-"+n {
				t.Fatalf("with no relevance, recency and importance must decide: %v", episodes)
			}
			for _, ep := range episodes {
				if ep["relevanceAvailable"] != false || asFloat(ep["relevance"], -1) != 0 {
					t.Errorf("after giving up on the model every episode must say relevance was unavailable: %v", ep)
				}
			}
		})
	}
}

// ---- the scope of a recall about a player, and the last edges ----

// A recall that names a player reaches the episodes about that player through a
// second arm of the scope, and the owner conjunct must cover that arm as well as
// the character's: an owner conjunct inside the character arm alone leaves every
// episode about a player open to anyone who names the player. B, as another
// character and naming A's player, reaches none of A's episodes about them, nor
// the preferences, promises and places of A that the same call appends; A, asking
// the same question as the same character, reaches one of each. The two calls
// differ in the owner alone, and the episode was recorded by a character that is
// neither of them, so it can only be reached through the player.
func TestLiveRecallAboutAPlayerIsTheOwnersAlone(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	a, b := asOwner("user-games-pa-"+n), asOwner("user-games-pb-"+n)
	now := time.Now().UTC().Truncate(time.Second)
	player, world := "p-ada-"+n, "world-"+n

	lg.must(t, a, "mutation", "upsertGamesPlayer", map[string]any{"playerId": player, "gamertag": "Ada" + n, "displayName": "Ada", "consented": true})
	lg.must(t, a, "builtin", "gamesRecordEpisodes", map[string]any{"episodes": episodesArg(
		episode("e-pa-"+n, "wren", player, "Ada showed wren a lantern", now.Add(-1*time.Hour), 5),
	)})
	lg.must(t, a, "mutation", "upsertGamesPreference", map[string]any{
		"preferenceId": "pref-pa-" + n, "playerId": player, "subject": "lanterns", "valence": 0.5, "confidence": 0.8,
	})
	lg.must(t, a, "mutation", "upsertGamesPromise", map[string]any{
		"promiseId": "prom-pa-" + n, "characterId": "otis", "playerId": player, "text": "carry the lantern", "status": "open", "madeAt": rfc3339(now),
	})
	lg.must(t, a, "mutation", "upsertGamesPlace", map[string]any{
		"placeId": "place-pa-" + n, "worldId": world, "name": "the lantern shed", "kind": "base", "position": map[string]any{"x": 1}, "at": rfc3339(now),
	})

	// recalledAs is what otis, naming the player, gets back for each kind.
	recalledAs := func(owner context.Context) map[string]int {
		counts := map[string]int{}
		for _, it := range lg.must(t, owner, "builtin", "gamesRecall", map[string]any{
			"characterId": "otis", "playerId": player, "worldId": world, "text": "lantern", "asOf": rfc3339(now),
		}) {
			counts[fmt.Sprint(it["kind"])]++
		}
		return counts
	}
	want := map[string]int{"episode": 1, "preference": 1, "promise": 1, "place": 1}
	if got := recalledAs(a); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("owner A recalled %v about their own player, want %v -- without this the next check proves nothing", got, want)
	}
	if got := recalledAs(b); len(got) != 0 {
		t.Fatalf("owner B recalled %v of owner A's rows about a player of A's", got)
	}
}

// tableErrorSpace is the embedder space when it cannot be read at all: the
// binding or the catalogue of vector tables is out of reach.
type tableErrorSpace struct{ testSpace }

func (tableErrorSpace) table(context.Context) (string, error) {
	return "", errors.New("the embedder space could not be read")
}

// tableNameSpace answers the name of a table that it was handed, as a space
// would if what it read were wrong or hostile.
type tableNameSpace struct {
	testSpace
	name string
}

func (s tableNameSpace) table(context.Context) (string, error) { return s.name, nil }

// A space that cannot be read, or that names something that is no vector table,
// is no reason for a recall to fail: it answers, ranked on recency and importance,
// with every episode saying relevance was unavailable, as with no embedder. The
// hostile name below is harmless on purpose, so that a statement that took it
// would still run and show itself in the answer: it is a real vector table with
// the rest of the join, and a comment, after it.
func TestLiveRecallRanksWithoutRelevanceWhenTheEmbedderSpaceCannotBeUsed(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	ctx := asOwner("user-games-space-" + n)
	now := time.Now().UTC().Truncate(time.Second)
	lg.must(t, ctx, "builtin", "gamesRecordEpisodes", map[string]any{"episodes": episodesArg(
		episode("e-cave-"+n, "wren", "", "wren explored a cave", now.Add(-200*time.Hour), 9),
		episode("e-farm-"+n, "wren", "", "wren fed the chickens", now.Add(-1*time.Hour), 3),
	)})
	recallArgs := map[string]any{"characterId": "wren", "worldId": "meadow", "text": "cave", "asOf": rfc3339(now)}
	base := testSpace{db: lg.db.DB, tableName: lg.table}
	var logged bytes.Buffer
	lg.provider.log = slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// The control: with the space as it is, the relevant old episode wins and says
	// relevance was available, so the answers below differ for the space's doing.
	if there := episodeRows(lg.must(t, ctx, "builtin", "gamesRecall", recallArgs)); len(there) != 2 || there[0]["id"] != "e-cave-"+n || there[0]["relevanceAvailable"] != true {
		t.Fatalf("with the space there, the relevant episode must rank first and say so: %v", there)
	}

	for _, tc := range []struct {
		name  string
		space embeddingSpace
		warns string // what the warning says
	}{
		{"a space that cannot be read", tableErrorSpace{base}, "could not resolve the embedder space"},
		{"a table name that is no vector table", tableNameSpace{base, lg.table + " nv ON nv.id = latest.id AND nv.vector_field = 'content' --"}, "no vector table"},
		{"a table name that is a table of another kind", tableNameSpace{base, "MemoryNodes"}, "no vector table"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lg.provider.space = tc.space
			logged.Reset()
			items, err := lg.call(t, ctx, "builtin", "gamesRecall", recallArgs)
			if err != nil {
				t.Fatalf("%s must not fail the recall: %v", tc.name, err)
			}
			if out := logged.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, tc.warns) {
				t.Errorf("%s must be logged as a warning that says %q, got:\n%s", tc.name, tc.warns, out)
			}
			episodes := episodeRows(items)
			if len(episodes) != 2 || episodes[0]["id"] != "e-farm-"+n {
				t.Fatalf("with no relevance, recency and importance must decide: %v", episodes)
			}
			for _, ep := range episodes {
				if ep["relevanceAvailable"] != false || asFloat(ep["relevance"], -1) != 0 {
					t.Errorf("every episode must say relevance was unavailable: %v", ep)
				}
			}
		})
	}
}

// A stored vector that has no direction (a zero vector) has a cosine that is not
// a number. It must not take the recall down: the answer would fail to encode,
// and a row that sorts above every number would lead the ranking. Its row has no
// relevance, says none was available, and ranks on what it has, while the other
// episodes are untouched. The write path never stores one; this is what a bad
// row, or a table written by anything else, would do.
func TestLiveRecallCountsAnUnusableVectorAsNoRelevance(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	ctx := asOwner("user-games-zerovec-" + n)
	now := time.Now().UTC().Truncate(time.Second)
	lg.must(t, ctx, "builtin", "gamesRecordEpisodes", map[string]any{"episodes": episodesArg(
		episode("e-zero-"+n, "wren", "", "wren explored a cave", now.Add(-1*time.Hour), 9),
		episode("e-good-"+n, "wren", "", "wren explored a cave", now.Add(-1*time.Hour), 5),
		episode("e-plain-"+n, "wren", "", "wren fed the chickens", now.Add(-100*time.Hour), 3),
	)})
	if _, err := lg.db.DB.Exec(`UPDATE `+lg.table+` SET embedding = $2::vector WHERE id = $1 AND vector_field = 'content'`,
		canonical(conceptEpisode, "e-zero-"+n), vectorLiteral(make([]float32, testDims))); err != nil {
		t.Fatalf("store the zero vector: %v", err)
	}

	items, err := lg.call(t, ctx, "builtin", "gamesRecall", map[string]any{
		"characterId": "wren", "worldId": "meadow", "text": "cave explored", "asOf": rfc3339(now),
	})
	if err != nil {
		t.Fatalf("a vector with no direction must not fail the recall: %v", err)
	}
	episodes := episodeRows(items)
	if len(episodes) != 3 {
		t.Fatalf("recall returned %d episodes, want 3: %v", len(episodes), episodes)
	}
	score := map[string]float64{}
	for _, ep := range episodes {
		id := trimmed(ep["id"])
		score[id] = asFloat(ep["score"], math.NaN())
		row := asObject(ep["row"])
		want := recallScore(asFloat(ep["relevance"], 0), now.Sub(timeOf(row["at"])).Hours(), asFloat(row["importance"], 0), defaultRecallWeights())
		if math.Abs(score[id]-want) > 1e-6 {
			t.Errorf("score %v disagrees with recallScore %v for %v", ep["score"], want, id)
		}
		switch id {
		case "e-zero-" + n:
			if asFloat(ep["relevance"], -1) != 0 || ep["relevanceAvailable"] != false {
				t.Errorf("the episode whose vector has no direction has relevance %v and says %v, want 0 and false", ep["relevance"], ep["relevanceAvailable"])
			}
		case "e-good-" + n:
			if asFloat(ep["relevance"], -1) <= 0 || ep["relevanceAvailable"] != true {
				t.Errorf("the episode beside it has relevance %v and says %v, want some and true -- without this the check above proves nothing", ep["relevance"], ep["relevanceAvailable"])
			}
		}
	}
	if score["e-zero-"+n] >= score["e-good-"+n] {
		t.Errorf("the episode with no usable vector (%v) must not outrank the relevant one (%v)", score["e-zero-"+n], score["e-good-"+n])
	}
}

// The weights and the half-life reach a float8 expression, and a value no sane use
// needs can overflow it: the database then answers with a raw out-of-range error.
// They are held to bounds, and a value outside them is refused before the statement
// runs. The edges themselves answer, with a finite score that is the formula's.
func TestLiveRecallRefusesTunablesThatWouldOverflowTheStatement(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	ctx := asOwner("user-games-overflow-" + n)
	now := time.Now().UTC().Truncate(time.Second)
	lg.must(t, ctx, "builtin", "gamesRecordEpisodes", map[string]any{"episodes": episodesArg(
		episode("e-edge-"+n, "wren", "", "wren explored a cave", now.Add(-10*time.Hour), 7),
	)})
	// The handler is called directly: a number like 1e305 cannot be written in the
	// language, and the refusal is the handler's.
	recall := func(extra map[string]any) ([]map[string]any, error) {
		args := map[string]any{"characterId": "wren", "worldId": "meadow", "text": "cave", "asOf": rfc3339(now)}
		for k, v := range extra {
			args[k] = v
		}
		nodes, err := lg.provider.recall(ctx, args, 0)
		if err != nil {
			return nil, err
		}
		var rows []map[string]any
		for _, node := range nodes {
			var row map[string]any
			if err := json.Unmarshal(node.Payload, &row); err != nil {
				t.Fatalf("the answer does not decode: %v", err)
			}
			rows = append(rows, row)
		}
		return rows, nil
	}

	for _, tc := range []struct {
		key   string
		value any
	}{
		{"halfLifeHours", 1e305}, {"halfLifeHours", 87601.0}, {"halfLifeHours", 0.0000001}, {"halfLifeHours", math.NaN()},
		{"wRelevance", 1e305}, {"wRecency", math.MaxFloat64}, {"wImportance", 1000.5}, {"wRelevance", -1.0}, {"wRecency", math.Inf(1)},
	} {
		rows, err := recall(map[string]any{tc.key: tc.value})
		if err == nil || !strings.HasPrefix(err.Error(), "games_invalid_argument: "+tc.key+" ") {
			t.Errorf("%s %v: error = %v (answered %d rows), want games_invalid_argument: %s ...", tc.key, tc.value, err, len(rows), tc.key)
		}
	}

	for _, tc := range []struct {
		name string
		args map[string]any
		w    recallWeights
	}{
		{"every weight at its largest and the longest half-life", map[string]any{"wRelevance": 1000.0, "wRecency": 1000.0, "wImportance": 1000.0, "halfLifeHours": 87600.0},
			recallWeights{relevance: 1000, recency: 1000, importance: 1000, halfLifeHours: 87600}},
		{"every weight at zero and the shortest half-life", map[string]any{"wRelevance": 0.0, "wRecency": 0.0, "wImportance": 0.0, "halfLifeHours": 0.000001},
			recallWeights{halfLifeHours: 0.000001}},
		{"a half-life of the most negative number there is, which is recency off", map[string]any{"halfLifeHours": -math.MaxFloat64},
			recallWeights{relevance: 0.5, recency: 0.3, importance: 0.2, halfLifeHours: 0}},
		{"a half-life of minus 1e308", map[string]any{"halfLifeHours": -1e308},
			recallWeights{relevance: 0.5, recency: 0.3, importance: 0.2, halfLifeHours: 0}},
	} {
		rows, err := recall(tc.args)
		if err != nil {
			t.Errorf("%s must answer: %v", tc.name, err)
			continue
		}
		if len(rows) == 0 {
			t.Errorf("%s answered nothing", tc.name)
			continue
		}
		row := asObject(rows[0]["row"])
		want := recallScore(asFloat(rows[0]["relevance"], 0), now.Sub(timeOf(row["at"])).Hours(), asFloat(row["importance"], 0), tc.w)
		if got := asFloat(rows[0]["score"], math.NaN()); math.IsNaN(got) || math.Abs(got-want) > 1e-6*math.Max(1, math.Abs(want)) {
			t.Errorf("%s: score %v, want the formula's %v", tc.name, rows[0]["score"], want)
		}
	}
}

// A model that is away is the ordinary state of a laptop that sleeps, and is
// logged as such; an embedder that panics is a bug, and is logged as one, with the
// stack that shows where. Either way the recall answers without relevance.
func TestLiveRecallLogsAnEmbedderPanicAsAWarningWithItsStack(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	ctx := asOwner("user-games-panic-" + n)
	now := time.Now().UTC().Truncate(time.Second)
	lg.must(t, ctx, "builtin", "gamesRecordEpisodes", map[string]any{"episodes": episodesArg(
		episode("e-cave-"+n, "wren", "", "wren explored a cave", now.Add(-200*time.Hour), 9),
		episode("e-farm-"+n, "wren", "", "wren fed the chickens", now.Add(-1*time.Hour), 3),
	)})
	var logged bytes.Buffer
	lg.provider.log = slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
	base := testSpace{db: lg.db.DB, tableName: lg.table}

	recall := func(space embeddingSpace) {
		t.Helper()
		lg.provider.space = space
		episodes := episodeRows(lg.must(t, ctx, "builtin", "gamesRecall", map[string]any{
			"characterId": "wren", "worldId": "meadow", "text": "cave", "asOf": rfc3339(now),
		}))
		if len(episodes) != 2 || episodes[0]["id"] != "e-farm-"+n || episodes[0]["relevanceAvailable"] != false {
			t.Fatalf("with the embedder gone, recency and importance must decide and say so: %v", episodes)
		}
	}

	recall(failingSpace{base})
	if out := logged.String(); strings.Contains(out, "level=WARN") || !strings.Contains(out, "level=INFO") {
		t.Fatalf("a model that is away is logged at Info and is no alarm, got:\n%s", out)
	}

	logged.Reset()
	recall(panickingSpace{base})
	out := logged.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "stack=") || !strings.Contains(out, "panickingSpace") {
		t.Fatalf("an embedder that panics must be logged at Warn with the stack that shows where, got:\n%s", out)
	}
	if strings.Contains(out, "level=INFO") {
		t.Errorf("the panic must be logged once, as a warning, and not again as an ordinary failure:\n%s", out)
	}
}

// The statement as the planner runs it. The similarity is read in several places
// and a common table expression that is used once is inlined into its reader, so
// the distance between the query and a vector, which costs more the wider the
// vector, would be computed at each of them: eight times a row. It appears once
// in the plan. Parallel workers are switched off for the plan because a Gather
// node prints its workers' output again, which is not a second computation.
func TestLiveRecallComputesEachVectorDistanceOnce(t *testing.T) {
	lg := newLiveGames(t)
	now := time.Now().UTC().Truncate(time.Second)
	ctx := context.Background()
	tx, err := lg.db.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SET LOCAL max_parallel_workers_per_gather = 0`); err != nil {
		t.Fatalf("set: %v", err)
	}
	rows, err := tx.QueryContext(ctx, "EXPLAIN (VERBOSE) "+recallStatement(lg.table),
		ownerKey("user-games-explain-"+runID()), "wren", "", rfc3339(now), 72.0, 0.5, 0.3, 0.2, 8, vectorLiteral(bagOfWords("cave")))
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan: %v", err)
		}
		plan.WriteString(line + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan: %v", err)
	}
	if got := strings.Count(plan.String(), "<=>"); got != 1 {
		t.Errorf("the vector distance is computed %d times in the plan, want once:\n%s", got, plan.String())
	}
}
