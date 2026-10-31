package gamespack

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
)

// lookup.go -- gamesFindSkill: which skill to run instead of asking a model.
//
// THREE KEYS, ONE ANSWER SHAPE. By goal signature, after a model planned a
// goal: the trusted skill, else the canary one (work.DecideServe says canary
// serves with the model standing by). By trigger, for an event or an
// initiative with no model call at all: the most recently used trusted skill
// learned under it. By meaning, for an addressed message: the trusted skill
// whose goal text is nearest, at or above the near-match floor -- 0.82, the
// engine's own near-match threshold (integrations/planner).
//
// A LOOKUP FINDS THE CALLER'S OWN SKILLS, THE CLUSTER'S OWNER'S TOO. The two lists it
// picks from (a goal's skills, the trusted skills) are queries that name the caller's
// owner in their filter (queries.memql), so a cluster owner's page is its own: the owned
// tier's read arm hands that caller every owner's rows, and a page taken from them
// before the owner is compared would let the cluster owner's own skills fall off it.
// Both lists are still held to ownedBy -- ownerKey on both sides, as the ladder holds
// every skill it moves (moveLadder) -- as this half's own check on top of the query's,
// since another owner's trusted skill would outrank the caller's canary one, or be the
// newest use, and run a skill the caller never taught. The statement of a lookup by
// meaning names the owner itself. What is left of a page is the engine's size, 20 skills
// of a goal and 100 trusted skills, and only the owner's own skills can crowd each other
// off it.
//
// THE SKILL ANSWERED IS READ AGAIN, FRESH. The lists are the engine's cached reads, and a
// write made through one replica reaches another replica's cache when the invalidation
// broadcast does: a skill that failed twice and went back to shadow could be answered
// from a stale list, and run with no model asked, for that long. So the best of a list is
// read by id past the cache (freshSkill) and judged again, the next best is tried when it
// no longer qualifies, and what is answered is the version just read. gamesSkillById is
// not scoped to the caller (the owned tier answers the cluster's owner any skill by id),
// so that read is held to ownedBy too.
//
// A LOOKUP BY MEANING IS A DECISION WAITING ON A MODEL. The text is embedded on
// the caller's own lane and waited for no longer than a recall waits
// (embedRecallQuery, which also turns an embedder's panic into a failure): a model
// that is away or slow fails the lookup, saying so, and the runtime treats that as
// no match. The signature and the trigger need no model.

// defaultMinSimilarity is the near-match floor.
const defaultMinSimilarity = 0.82

// nearestSkillSQL finds the caller's trusted skill nearest a query vector.
// %s is the vector table. Skills of one goal that are all trusted embed the same words
// and are equally near, so ties are ordered by the skill's id, last, and every call
// answers the same one.
//
//	$1 canonical owner  $2 query vector
//
// staged-data: INDIFFERENT -- it pins v1:games:skill, an EMBEDDED pack
// concept; staging withholds promoted authored concepts only (memql#3980),
// and no authored bundle can declare one in this namespace.
const nearestSkillSQL = `
WITH latest AS (
    SELECT DISTINCT ON (id) id, payload
    FROM "MemoryNodes"
    WHERE concept = 'v1:games:skill'
      AND (payload->>'ownerUserId') = $1
    ORDER BY id, "createdAt" DESC
)
SELECT latest.id, 1 - (nv.embedding <=> $2::vector) AS similarity
FROM latest
JOIN %s nv ON nv.id = latest.id AND nv.vector_field = 'content'
WHERE latest.payload->>'rung' = 'trusted'
ORDER BY nv.embedding <=> $2::vector, latest.id
LIMIT 1
`

func (p *Provider) findSkill(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	owner, err := requireActor(ctx)
	if err != nil {
		return nil, err
	}
	key, value, err := lookupKey(args)
	if err != nil {
		return nil, err
	}
	switch key {
	case "goalSignature":
		// Every signature the engine computes is 64 lower-case hexadecimal digits, so any
		// other text can match no skill, and saying so is better than answering none.
		signature, err := signatureArg(args)
		if err != nil {
			return nil, err
		}
		rows, err := p.store.query(ctx, "gamesSkillsForSignature", map[string]any{"goalSignature": signature})
		if err != nil {
			return nil, err
		}
		// Trusted before canary, and what is still servable once the skill is read again.
		best, err := p.pickFresh(ctx, owner, ownRows(rows, owner), func(now map[string]any) bool {
			return bestServable([]map[string]any{now}) != nil
		})
		if err != nil || best == nil {
			return nil, err
		}
		return skillAnswer(best, 0)
	case "trigger":
		rows, err := p.store.query(ctx, "gamesTrustedSkills", nil)
		if err != nil {
			return nil, err
		}
		// Only a trusted skill runs with no model asked, whatever the read hands over, and
		// still under the trigger when it is read again; and the most recently used is a
		// comparison made here, not the first row of a sort that puts a skill with no
		// lastReplayAt first when it sorts descending.
		underTrigger := func(r map[string]any) bool {
			return trimmed(r["rung"]) == string(work.RungTrusted) && slices.Contains(asStrings(r["triggers"]), value)
		}
		var learnedUnder []map[string]any
		for _, r := range ownRows(rows, owner) {
			if underTrigger(r) {
				learnedUnder = append(learnedUnder, r)
			}
		}
		best, err := p.pickFresh(ctx, owner, learnedUnder, underTrigger)
		if err != nil || best == nil {
			return nil, err
		}
		return skillAnswer(best, 0)
	default:
		floor, err := lookupFloor(args)
		if err != nil {
			return nil, err
		}
		return p.nearestSkill(ctx, owner, value, floor)
	}
}

// lookupKey reads the one key a lookup is made by. Each of the three is a string or
// absent -- a number, a boolean or a list is refused, since read as its printed form it
// would look up a skill nobody asked for -- and exactly one is given: two keys are two
// questions and none is no question. A blank key is not given.
func lookupKey(args map[string]any) (key, value string, err error) {
	var given []string
	values := make(map[string]string, 3)
	for _, k := range []string{"goalSignature", "trigger", "text"} {
		v, err := traceText(args, k)
		if err != nil {
			return "", "", err
		}
		if v != "" {
			given = append(given, k)
			values[k] = v
		}
	}
	if len(given) != 1 {
		got := "none"
		if len(given) > 0 {
			got = strings.Join(given, ", ")
		}
		return "", "", fmt.Errorf("games_invalid_argument: pass exactly one of goalSignature, trigger and text, got %s", got)
	}
	return given[0], values[given[0]], nil
}

// lookupFloor reads the near-match floor of a lookup by meaning: the default when the
// caller names none, else a number above 0 and at most 1, which is the range of a cosine
// that means a skill is near. Anything else is refused -- an unreadable floor read as the
// default would answer for a caller who asked for another, and a floor of 0 or less would
// run the nearest skill whatever it is. Only a lookup by meaning calls it: the other two
// have no use for a floor, and do not judge one.
func lookupFloor(args map[string]any) (float64, error) {
	v := args["minSimilarity"]
	if v == nil {
		return defaultMinSimilarity, nil
	}
	f := asFloat(v, math.NaN()) // asFloat answers its fallback for a NaN, an infinity and what is no number
	if math.IsNaN(f) || f <= 0 || f > 1 {
		return 0, fmt.Errorf("games_invalid_argument: minSimilarity must be a number above 0 and at most 1, got %.40q", fmt.Sprint(v))
	}
	return f, nil
}

// ownRows keeps the rows that are owner's. The queries name the caller's owner in their
// filter, so from them it drops nothing; it is this half's own check on top of the query's
// (see the header), held by the tests over a store that hands over every owner's rows.
func ownRows(rows []map[string]any, owner string) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		if ownedBy(r, owner) {
			out = append(out, r)
		}
	}
	return out
}

// bestServable is the skill a signature lookup serves: trusted before canary,
// and within a rung the most recently used. Shadow and retired never serve.
func bestServable(rows []map[string]any) map[string]any {
	var best map[string]any
	rank := func(r map[string]any) int {
		switch trimmed(r["rung"]) {
		case string(work.RungTrusted):
			return 2
		case string(work.RungCanary):
			return 1
		}
		return 0
	}
	for _, r := range rows {
		if rank(r) == 0 {
			continue
		}
		if best == nil || rank(r) > rank(best) ||
			(rank(r) == rank(best) && timeOf(r["lastReplayAt"]).After(timeOf(best["lastReplayAt"]))) {
			best = r
		}
	}
	return best
}

// pickFresh answers the best of the candidates that is still what the lookup wants when it
// is read again. The candidates come from a list that may be a moment old: the best is read
// by id past the cache (freshSkill) and judged by wanted; one that no longer qualifies is
// dropped and the next best is tried; and what is answered is the version just read. None
// qualifying is none.
func (p *Provider) pickFresh(ctx context.Context, owner string, candidates []map[string]any, wanted func(now map[string]any) bool) (map[string]any, error) {
	rest := slices.Clone(candidates)
	for {
		best := bestServable(rest)
		if best == nil {
			return nil, nil
		}
		id := bare(trimmed(best["id"]))
		now, err := p.freshSkill(ctx, owner, id)
		if err != nil {
			return nil, err
		}
		if now != nil && wanted(now) {
			return now, nil
		}
		rest = slices.DeleteFunc(rest, func(r map[string]any) bool { return bare(trimmed(r["id"])) == id })
	}
}

// freshSkill reads one skill again, past the engine's result cache, and answers it only if it
// is the caller's: gamesSkillById answers a skill whoever's it is, so the owner is held here,
// ownerKey on both sides.
func (p *Provider) freshSkill(ctx context.Context, owner, id string) (map[string]any, error) {
	rows, err := p.store.query(memql.ContextWithFreshRead(ctx), "gamesSkillById", map[string]any{"skillId": bare(id)})
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if ownedBy(r, owner) {
			return r, nil
		}
	}
	return nil, nil
}

// matchesFloor reports whether a similarity the database answered reaches the floor. A
// similarity that is not a number is no similarity: pgvector answers NaN for the cosine
// of a vector with no direction (a stored zero vector, which the write path refuses and
// anything else could write), and a NaN is neither under the floor nor at it. So this asks
// for "at or above" and not for "not under": every comparison with a NaN is false, which
// makes `similarity >= floor` refuse it and `!(similarity < floor)` let it through. Recall
// counts the same row as no relevance. An infinity is none either, and could not be
// encoded in the answer.
func matchesFloor(similarity, floor float64) bool {
	return !math.IsInf(similarity, 0) && similarity >= floor
}

// nearestSkill embeds the text on the caller's own lane -- a reply is waiting
// on it -- and answers the trusted skill nearest it at or above the floor.
//
// The name of the vector table goes into the text of the statement, because an
// identifier cannot be a parameter, so only the name of a vector table does
// (isVectorTable, the one definition recall's join and forget's deletes ask too): any
// other name fails the lookup before the model is asked and before a statement is run.
func (p *Provider) nearestSkill(ctx context.Context, owner, text string, floor float64) ([]memorynodes.MemoryNode, error) {
	table, err := p.space.table(ctx)
	if err != nil {
		return nil, err
	}
	if table == "" {
		return nil, nil
	}
	if !isVectorTable(table) {
		return nil, fmt.Errorf("games: the embedder space names %.60q, which is no vector table's name, so it is not put into a statement and no skill is matched by meaning", table)
	}
	db := p.db()
	if db == nil {
		return nil, fmt.Errorf("games: no database handle on this node")
	}
	vec, err := p.embedRecallQuery(ctx, text)
	if err != nil {
		return nil, fmt.Errorf("games: the text could not be embedded, so no skill can be matched by meaning: %w", err)
	}
	var (
		id         string
		similarity float64
	)
	err = db.QueryRowContext(ctx, fmt.Sprintf(nearestSkillSQL, table), ownerKey(owner), vectorLiteral(vec)).Scan(&id, &similarity)
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("games: nearest skill: %w", err)
	}
	if !matchesFloor(similarity, floor) {
		return nil, nil
	}
	// The skill is read FRESH and judged again: the statement read the database, and an
	// answer is the verdict of the version it judged, not of one a cache remembers, nor
	// of one a ladder move or a forget has changed since.
	now, err := p.freshSkill(ctx, owner, id)
	if err != nil {
		return nil, err
	}
	if now == nil || trimmed(now["rung"]) != string(work.RungTrusted) {
		return nil, nil
	}
	return skillAnswer(now, similarity)
}

// skillAnswer renders a skill row plus the lookup's verdict. A similarity is reported
// when there is one: a lookup by meaning only answers at or above a floor that is above 0,
// so a similarity of 0 here means the lookup was made by something else.
func skillAnswer(row map[string]any, similarity float64) ([]memorynodes.MemoryNode, error) {
	rung, _ := work.ParseRung(trimmed(row["rung"]))
	serve, standby, reason := serveOf(rung)
	out := make(map[string]any, len(row)+4)
	for k, v := range row {
		out[k] = v
	}
	out["id"] = bare(trimmed(row["id"]))
	out["serve"] = serve
	out["standby"] = standby
	if reason != "" {
		out["serveReason"] = reason
	}
	if similarity > 0 {
		out["similarity"] = similarity
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	return []memorynodes.MemoryNode{{
		ID:        bare(trimmed(row["id"])),
		Concept:   "integration:games:skillMatch",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   raw,
	}}, nil
}
