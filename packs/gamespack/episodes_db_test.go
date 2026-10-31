package gamespack

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// episode is one gamesRecordEpisodes entry, in the meadow.
func episode(id, character, player, summary string, at time.Time, importance int) map[string]any {
	e := map[string]any{
		"episodeId": id, "characterId": character, "worldId": "meadow",
		"at": rfc3339(at), "kind": "chat", "summary": summary, "importance": importance,
	}
	if player != "" {
		e["playerId"] = player
	}
	return e
}

func episodesArg(eps ...map[string]any) []any {
	out := make([]any, len(eps))
	for i, e := range eps {
		out[i] = e
	}
	return out
}

// vectorsFor counts the vectors the test space holds for these episodes.
func (lg *liveGames) vectorsFor(t *testing.T, episodeIDs ...string) int {
	t.Helper()
	ids := make([]any, len(episodeIDs))
	marks := make([]string, len(episodeIDs))
	for i, id := range episodeIDs {
		ids[i] = canonical(conceptEpisode, id)
		marks[i] = "$" + string(rune('1'+i))
	}
	var n int
	if err := lg.db.DB.QueryRow(`SELECT count(*) FROM `+lg.table+` WHERE id IN (`+strings.Join(marks, ", ")+`)`, ids...).Scan(&n); err != nil {
		t.Fatalf("count vectors: %v", err)
	}
	return n
}

// A batch is written under the caller and every summary is embedded.
func TestLiveRecordEpisodesWritesAndEmbeds(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	ctx := asOwner("user-games-episodes-" + n)
	now := time.Now().UTC()
	res := lg.must(t, ctx, "builtin", "gamesRecordEpisodes", map[string]any{"episodes": episodesArg(
		episode("e-garden-"+n, "wren", "", "wren planted a garden", now, 4),
		episode("e-cave-"+n, "wren", "", "wren found a cave", now, 6),
	)})
	if asInt(res[0]["written"], 0) != 2 || asInt(res[0]["embedded"], 0) != 2 || asInt(res[0]["pending"], -1) != 0 {
		t.Fatalf("answer = %v, want 2 written, 2 embedded, 0 pending", res[0])
	}
	rows := lg.must(t, ctx, "query", "gamesSessionEpisodes", map[string]any{"characterId": "wren", "since": rfc3339(now.Add(-time.Minute))})
	if len(rows) != 2 {
		t.Fatalf("the caller reads %d of the episodes they recorded, want 2: %v", len(rows), rows)
	}
	if got := lg.vectorsFor(t, "e-garden-"+n, "e-cave-"+n); got != 2 {
		t.Fatalf("%d vectors stored, want one per episode", got)
	}

	tooMany := make([]map[string]any, maxEpisodesPerCall+1)
	for i := range tooMany {
		tooMany[i] = episode("e-flood-"+n+"-"+string(rune('a'+i%26))+string(rune('a'+i/26)), "wren", "", "one of too many", now, 1)
	}
	if _, err := lg.call(t, ctx, "builtin", "gamesRecordEpisodes", map[string]any{"episodes": episodesArg(tooMany...)}); err == nil || !strings.Contains(err.Error(), "games_invalid_argument") {
		t.Fatalf("a batch over %d must be refused with games_invalid_argument, got %v", maxEpisodesPerCall, err)
	}
}

// The model is away: the episodes are still recorded and counted pending, and
// gamesEmbedPending gives them their vectors once it is back -- a page at a
// time, saying when there is more.
func TestLiveEpisodesRecordedWhileTheModelIsAwayAreEmbeddedLater(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	ctx := asOwner("user-games-pending-" + n)
	now := time.Now().UTC()
	lg.provider.space = failingSpace{testSpace{db: lg.db.DB, tableName: lg.table}}

	res := lg.must(t, ctx, "builtin", "gamesRecordEpisodes", map[string]any{"episodes": episodesArg(
		episode("e-bridge-"+n, "wren", "", "wren built a bridge", now, 5),
		episode("e-river-"+n, "wren", "", "wren crossed the river", now, 5),
	)})
	if asInt(res[0]["written"], 0) != 2 || asInt(res[0]["embedded"], -1) != 0 || asInt(res[0]["pending"], 0) != 2 {
		t.Fatalf("answer = %v, want 2 written, 0 embedded, 2 pending: a failed embedding is not a failed write", res[0])
	}
	// A FULL page of failures is not "more". Two episodes are pending and the
	// limit is one, so the page is full and there is more to embed; but nothing
	// on it embedded, and a caller looping while more would spin for as long as
	// the model is away.
	if res := lg.must(t, ctx, "builtin", "gamesEmbedPending", map[string]any{"limit": 1}); asInt(res[0]["embedded"], -1) != 0 || asInt(res[0]["failed"], 0) != 1 || res[0]["more"] != false {
		t.Fatalf("still away, full page: %v, want 0 embedded, 1 failed and no more", res[0])
	}
	if res := lg.must(t, ctx, "builtin", "gamesEmbedPending", nil); asInt(res[0]["embedded"], -1) != 0 || asInt(res[0]["failed"], 0) != 2 || res[0]["more"] != false {
		t.Fatalf("still away: %v, want 0 embedded, 2 failed and no more", res[0])
	}

	// Back again: a full page that embeds IS more.
	lg.provider.space = testSpace{db: lg.db.DB, tableName: lg.table}
	if res := lg.must(t, ctx, "builtin", "gamesEmbedPending", map[string]any{"limit": 1}); asInt(res[0]["embedded"], 0) != 1 || res[0]["more"] != true {
		t.Fatalf("first page: %v, want 1 embedded and more", res[0])
	}
	if res := lg.must(t, ctx, "builtin", "gamesEmbedPending", nil); asInt(res[0]["embedded"], 0) != 1 || res[0]["more"] != false {
		t.Fatalf("second page: %v, want the last one embedded and no more", res[0])
	}
	if got := lg.vectorsFor(t, "e-bridge-"+n, "e-river-"+n); got != 2 {
		t.Fatalf("%d vectors stored after the back-fill, want 2", got)
	}
	if res := lg.must(t, ctx, "builtin", "gamesEmbedPending", nil); asInt(res[0]["embedded"], -1) != 0 {
		t.Fatalf("nothing is pending any more, yet %v", res[0])
	}
}

// A batch is all or nothing for bad input: every entry is checked before the
// first is written, so a bad entry anywhere writes nothing. The bad one is the
// SECOND here, so an implementation that writes as it goes has already written
// the first. Each case is something the engine refuses only later, at the
// mutation, the storage gate or the relationship canonicalization, after the
// entries before it were recorded.
func TestLiveABatchWithABadEntryWritesNothing(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	now := time.Now().UTC()
	for i, tc := range []struct {
		name  string
		field string // the field the refusal must name
		bad   any    // its value in the second entry
	}{
		{"a kind outside the enum", "kind", "nonsense"},
		{"an importance above 10", "importance", 11},
		{"an episodeId with colons", "episodeId", "wren:tick:" + n},
		{"an episodeId with a NUL character", "episodeId", "wren\x00tick-" + n},
		{"a playerId that is another concept's id", "playerId", "v1:games:episode:x"},
		{"a summary that is a number", "summary", 42},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := asOwner(fmt.Sprintf("user-games-bad-%d-%s", i, n))
			firstID, secondID := fmt.Sprintf("e-first-%d-%s", i, n), fmt.Sprintf("e-second-%d-%s", i, n)
			first := episode(firstID, "wren", "", "the first entry is fine", now, 3)
			second := episode(secondID, "wren", "", "the second entry is not", now, 3)
			good, had := second[tc.field]
			second[tc.field] = tc.bad
			since := rfc3339(now.Add(-time.Minute))

			_, err := lg.call(t, ctx, "builtin", "gamesRecordEpisodes", map[string]any{"episodes": episodesArg(first, second)})
			if err == nil {
				t.Fatal("a batch with an invalid second entry was accepted")
			}
			if !strings.HasPrefix(err.Error(), "games_invalid_argument: ") || !strings.Contains(err.Error(), "episode 1 "+tc.field) {
				t.Errorf("error = %q, want games_invalid_argument naming episode 1 and %s", err, tc.field)
			}
			if rows := lg.must(t, ctx, "query", "gamesSessionEpisodes", map[string]any{"characterId": "wren", "since": since}); len(rows) != 0 {
				t.Errorf("the refused batch left %d episode(s) behind: %v", len(rows), rows)
			}
			if got := lg.vectorsFor(t, firstID, secondID); got != 0 {
				t.Errorf("the refused batch left %d vector(s) behind", got)
			}

			// The control: the same batch with the second entry corrected is
			// recorded whole, so the empty read above is the refusal's doing
			// and not a read that would show nothing either way.
			if had {
				second[tc.field] = good
			} else {
				delete(second, tc.field)
			}
			res := lg.must(t, ctx, "builtin", "gamesRecordEpisodes", map[string]any{"episodes": episodesArg(first, second)})
			if asInt(res[0]["written"], 0) != 2 {
				t.Fatalf("the corrected batch: %v, want 2 written", res[0])
			}
			if rows := lg.must(t, ctx, "query", "gamesSessionEpisodes", map[string]any{"characterId": "wren", "since": since}); len(rows) != 2 {
				t.Fatalf("the corrected batch is read back as %d episode(s), want 2: %v", len(rows), rows)
			}
		})
	}
}

// An episode's time is stored in ONE form, so the text comparison
// gamesSessionEpisodes makes on `at` is a time comparison. Four instants are
// written in four spellings (an offset, a plain Z, a long fraction, a lowercase
// t and z), in an order that is neither time order nor text order. Each comes
// back in the one stored form, in time order, and a `since` bound that falls
// between them cuts where the time does: the episode written with an offset
// began at 04:30 UTC, and as text its spelling would sort after every other.
func TestLiveEpisodesAreStoredInOneTimeFormSoTheyOrderAndFilterByTime(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	ctx := asOwner("user-games-time-" + n)
	character := "wren-time-" + n // this run's alone, so the read sees only these
	spellings := []struct{ id, written, stored string }{
		{"e-time-offset-" + n, "2026-10-18T10:00:00+05:30", "2026-10-18T04:30:00.000Z"},
		{"e-time-plain-" + n, "2026-10-18T05:00:00Z", "2026-10-18T05:00:00.000Z"},
		{"e-time-fraction-" + n, "2026-10-18T06:00:00.123456789123Z", "2026-10-18T06:00:00.123Z"},
		{"e-time-lower-" + n, "2026-10-18t07:00:00z", "2026-10-18T07:00:00.000Z"},
	}
	var entries []map[string]any
	for _, i := range []int{1, 3, 0, 2} { // plain, lowercase, offset, fraction
		e := episode(spellings[i].id, character, "", "when "+spellings[i].id, time.Now().UTC(), 3)
		e["at"] = spellings[i].written
		entries = append(entries, e)
	}
	if res := lg.must(t, ctx, "builtin", "gamesRecordEpisodes", map[string]any{"episodes": episodesArg(entries...)}); asInt(res[0]["written"], 0) != 4 {
		t.Fatalf("answer = %v, want 4 written", res[0])
	}

	read := func(since string, want ...int) {
		t.Helper()
		rows := lg.must(t, ctx, "query", "gamesSessionEpisodes", map[string]any{"characterId": character, "since": since})
		if len(rows) != len(want) {
			t.Fatalf("since %s read %d episode(s), want %d: %v", since, len(rows), len(want), rows)
		}
		for k, i := range want { // newest first
			if bare(trimmed(rows[k]["id"])) != spellings[i].id || trimmed(rows[k]["at"]) != spellings[i].stored {
				t.Errorf("since %s, row %d = %s at %s, want %s at %s", since, k, rows[k]["id"], rows[k]["at"], spellings[i].id, spellings[i].stored)
			}
		}
	}
	read("2026-10-18T04:00:00.000Z", 3, 2, 1, 0) // all four, newest first
	read("2026-10-18T04:45:00.000Z", 3, 2, 1)    // not the offset one, which began at 04:30
}

// The pending scan is the caller's alone. B's episode is pending, recorded while
// the model was away. Once the model is back, A's pass over "what has no
// vector" must not reach it, though it is the newest row in the database and A's
// page is as large as a page can be; and B's own pass does embed it, so the
// empty result is the owner check's doing and not a scan that finds nothing.
func TestLiveThePendingScanNeverReachesAnotherOwnersEpisodes(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	a, b := asOwner("user-games-scan-a-"+n), asOwner("user-games-scan-b-"+n)
	bEpisode := "e-scan-b-" + n

	lg.provider.space = failingSpace{testSpace{db: lg.db.DB, tableName: lg.table}}
	res := lg.must(t, b, "builtin", "gamesRecordEpisodes", map[string]any{"episodes": episodesArg(
		episode(bEpisode, "wren", "", "wren mended the fence", time.Now().UTC(), 5),
	)})
	if asInt(res[0]["written"], 0) != 1 || asInt(res[0]["pending"], 0) != 1 {
		t.Fatalf("answer = %v, want 1 written and 1 pending", res[0])
	}

	lg.provider.space = testSpace{db: lg.db.DB, tableName: lg.table}
	res = lg.must(t, a, "builtin", "gamesEmbedPending", map[string]any{"limit": maxPendingPerCall})
	if asInt(res[0]["embedded"], -1) != 0 || asInt(res[0]["failed"], -1) != 0 || res[0]["more"] != false {
		t.Fatalf("another owner's pass touched something: %v, want nothing embedded, nothing failed, no more", res[0])
	}
	if got := lg.vectorsFor(t, bEpisode); got != 0 {
		t.Fatalf("another owner's pass gave B's episode %d vector(s)", got)
	}

	res = lg.must(t, b, "builtin", "gamesEmbedPending", nil)
	if asInt(res[0]["embedded"], 0) != 1 {
		t.Fatalf("B's own pass: %v, want B's episode embedded", res[0])
	}
	if got := lg.vectorsFor(t, bEpisode); got != 1 {
		t.Fatalf("%d vector(s) after B's own pass, want 1", got)
	}
}

// An episode id is the identity of a row, and the ids of one cluster are one set.
// This pins what recording an id that already exists does, because a runtime that
// sends its memories again (a re-import, a retry after an error) depends on it:
//
//   - For the OWNER of the row it WRITES A NEW VERSION, and does not fail. The row
//     is the same row; a read answers its newest version; the versions before it
//     stay stored until retention sheds them. The version is a shallow merge over
//     the one before, so a send that leaves out the playerId keeps the one it had.
//     Through gamesRecordEpisodes the vector is rewritten in place, not added.
//   - For ANY OTHER OWNER it is REFUSED by the owned tier (row-authz), before
//     anything is written, and the row stays exactly as it was. The refusal names
//     the id and says it is not theirs, so it tells a caller that the id exists.
func TestLiveRecordingAnEpisodeIDAgainWritesANewVersionForItsOwnerAndIsRefusedForAnyoneElse(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	a, b := asOwner("user-games-reuse-a-"+n), asOwner("user-games-reuse-b-"+n)
	now := time.Now().UTC()
	id, character, player := "e-reuse-"+n, "wren-reuse-"+n, "p-reuse-"+n
	since := now.Add(-time.Minute).Format(episodeAtLayout)
	create := func(ctx context.Context, summary string, importance int, withPlayer bool) error {
		args := map[string]any{
			"episodeId": id, "characterId": character, "worldId": "meadow", "at": now.Format(episodeAtLayout),
			"kind": "chat", "summary": summary, "importance": importance,
		}
		if withPlayer {
			args["playerId"] = player
		}
		_, err := lg.call(t, ctx, "mutation", "createGamesEpisode", args)
		return err
	}
	readOne := func(ctx context.Context, want string) map[string]any {
		t.Helper()
		rows := lg.must(t, ctx, "query", "gamesSessionEpisodes", map[string]any{"characterId": character, "since": since})
		if len(rows) != 1 || rows[0]["summary"] != want {
			t.Fatalf("a read answers %v, want the one episode with the summary %q", rows, want)
		}
		return rows[0]
	}

	if err := create(a, "first", 3, true); err != nil {
		t.Fatalf("the first create: %v", err)
	}
	readOne(a, "first")

	// The owner records the same id again: a new version of the same row.
	if err := create(a, "second", 7, true); err != nil {
		t.Fatalf("a second create on an existing id, by its owner, must write a new version, got %v", err)
	}
	if got := lg.stored(t, conceptEpisode, id); got != 2 {
		t.Errorf("%d versions stored after the second create, want 2: the first stays", got)
	}
	if row := readOne(a, "second"); asInt(row["importance"], 0) != 7 {
		t.Errorf("a read answers %v, want the newest version", row)
	}

	// A send that leaves out the player keeps the one the row had.
	if err := create(a, "third", 7, false); err != nil {
		t.Fatalf("a create that leaves out the playerId: %v", err)
	}
	if row := readOne(a, "third"); row["playerId"] != canonical(conceptPlayer, player) {
		t.Errorf("a create without a playerId left the row's playerId as %v, want it kept as %s", row["playerId"], canonical(conceptPlayer, player))
	}

	// Through gamesRecordEpisodes: another version, and the vector is rewritten in place.
	for i := 0; i < 2; i++ {
		res := lg.must(t, a, "builtin", "gamesRecordEpisodes", map[string]any{"episodes": episodesArg(episode(id, character, player, "fourth", now, 5))})
		if asInt(res[0]["written"], 0) != 1 || asInt(res[0]["embedded"], 0) != 1 {
			t.Fatalf("recording the id again (%d): %v, want 1 written and 1 embedded", i, res[0])
		}
	}
	if got := lg.stored(t, conceptEpisode, id); got != 5 {
		t.Errorf("%d versions stored after five writes of one id, want 5", got)
	}
	if got := lg.vectorsFor(t, id); got != 1 {
		t.Errorf("%d vectors for the id, want 1: the vector is rewritten, not added", got)
	}
	readOne(a, "fourth")

	// Another owner: refused, and the row is as it was.
	for name, call := range map[string]func() error{
		"createGamesEpisode": func() error { return create(b, "taken", 9, true) },
		"gamesRecordEpisodes": func() error {
			_, err := lg.call(t, b, "builtin", "gamesRecordEpisodes", map[string]any{"episodes": episodesArg(episode(id, character, player, "taken", now, 9))})
			return err
		},
	} {
		err := call()
		if err == nil || !strings.Contains(err.Error(), "row-authz") || !strings.Contains(err.Error(), "is not this caller's") || !strings.Contains(err.Error(), id) {
			t.Errorf("%s by another owner: %v, want the owned tier's refusal naming the id", name, err)
		}
	}
	if got := lg.stored(t, conceptEpisode, id); got != 5 {
		t.Errorf("%d versions stored after another owner's refused writes, want the 5 there were", got)
	}
	if got := lg.vectorsFor(t, id); got != 1 {
		t.Errorf("%d vectors for the id after another owner's refused writes, want 1", got)
	}
	readOne(a, "fourth")
	if rows := lg.must(t, b, "query", "gamesSessionEpisodes", map[string]any{"characterId": character, "since": since}); len(rows) != 0 {
		t.Errorf("another owner reads %v of the row", rows)
	}
}
