package gamespack

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// recall.go -- gamesRecall: what one character remembers that matters now.
//
// A PACK BUILTIN, NOT A CHANGE TO recall (integrations/harnessrecall). The
// score has a term recall cannot know -- importance, a field of THIS pack's
// concept -- and the scope is two of this pack's fields (the character's own
// episodes, and episodes about the player being addressed). Teaching the core
// builtin payload filters and a third weight is a platform design of its own,
// with callers in product bundles this repository cannot see; this one
// statement is the same shape as harnessrecall's, over one concept the pack
// owns.
//
// ONE STATEMENT, SCORED AND LIMITED IN SQL, because the 30-day window can hold
// tens of thousands of episodes and a decision waits on this read. The score
// is recallScore below, which the database test holds the statement to:
//
//	score = wRelevance x max(cosine, 0) + wRecency x 2^(-ageHours/halfLife) + wImportance x importance/10
//
// NO EMBEDDER, NO MODEL, STILL AN ANSWER. Relevance is a LEFT JOIN: an episode
// with no vector, a space nothing was ever stored in, or a query the model
// could not embed scores 0 relevance and is still ranked by recency and
// importance, and every episode row says for itself whether it was given a
// relevance: true only when the query was embedded and the episode has a vector
// in the current space that can be compared with it.
//
// A SLOW MODEL COSTS A DECISION AT MOST recallEmbedTimeout. The vector of the
// query is waited for that long, whether or not the embedder watches its context,
// and then recall answers as it does with the model away.

const ln2 = 0.6931471805599453

// What the weights and the half-life may be. The score is a weighted sum in float8
// and the recency term divides by the half-life, so a value no use needs can
// overflow either, and the database answers that with a raw out-of-range error.
// These bounds are far beyond any use and far inside float8, so the arithmetic
// cannot overflow, and a value outside them is refused before the statement runs.
const (
	maxRecallWeight        = 1000.0
	minRecallHalfLifeHours = 0.000001 // 3.6 milliseconds
	maxRecallHalfLifeHours = 87600.0  // ten years
)

// recallEmbedTimeout is how long a recall waits for the vector of its query. A
// decision waits on that read, so a model that is slow rather than away costs it
// this much and no more: past it, recall answers without relevance, as it does
// when the model is away.
const recallEmbedTimeout = 2 * time.Second

// recallEmbedDeadline is the bound on one recall's query embedding.
func (p *Provider) recallEmbedDeadline() time.Duration {
	if p.embedTimeout > 0 {
		return p.embedTimeout
	}
	return recallEmbedTimeout
}

// recallWeights are the score's tunables. The defaults are the runtime
// contract's numbers.
type recallWeights struct {
	relevance     float64
	recency       float64
	importance    float64
	halfLifeHours float64
}

func defaultRecallWeights() recallWeights {
	return recallWeights{relevance: 0.5, recency: 0.3, importance: 0.2, halfLifeHours: 72}
}

// recallScore is the score the statement computes, as a function over values.
// A negative cosine counts as 0, a negative age as 0, and a non-positive
// half-life turns the recency term off.
func recallScore(relevance, ageHours, importance float64, w recallWeights) float64 {
	if relevance < 0 {
		relevance = 0
	}
	if ageHours < 0 {
		ageHours = 0
	}
	recency := 0.0
	if w.halfLifeHours > 0 {
		recency = math.Exp(-ln2 * ageHours / w.halfLifeHours)
	}
	return w.relevance*relevance + w.recency*recency + w.importance*importance/10
}

// recallEpisodesSQL ranks one owner's episodes for one character.
//
//	$1 canonical owner  $2 characterId  $3 canonical playerId ('' = nobody)
//	$4 now  $5 halfLifeHours (zero or less: no recency)  $6 wRelevance
//	$7 wRecency  $8 wImportance  $9 k
//	$10 query vector (the joined variant only)
//
// %[1]s is the similarity of a row to the query (1 - cosine distance, NULL when
// the row has none) and %[2]s the join, filled by recallStatement.
//
// A SIMILARITY THAT IS NOT A NUMBER IS NO SIMILARITY. pgvector answers NaN for
// the cosine of a vector with no direction (a stored zero vector, which the write
// path refuses and anything else could write). PostgreSQL sorts NaN above every
// number and the answer cannot encode it, so such a row would lead the ranking and
// fail the recall. A row whose similarity is not finite, as one with no vector,
// has relevance 0, and says that none was available.
//
// THE DISTANCE IS COMPUTED ONCE A ROW. PostgreSQL inlines a common table
// expression that is used once into whatever reads it, so the similarity, which
// is read for the relevance, for whether the row had one and again in the score,
// was computed at every one of those places (eight times a row, 35 times in the
// plan text) at a cost that grows with the width of the vector. Over 10,000
// episodes of 1536-dimension vectors the statement took about 0.73 s that way and
// about 0.13 s with `scored` MATERIALIZED, which is the one place the distance is
// computed; `ranked` reads a stored column and needs no more. The plan is held to
// it (TestLiveRecallComputesEachVectorDistanceOnce).
//
// THE RECENCY TERM NEVER FAILS A ROW. An episode's time is the game's, which a
// runtime sets, and one statement reads every episode the owner holds, so one
// row that raised an error would end the owner's recall until it expired. Three
// things would have, and each is answered where the arithmetic is:
//
//   - extract(epoch ...) is NUMERIC, and exp() of a numeric far below zero is a
//     numeric no double can hold: the cast to float8 the score needs refuses it
//     ("out of range for type double precision") for an episode between about
//     nine and twenty-seven years old at the default half-life, or a month old at
//     a half-life of half an hour. The age is taken as float8 and the exponent
//     is floored at -700 (a recency near 1e-304, which is none).
//   - a half-life of zero or less is recency OFF, as recallScore says, not a
//     division by zero.
//   - an `at` the database cannot read counts as no recency. The write path
//     admits the years 0000 to 9999 and PostgreSQL has no year 0000, so an
//     episode at that year is storable and cannot be cast. pg_input_is_valid is
//     PostgreSQL 16, the cluster's major version.
//
// `at` is parsed here, never compared as text, so the form it is stored in
// (episodeAtLayout) does not matter to this statement.
//
// staged-data: INDIFFERENT -- the statement pins v1:games:episode, an EMBEDDED
// pack concept. Staged data is the tier that withholds a PROMOTED authored
// concept's rows (memql#3980); no authored bundle can declare a concept in a
// pack's namespace, so there is no staged row of this concept for a gate to
// withhold, and none it could wrongly drop.
const recallEpisodesSQL = `
WITH latest AS (
    SELECT DISTINCT ON (id) id, "createdAt", payload
    FROM "MemoryNodes"
    WHERE concept = 'v1:games:episode'
      AND (payload->>'ownerUserId') = $1
      AND ((payload->>'characterId') = $2 OR ($3 <> '' AND (payload->>'playerId') = $3))
    ORDER BY id, "createdAt" DESC
),
scored AS MATERIALIZED (
    SELECT latest.id, latest."createdAt", latest.payload,
           %[1]s AS similarity,
           CASE WHEN $5 > 0 AND pg_input_is_valid(latest.payload->>'at', 'timestamptz')
                THEN exp(GREATEST(-700, -0.6931471805599453 * GREATEST(extract(epoch FROM ($4::timestamptz - (latest.payload->>'at')::timestamptz))::float8, 0) / (3600 * $5::float8)))
                ELSE 0 END AS recency,
           COALESCE((latest.payload->>'importance')::float8, 1) / 10 AS importance
    FROM latest
    %[2]s
),
ranked AS (
    SELECT id, "createdAt", payload, recency, importance,
           COALESCE(similarity BETWEEN -1e308 AND 1e308, FALSE) AS available,
           CASE WHEN similarity BETWEEN -1e308 AND 1e308 THEN GREATEST(similarity, 0) ELSE 0 END AS relevance
    FROM scored
)
SELECT id, payload, relevance, recency, importance, available,
       $6 * relevance + $7 * recency + $8 * importance AS score
FROM ranked
ORDER BY score DESC, "createdAt" DESC
LIMIT $9
`

// recallStatement fills the similarity and the join for a vector table, or the
// no-relevance form, where no row has a similarity, when there is none. A name
// that is not the name of a vector table (isVectorTable) is none: it is the
// no-relevance form too, because the name is put into the statement as text.
func recallStatement(table string) string {
	if table == "" || !isVectorTable(table) {
		return fmt.Sprintf(recallEpisodesSQL, "NULL::float8", "")
	}
	return fmt.Sprintf(recallEpisodesSQL,
		"1 - (nv.embedding <=> $10::vector)",
		"LEFT JOIN "+table+" nv ON nv.id = latest.id AND nv.vector_field = 'content'")
}

// recallItem is one row of the answer.
type recallItem struct {
	Rank               int            `json:"rank"`
	Kind               string         `json:"kind"`
	ID                 string         `json:"id"`
	Text               string         `json:"text"`
	Score              float64        `json:"score"`
	Relevance          float64        `json:"relevance"`
	Recency            float64        `json:"recency"`
	Importance         float64        `json:"importance"`
	RelevanceAvailable bool           `json:"relevanceAvailable"`
	Row                map[string]any `json:"row"`
}

func (p *Provider) recall(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	owner, err := requireActor(ctx)
	if err != nil {
		return nil, err
	}
	characterID := trimmed(args["characterId"])
	worldID := trimmed(args["worldId"])
	text := trimmed(args["text"])
	if characterID == "" || worldID == "" || text == "" {
		return nil, fmt.Errorf("games_invalid_argument: characterId, worldId and text are required")
	}
	playerBare := bare(trimmed(args["playerId"]))
	k := clampInt(asInt(args["k"], 8), 1, 50)
	w := defaultRecallWeights()
	if w.relevance, err = recallWeight(args, "wRelevance", w.relevance); err != nil {
		return nil, err
	}
	if w.recency, err = recallWeight(args, "wRecency", w.recency); err != nil {
		return nil, err
	}
	if w.importance, err = recallWeight(args, "wImportance", w.importance); err != nil {
		return nil, err
	}
	if w.halfLifeHours, err = recallHalfLife(args, w.halfLifeHours); err != nil {
		return nil, err
	}
	now, err := recallAsOf(args["asOf"], p.clock())
	if err != nil {
		return nil, err
	}

	items, err := p.recallEpisodes(ctx, owner, characterID, canonical(conceptPlayer, playerBare), text, now, w, k)
	if err != nil {
		return nil, err
	}
	if playerBare != "" {
		prefs, err := p.store.query(ctx, "gamesPreferencesForPlayer", map[string]any{"playerId": playerBare})
		if err != nil {
			return nil, err
		}
		for _, r := range firstN(prefs, clampInt(asInt(args["preferences"], 3), 0, 20)) {
			items = append(items, recallItem{Kind: "preference", ID: bare(trimmed(r["id"])), Text: trimmed(r["subject"]), Score: asFloat(r["confidence"], 0), Row: bareRow(r)})
		}
		promises, err := p.store.query(ctx, "gamesOpenPromises", map[string]any{"characterId": characterID, "playerId": playerBare})
		if err != nil {
			return nil, err
		}
		for _, r := range firstN(promises, clampInt(asInt(args["promises"], 5), 0, 20)) {
			items = append(items, recallItem{Kind: "promise", ID: bare(trimmed(r["id"])), Text: trimmed(r["text"]), Row: bareRow(r)})
		}
	}
	places, err := p.store.query(ctx, "gamesPlacesInWorld", map[string]any{"worldId": worldID})
	if err != nil {
		return nil, err
	}
	for _, r := range firstN(places, clampInt(asInt(args["places"], 10), 0, 50)) {
		items = append(items, recallItem{Kind: "place", ID: bare(trimmed(r["id"])), Text: trimmed(r["name"]), Row: bareRow(r)})
	}
	return recallNodes(items)
}

// recallWeight reads one of the three weights: the default when the caller names
// none, else a number from 0 to maxRecallWeight. Anything else is refused, a
// non-finite number and a value that is no number included, where asFloat alone
// would have taken them for the default without a word.
func recallWeight(args map[string]any, key string, fallback float64) (float64, error) {
	v := args[key]
	if v == nil {
		return fallback, nil
	}
	f := asFloat(v, math.NaN()) // asFloat answers its fallback for a NaN, an infinity and what is no number
	if math.IsNaN(f) || f < 0 || f > maxRecallWeight {
		return 0, fmt.Errorf("games_invalid_argument: %s must be a number from 0 to %g, got %.40q", key, maxRecallWeight, fmt.Sprint(v))
	}
	return f, nil
}

// recallHalfLife reads the half-life in hours: the default when the caller names
// none; zero or less, which turns the recency term off, as it is (the statement
// does not divide by it then, so no bound is needed on that side); else a number
// from minRecallHalfLifeHours to maxRecallHalfLifeHours. Anything else is refused.
func recallHalfLife(args map[string]any, fallback float64) (float64, error) {
	v := args["halfLifeHours"]
	if v == nil {
		return fallback, nil
	}
	f := asFloat(v, math.NaN())
	if math.IsNaN(f) || f > maxRecallHalfLifeHours || (f > 0 && f < minRecallHalfLifeHours) {
		return 0, fmt.Errorf("games_invalid_argument: halfLifeHours must be a number, zero or less for no recency and otherwise from %s to %s, got %.40q",
			strconv.FormatFloat(minRecallHalfLifeHours, 'f', -1, 64), strconv.FormatFloat(maxRecallHalfLifeHours, 'f', -1, 64), fmt.Sprint(v))
	}
	return f, nil
}

// recallAsOf is the moment recall measures every episode's age from: the
// server's clock when the caller names none, else the instant asOf names. An asOf
// that cannot be read as a time, or lies outside the years 0001 to 9999 in UTC,
// is refused instead of measured from the server's clock, because a runtime with
// a clock of its own would get a quietly wrong ranking, and because PostgreSQL
// has no year 0000 to cast. Absent, null and blank are no asOf.
func recallAsOf(v any, clock time.Time) (time.Time, error) {
	switch x := v.(type) {
	case nil:
		return clock, nil
	case string:
		if strings.TrimSpace(x) == "" {
			return clock, nil
		}
	case time.Time:
		if x.IsZero() {
			return clock, nil
		}
	}
	t, ok := readTime(v)
	if !ok {
		return time.Time{}, fmt.Errorf("games_invalid_argument: asOf must be an RFC 3339 datetime such as 2026-10-18T12:00:00Z, got %.40q", fmt.Sprint(v))
	}
	if year := t.Year(); year < 1 || year > 9999 {
		return time.Time{}, fmt.Errorf("games_invalid_argument: asOf must be in the years 0001 to 9999 UTC, got %.40q", fmt.Sprint(v))
	}
	return t, nil
}

// recallEpisodes runs the one statement. The query vector is embedded on the
// caller's own lane: a decision is waiting on it.
func (p *Provider) recallEpisodes(ctx context.Context, owner, characterID, playerID, text string, now time.Time, w recallWeights, k int) ([]recallItem, error) {
	db := p.db()
	if db == nil {
		return nil, fmt.Errorf("games: no database handle on this node")
	}
	table, err := p.space.table(ctx)
	if err != nil {
		p.logger().Warn("games: recall could not resolve the embedder space; ranking without relevance", "error", err)
		table = ""
	}
	if table != "" && !isVectorTable(table) {
		p.logger().Warn("games: recall was handed a name that is no vector table and will not put it in a statement; ranking without relevance", "table", fmt.Sprintf("%.60q", table))
		table = ""
	}
	var vec []float32
	if table != "" {
		if vec, err = p.embedRecallQuery(ctx, text); err != nil {
			// A model that is away or slow is the ordinary state of a laptop that sleeps
			// and is logged as one. An embedder that panics is a bug, and is logged as
			// one, with the stack that shows where.
			var panicked *embedPanic
			if errors.As(err, &panicked) {
				p.logger().Warn("games: the embedder panicked on a recall query; ranking without relevance", "panic", fmt.Sprint(panicked.value), "stack", string(panicked.stack))
			} else {
				p.logger().Info("games: recall could not embed its query; ranking without relevance", "error", err)
			}
			table = ""
		}
	}
	params := []any{ownerKey(owner), characterID, playerID, rfc3339(now), w.halfLifeHours, w.relevance, w.recency, w.importance, k}
	if table != "" {
		params = append(params, vectorLiteral(vec))
	}
	rows, err := db.QueryContext(ctx, recallStatement(table), params...)
	if err != nil {
		return nil, fmt.Errorf("games: recall: %w", err)
	}
	defer rows.Close()
	return scanRecall(rows)
}

// embedPanic is what an embedder's panic becomes in embedRecallQuery: an error,
// carrying the stack that shows where, for the caller to log as the bug it is.
type embedPanic struct {
	value any
	stack []byte
}

func (e *embedPanic) Error() string { return fmt.Sprintf("embedding the query panicked: %v", e.value) }

// embedRecallQuery embeds the text of a recall on the caller's own lane and
// waits at most recallEmbedDeadline for it. The bound is this function's and not
// the embedder's: a provider that does not watch its context would otherwise hold
// the decision as long as it likes. At the deadline the embedding is abandoned,
// not awaited. It runs on its own copy of the space and sends to a channel with
// room for its answer, so an abandoned call finishes alone and leaves nothing
// behind, and a panic in it is an error here instead of the end of the node.
func (p *Provider) embedRecallQuery(ctx context.Context, text string) ([]float32, error) {
	bound := p.recallEmbedDeadline()
	ctx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()

	type embedded struct {
		vec []float32
		err error
	}
	space := p.space
	done := make(chan embedded, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- embedded{err: &embedPanic{value: r, stack: debug.Stack()}}
			}
		}()
		vec, err := space.embedQuery(ctx, text)
		done <- embedded{vec, err}
	}()
	select {
	case e := <-done:
		return e.vec, e.err
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("embedding the query did not finish within %s: %w", bound, ctx.Err())
		}
		return nil, ctx.Err()
	}
}

func scanRecall(rows *sql.Rows) ([]recallItem, error) {
	var out []recallItem
	for rows.Next() {
		var (
			id                                    string
			raw                                   json.RawMessage
			relevance, recency, importance, score float64
			available                             bool
		)
		if err := rows.Scan(&id, &raw, &relevance, &recency, &importance, &available, &score); err != nil {
			return nil, fmt.Errorf("games: recall scan: %w", err)
		}
		var row map[string]any
		if err := json.Unmarshal(raw, &row); err != nil {
			return nil, fmt.Errorf("games: recall row %s: %w", id, err)
		}
		row["id"] = bare(id)
		out = append(out, recallItem{
			Kind: "episode", ID: bare(id), Text: trimmed(row["summary"]), Score: score,
			Relevance: relevance, Recency: recency, Importance: importance,
			RelevanceAvailable: available, Row: row,
		})
	}
	return out, rows.Err()
}

// recallNodes renders the items as the builtin's rows. Each carries its
// RANK, 1 first, and a client orders by it: the order rows arrive in is the
// transport's, and the rank is the answer's.
func recallNodes(items []recallItem) ([]memorynodes.MemoryNode, error) {
	out := make([]memorynodes.MemoryNode, 0, len(items))
	now := time.Now().UTC()
	for i, it := range items {
		it.Rank = i + 1
		raw, err := json.Marshal(it)
		if err != nil {
			return nil, err
		}
		out = append(out, memorynodes.MemoryNode{
			ID:        it.ID,
			Concept:   "integration:games:memoryItem",
			Type:      memorynodes.NodeTypeObject,
			CreatedAt: now.Add(-time.Duration(i) * time.Microsecond),
			Payload:   raw,
		})
	}
	return out, nil
}

func firstN(rows []map[string]any, n int) []map[string]any {
	if n <= 0 {
		return nil
	}
	if len(rows) > n {
		return rows[:n]
	}
	return rows
}
