package gamespack

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/core/num"
)

// episodes.go -- gamesRecordEpisodes and gamesEmbedPending.
//
// A BATCH IS ALL OR NOTHING FOR BAD INPUT. Every entry is checked (validateEpisode)
// against everything the write will check -- what createGamesEpisode declares,
// and the id shapes the storage gate and relationship canonicalization refuse --
// before the first is written, so an entry the write would refuse writes
// nothing instead of stranding the entries before it. What is left is the failure that cannot be
// seen coming, a database error after the check passed; that stays an
// internal error, because it is not a refusal.
//
// THE EMBEDDING RIDES THE BACKGROUND LANE. A runtime records memories in
// batches, off its game loop, and nobody waits on their vectors; a decision
// is waiting on recall's query vector. Stamping the write path's context with
// memql.ContextWithBackgroundLane counts every one of these embedding calls
// against the background rate bucket (ai_guard.go), so a burst of memories
// can never spend the interactive budget a decision needs.
//
// A FAILED EMBEDDING IS NOT A FAILED WRITE. The episode is recorded first and
// stays recorded; recall ranks it by recency and importance until
// gamesEmbedPending gives it a vector.

// maxEpisodesPerCall bounds one batch: a runtime flushes every few seconds,
// and a bigger batch is a backlog that should be split, not one statement.
const maxEpisodesPerCall = 50

// maxPendingPerCall bounds one gamesEmbedPending pass.
const maxPendingPerCall = 200

// What createGamesEpisode constrains, which validateEpisode mirrors.
const (
	maxEpisodeIDLength      = 64  // episodeId, characterId and worldId
	maxEpisodeSummaryLength = 400 // summary
	minEpisodeImportance    = 1
	maxEpisodeImportance    = 10
)

// episodeKinds is the closed set of episode kinds, spelled once in Go. The
// concept and the createGamesEpisode mutation each declare the same enum in
// the DSL, and episodes_test.go holds all three equal.
var episodeKinds = []string{"chat", "event", "activity", "session"}

// episodeAtLayout is the one form an episode's `at` is stored in: UTC, the
// milliseconds always written, the zone always Z. gamesSessionEpisodes compares
// `at` as text, and in this form text order is time order; timeOf reads it back.
const episodeAtLayout = "2006-01-02T15:04:05.000Z"

// episodeAtShape is an RFC 3339 date-time with a two-digit field everywhere, a
// dot (never a comma) before any fraction, and a zone that is Z or a numeric
// offset.
var episodeAtShape = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2})[Tt](\d{2}):(\d{2}):(\d{2})(?:\.\d+)?(?:[Zz]|[+-](\d{2}):(\d{2}))$`)

// isDateTime reports whether s is a datetime the episode concept's schema
// accepts.
//
// IT IS STRICTER THAN time.Parse(time.RFC3339), AND MUST NOT BE LOOSER THAN THE
// SCHEMA. The engine refuses a bad `at` when the row is written, by the
// schema's date-time format, and not when the mutation's arguments are checked;
// time.Parse takes things that format refuses (a comma before the fraction, a
// one-digit hour, an offset of +24:00 or +00:60), and a value accepted here but
// refused there strands the entries before it, which is what checking up front
// is for. Being narrower than the schema costs nothing real: it refuses a leap
// second (second 60), which Go's time package cannot represent and no runtime
// sends.
func isDateTime(s string) bool {
	m := episodeAtShape.FindStringSubmatch(s)
	if m == nil {
		return false
	}
	if _, err := time.Parse(time.DateOnly, m[1]); err != nil {
		return false // a day that is not on the calendar
	}
	// The hour, minute and second, then the zone's hour and minute when the
	// zone is an offset rather than Z (those two groups are empty for Z).
	for i, most := range []int{23, 59, 59, 23, 59} {
		if field := m[2+i]; field != "" {
			if n, _ := strconv.Atoi(field); n > most {
				return false
			}
		}
	}
	return true
}

// canonicalDateTime returns s in episodeAtLayout, or false when s is not a
// datetime the schema accepts. A lowercase t or z, an offset and a fraction
// longer than milliseconds are all accepted but never stored as written, so a
// string comparison of two stored times is a comparison of the times; the
// milliseconds are cut, not rounded, so a time is never moved later than it was.
func canonicalDateTime(s string) (string, bool) {
	if !isDateTime(s) {
		return "", false
	}
	t, err := time.Parse(time.RFC3339, strings.ToUpper(s)) // Go reads T and Z, not t and z
	if err != nil {
		return "", false
	}
	stored := t.UTC().Format(episodeAtLayout)
	// Moved to UTC, an offset can leave the years 0000 to 9999 (9999-12-31T23:59:59-23:59
	// is in year 10000), which the schema's date check cannot read back.
	if !isDateTime(stored) {
		return "", false
	}
	return stored, true
}

// episodeText reads one text field of an entry. A missing or null field is "";
// anything else that is not a JSON string is refused, because a number or an
// object would otherwise be stored as its printed form: a summary of 42 as "42",
// a playerId of 12345 as the player v1:games:player:12345.
func episodeText(index int, m map[string]any, key string) (string, error) {
	switch v := m[key].(type) {
	case nil:
		return "", nil
	case string:
		return strings.TrimSpace(v), nil
	}
	return "", fmt.Errorf("games_invalid_argument: episode %d %s must be a string", index, key)
}

// episodeEntry is one validated entry of a gamesRecordEpisodes batch: typed, so
// recordEpisodes needs no assertion to read it.
type episodeEntry struct {
	id, characterID, playerID, worldID, at, kind, summary string
	importance                                            int
}

// args are the arguments createGamesEpisode is called with. The player is left
// out when there is none.
func (e episodeEntry) args() map[string]any {
	w := map[string]any{
		"episodeId":   e.id,
		"characterId": e.characterID,
		"worldId":     e.worldID,
		"at":          e.at,
		"kind":        e.kind,
		"summary":     e.summary,
		"importance":  e.importance,
	}
	if e.playerID != "" {
		w["playerId"] = e.playerID
	}
	return w
}

// validateEpisode checks one entry of a gamesRecordEpisodes batch against what
// the write will check, and returns it typed; its args are what
// createGamesEpisode is called with.
//
// Lengths count characters, as the engine's @maxLength does, and values are
// trimmed before they are measured, because the trimmed value is what is
// written. Every refusal leads with games_invalid_argument and names the entry
// and the field; a summary is never echoed, because it is the player's words.
func validateEpisode(index int, raw any) (episodeEntry, error) {
	m, ok := raw.(map[string]any)
	if !ok {
		return episodeEntry{}, fmt.Errorf("games_invalid_argument: episode %d is not an object", index)
	}
	text := make(map[string]string, 6)
	for _, f := range []struct {
		key string
		max int // 0: no bound beyond the field's own check below
	}{
		{"episodeId", maxEpisodeIDLength},
		{"characterId", maxEpisodeIDLength},
		{"worldId", maxEpisodeIDLength},
		{"at", 0},
		{"kind", 0},
		{"summary", maxEpisodeSummaryLength},
	} {
		s, err := episodeText(index, m, f.key)
		if err != nil {
			return episodeEntry{}, err
		}
		if s == "" {
			return episodeEntry{}, fmt.Errorf("games_invalid_argument: episode %d has no %s", index, f.key)
		}
		if n := utf8.RuneCountInString(s); f.max > 0 && n > f.max {
			return episodeEntry{}, fmt.Errorf("games_invalid_argument: episode %d %s is %d characters, at most %d", index, f.key, n, f.max)
		}
		text[f.key] = s
	}
	// The two ids the engine checks only when it writes the row: the storage
	// gate refuses an episodeId with colons, and relationship canonicalization
	// refuses a playerId that is another concept's. Met mid-batch, either would
	// strand the entries before this one. An episodeId is also the one text that
	// the engine puts into its own SQL, where a NUL byte is a syntax error:
	// measured over every control character, U+0000 and the colon are the only
	// two code points the mutation refuses in an id, and it refuses none in the
	// other fields.
	if id.ValidateShortId(conceptEpisode, text["episodeId"]) != nil || strings.ContainsRune(text["episodeId"], 0) {
		return episodeEntry{}, fmt.Errorf("games_invalid_argument: episode %d episodeId must be a bare id with no colons (or %s:<id>) and no NUL character, got %.40q", index, conceptEpisode, text["episodeId"])
	}
	player, err := episodeText(index, m, "playerId")
	if err != nil {
		return episodeEntry{}, err
	}
	if id.ValidateShortId(conceptPlayer, player) != nil {
		return episodeEntry{}, fmt.Errorf("games_invalid_argument: episode %d playerId must be a bare id with no colons (or %s:<id>), got %.40q", index, conceptPlayer, player)
	}
	at, ok := canonicalDateTime(text["at"])
	if !ok {
		return episodeEntry{}, fmt.Errorf("games_invalid_argument: episode %d at must be an RFC 3339 datetime in the years 0000 to 9999 UTC, such as 2026-10-18T12:00:00Z, got %.40q", index, text["at"])
	}
	if !slices.Contains(episodeKinds, text["kind"]) {
		return episodeEntry{}, fmt.Errorf("games_invalid_argument: episode %d kind must be one of %s, got %.40q", index, strings.Join(episodeKinds, ", "), text["kind"])
	}
	if m["importance"] == nil {
		return episodeEntry{}, fmt.Errorf("games_invalid_argument: episode %d has no importance", index)
	}
	// A whole number, as the mutation's int argument is: a fraction, text and a
	// boolean are refused rather than rounded or read as zero.
	importance, whole := num.WholeInt64(asFloat(m["importance"], math.NaN()))
	if !whole || importance < minEpisodeImportance || importance > maxEpisodeImportance {
		return episodeEntry{}, fmt.Errorf("games_invalid_argument: episode %d importance must be a whole number from %d to %d", index, minEpisodeImportance, maxEpisodeImportance)
	}
	return episodeEntry{
		id:          text["episodeId"],
		characterID: text["characterId"],
		playerID:    player,
		worldID:     text["worldId"],
		at:          at,
		kind:        text["kind"],
		summary:     text["summary"],
		importance:  num.ClampInt64(importance),
	}, nil
}

func (p *Provider) recordEpisodes(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if _, err := requireActor(ctx); err != nil {
		return nil, err
	}
	list, ok := args["episodes"].([]any)
	if !ok || len(list) == 0 {
		return nil, fmt.Errorf("games_invalid_argument: episodes must be a non-empty list")
	}
	if len(list) > maxEpisodesPerCall {
		return nil, fmt.Errorf("games_invalid_argument: at most %d episodes per call, got %d", maxEpisodesPerCall, len(list))
	}
	entries := make([]episodeEntry, 0, len(list))
	for i, raw := range list {
		e, err := validateEpisode(i, raw)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	ids := make([]string, 0, len(entries))
	for i, e := range entries {
		if err := p.store.write(ctx, "createGamesEpisode", e.args()); err != nil {
			return nil, fmt.Errorf("games: episode %d (%s) was not recorded; the %d before it were: %w", i, e.id, i, err)
		}
		ids = append(ids, e.id)
	}
	bg := memql.ContextWithBackgroundLane(ctx)
	embedded := 0
	for _, e := range entries {
		if ctxDone(ctx) {
			break
		}
		if err := p.space.store(bg, canonical(conceptEpisode, e.id), conceptEpisode, e.summary); err != nil {
			p.logger().Info("games: an episode was recorded without a vector; gamesEmbedPending will retry it",
				"episodeId", e.id, "error", err)
			continue
		}
		embedded++
	}
	return answer("recordEpisodes", map[string]any{
		"written":  len(ids),
		"embedded": embedded,
		"pending":  len(ids) - embedded,
		"ids":      ids,
	})
}

// pendingVectorsSQL lists the caller's episodes and skills with no vector in
// one vector table, newest first. %s is the table.
//
//	$1 canonical owner  $2 limit
//
// staged-data: INDIFFERENT -- it pins v1:games:episode and v1:games:skill,
// EMBEDDED pack concepts; staging withholds promoted authored concepts only
// (memql#3980), and no authored bundle can declare one in this namespace.
const pendingVectorsSQL = `
WITH latest AS (
    SELECT DISTINCT ON (id) id, concept, "createdAt", payload
    FROM "MemoryNodes"
    WHERE concept IN ('v1:games:episode', 'v1:games:skill')
      AND (payload->>'ownerUserId') = $1
    ORDER BY id, "createdAt" DESC
)
SELECT latest.id, latest.concept, COALESCE(latest.payload->>'summary', latest.payload->>'goalText', '') AS text
FROM latest
WHERE %s
ORDER BY latest."createdAt" DESC
LIMIT $2
`

// pendingStatement fills the "has no vector" predicate. With no table yet,
// every row is pending.
//
// The name goes into the text of the statement (an identifier cannot be a parameter), so
// only the name of a vector table does -- isVectorTable, the one definition, which recall's
// builder, forget's deletes and the lookup ask too. Any other name is a space that cannot be
// scanned: an error, and no statement. It fails and does not fall back to "every row is
// pending", as recall's builder falls back to no relevance, because a pending scan that
// claimed every row had no vector would have the model embed rows that have one.
func pendingStatement(table string) (string, error) {
	if table == "" {
		return fmt.Sprintf(pendingVectorsSQL, "TRUE"), nil
	}
	if !isVectorTable(table) {
		return "", fmt.Errorf("games: the embedder space names %.60q, which is no vector table's name, so it is not put into a statement and nothing is embedded", table)
	}
	return fmt.Sprintf(pendingVectorsSQL,
		"NOT EXISTS (SELECT 1 FROM "+table+" nv WHERE nv.id = latest.id AND nv.vector_field = 'content')"), nil
}

func (p *Provider) embedPending(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	owner, err := requireActor(ctx)
	if err != nil {
		return nil, err
	}
	limit := clampInt(asInt(args["limit"], 25), 1, maxPendingPerCall)
	db := p.db()
	if db == nil {
		return nil, fmt.Errorf("games: no database handle on this node")
	}
	table, err := p.space.table(ctx)
	if err != nil {
		return nil, err
	}
	statement, err := pendingStatement(table)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, statement, ownerKey(owner), limit)
	if err != nil {
		return nil, fmt.Errorf("games: pending vectors: %w", err)
	}
	type pending struct{ id, concept, text string }
	var todo []pending
	for rows.Next() {
		var it pending
		if err := rows.Scan(&it.id, &it.concept, &it.text); err != nil {
			rows.Close()
			return nil, fmt.Errorf("games: pending vectors scan: %w", err)
		}
		if it.text != "" {
			todo = append(todo, it)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	bg := memql.ContextWithBackgroundLane(ctx)
	embedded, failed := 0, 0
	for _, it := range todo {
		if ctxDone(ctx) {
			break
		}
		if err := p.space.store(bg, it.id, it.concept, it.text); err != nil {
			failed++
			continue
		}
		embedded++
	}
	// MORE ONLY WHEN THE PAGE MADE PROGRESS. A full page where nothing embedded
	// is the model being away, and answering "more" to it would have a caller
	// that loops while more spin for as long as the model stays away.
	return answer("embedPending", map[string]any{
		"embedded": embedded,
		"failed":   failed,
		"more":     len(todo) == limit && embedded > 0,
	})
}
