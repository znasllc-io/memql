package gamespack

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/id"
)

// skills.go -- skill learning for an external runtime, the first half: record
// a trace and learn from it. The runtime executes every step; the engine
// stores the evidence and generalizes it. The ladder is ladder.go and
// ladder_calls.go, the lookup lookup.go.
//
// LEARNING IS ONCE PER (owner, signature, action sequence, generation). The
// first time two successful, non-negative, non-skill traces of one goal agree
// on their actions, they are generalized and the skill is created in shadow
// at a DETERMINISTIC id (SkillID), so two traces racing to teach one skill
// name one row -- and the learn gate serializes them anyway. Later traces do
// not rewrite the template: a version a person approved must stay the version
// that runs (a re-generalized template would be a new digest, which the
// promotion check already refuses). A retired skill is terminal; the goal
// coming back is learned anew at the next generation.
//
// A TRACE ID IS ONE OWNER'S. The id the caller names is the id of the row for the
// whole cluster, and createGamesPlanTrace runs under internal origin, which the
// owned tier's write guard lets through without comparing the row's owner: a second
// owner who sent the first owner's id would write a new version of the first owner's
// row and become its owner. So storeTrace takes the gate on the id, reads who holds
// the row now, and refuses a different owner before anything is written. A skill,
// the other row this file writes under internal origin, carries no such risk: its id
// is derived from its owner's key (SkillID), a hash another owner's call cannot land
// on, so the caller does not choose it and a second owner arrives at a different one.

// What createGamesPlanTrace declares as the length of its text arguments, which
// recordTrace checks before it takes a gate.
const (
	maxTraceIDLength   = 64  // traceId, characterId and trigger
	maxTraceTextLength = 200 // goalText and say
)

// traceText reads one text argument of a trace, and of the ladder's builtins. A missing
// or null argument is ""; a value that is not a string is refused, because the engine's
// own argument handling lets a number, a boolean or a list through to a string-typed
// argument and it would be stored as its printed form (a goalText of 42 as "42").
func traceText(args map[string]any, key string) (string, error) {
	switch v := args[key].(type) {
	case nil:
		return "", nil
	case string:
		return strings.TrimSpace(v), nil
	}
	return "", fmt.Errorf("games_invalid_argument: %s must be a string", key)
}

func (p *Provider) recordTrace(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	owner, err := requireActor(ctx)
	if err != nil {
		return nil, err
	}
	// EVERYTHING THE MUTATION WOULD REFUSE IS REFUSED HERE, with a code, before a gate is
	// taken (validateEpisode's rule): a text argument is a string, is within its
	// limit counted in characters after the trim (the field is named and the value never
	// echoed, since it is what a player or a character said), and the time is a
	// datetime. Left to the mutation each came back as an uncoded engine error, and an
	// offset in the time was stored as sent.
	text := make(map[string]string, 7)
	for _, f := range []struct {
		key string
		max int // 0: no bound of its own
	}{
		{"traceId", 0}, // its length is checked with its shape, below
		{"characterId", maxTraceIDLength},
		{"goalText", maxTraceTextLength},
		{"trigger", maxTraceIDLength},
		{"at", 0},
		{"say", maxTraceTextLength},
		{"skillId", 0},
	} {
		v, err := traceText(args, f.key)
		if err != nil {
			return nil, err
		}
		if n := utf8.RuneCountInString(v); f.max > 0 && n > f.max {
			return nil, fmt.Errorf("games_invalid_argument: %s is %d characters, at most %d", f.key, n, f.max)
		}
		text[f.key] = v
	}
	traceID, characterID, goalText, trigger := text["traceId"], text["characterId"], text["goalText"], text["trigger"]
	if traceID == "" || characterID == "" || goalText == "" || trigger == "" || text["at"] == "" {
		return nil, fmt.Errorf("games_invalid_argument: traceId, characterId, goalText, trigger and at are required")
	}
	// The id the claim below takes the gate on and looks up has to be the id the write
	// will use, so it is checked the way an episode's is: a bare id or the row's own
	// canonical id (never other colons), no NUL (a byte the database refuses in
	// a statement), and the length the mutation declares.
	if id.ValidateShortId(conceptPlanTrace, traceID) != nil || strings.ContainsRune(traceID, 0) || utf8.RuneCountInString(traceID) > maxTraceIDLength {
		return nil, fmt.Errorf("games_invalid_argument: traceId must be a bare id with no colons (or %s:<id>), no NUL character and at most %d characters, got %.40q", conceptPlanTrace, maxTraceIDLength, traceID)
	}
	// The time is stored in one fixed UTC form, as an episode's is: gamesLearningTraces
	// sorts it as text, the newest trace's words are the skill's, and an offset sorts by
	// its clock face (14:00+02:00, which is 12:00Z, sorted ahead of 12:30Z).
	at, ok := canonicalDateTime(text["at"])
	if !ok {
		return nil, fmt.Errorf("games_invalid_argument: at must be an RFC 3339 datetime in the years 0000 to 9999, such as 2026-10-18T12:00:00Z, got %.40q", text["at"])
	}
	// A skill run names its skill by a bare id or the skill's own canonical id; bare()
	// would take the short id out of ANY concept's id.
	if sk := text["skillId"]; sk != "" && (id.ValidateShortId(conceptSkill, sk) != nil || strings.ContainsRune(sk, 0)) {
		return nil, fmt.Errorf("games_invalid_argument: skillId must be a bare id with no colons (or %s:<id>) and no NUL character, got %.40q", conceptSkill, sk)
	}
	outcome := trimmed(args["outcome"])
	if outcome != "success" && outcome != "failure" && outcome != "abandoned" {
		return nil, fmt.Errorf("games_invalid_argument: outcome must be success, failure or abandoned, not %.40q", outcome)
	}
	reaction := trimmed(args["reaction"])
	if reaction == "" {
		reaction = "neutral"
	}
	if reaction != "positive" && reaction != "neutral" && reaction != "negative" {
		return nil, fmt.Errorf("games_invalid_argument: reaction must be positive, neutral or negative, not %.40q", reaction)
	}
	// Inputs the caller named are an object; anything else read as no inputs would
	// change the signature without a word.
	inputs := map[string]any{}
	switch v := args["inputs"].(type) {
	case nil:
	case map[string]any:
		if v != nil {
			inputs = v
		}
	default:
		return nil, fmt.Errorf("games_invalid_argument: inputs must be an object")
	}
	plan, err := parsePlan(args["steps"])
	if err != nil {
		return nil, err
	}
	if len(plan) == 0 {
		return nil, fmt.Errorf("games_invalid_argument: a trace has at least one step")
	}
	signature := GoalSignature(goalText, inputKeys(inputs))
	if supplied := trimmed(args["goalSignature"]); supplied != "" && supplied != signature {
		return nil, fmt.Errorf("games_signature_mismatch: the supplied goalSignature %s is not GoalSignature(goalText, input keys) = %s; check the caller's normalization", supplied, signature)
	}
	actions := Actions(plan)
	skillID := bare(text["skillId"])
	write := map[string]any{
		"traceId":       traceID,
		"characterId":   characterID,
		"goalText":      goalText,
		"goalSignature": signature,
		"trigger":       trigger,
		"inputs":        inputs,
		"steps":         args["steps"],
		"actions":       stringsAsAny(actions),
		"outcome":       outcome,
		"reaction":      reaction,
		"at":            at,
	}
	if say := text["say"]; say != "" {
		write["say"] = say
	}
	if skillID != "" {
		write["skillId"] = skillID
	}
	if err := p.storeTrace(ctx, owner, traceID, write); err != nil {
		return nil, err
	}
	learned := ""
	if outcome == "success" && reaction != "negative" && skillID == "" {
		if learned, err = p.learn(ctx, owner, signature, actions); err != nil {
			return nil, err
		}
	}
	return answer("recordTrace", map[string]any{
		"traceId":        traceID,
		"goalSignature":  signature,
		"learnedSkillId": learned,
	})
}

// storeTrace writes a validated trace for owner, once the trace id is known to be
// theirs: the gate on the id is taken, the row's holder is read FRESH, a different
// owner is refused, and only then is the row written, all before the gate is let go.
//
// THE GATE IS WHAT MAKES LOOKING AND WRITING ONE STEP. Two owners can each look while
// the id is still free; the second write is then a new version of a row it does not
// hold, unless the second look has to wait for the first write. The key is the BARE
// id, since a bare and a canonical spelling are one row, and it has no owner in it,
// since the whole question is two owners at one id. The gate is let go before the
// learner is asked (learn takes its own), so the two are never held together.
//
// The look is a raw statement and so never touches the result cache, which is what
// ContextWithFreshRead asks of an engine read; the context is marked all the same
// so that anything standing in for the statement is held to it.
//
// The refusal does not say whose the row is, and a failure to look refuses too:
// nothing is written on a guess.
func (p *Provider) storeTrace(ctx context.Context, owner, traceID string, write map[string]any) error {
	release, err := p.gate(ctx, "games-trace:"+bare(traceID))
	if err != nil {
		return fmt.Errorf("games: the trace id could not be locked, so the trace was not recorded; send it again: %w", err)
	}
	defer release()
	holder, found, err := p.rowHolder(memql.ContextWithFreshRead(ctx), conceptPlanTrace, canonical(conceptPlanTrace, traceID))
	if err != nil {
		return err
	}
	if found && ownerKey(holder) != ownerKey(owner) {
		return fmt.Errorf("games_invalid_argument: that trace id is taken; a trace id names one trace for the whole cluster, so send a fresh random one")
	}
	return p.store.createTrace(ctx, write)
}

// rowHolderSQL names who holds a row now: the owner its newest version names. It is
// the one read in this pack that is NOT scoped to the caller, because its question is
// whether a row is somebody else's; what it returns never leaves the pack.
//
//	$1 concept  $2 canonical row id
//
// staged-data: MUST-NOT-GATE -- a row a staged-data gate hid would read as free,
// and the write that follows would land on it: another owner's row, taken over.
const rowHolderSQL = `SELECT COALESCE(payload->>'ownerUserId', '')
FROM "MemoryNodes"
WHERE concept = $1 AND id = $2
ORDER BY "createdAt" DESC
LIMIT 1`

// rowHolder returns the owner of the newest version of the row (concept, rowID), and
// whether there is such a row. rowID is canonical.
func (p *Provider) rowHolder(ctx context.Context, concept, rowID string) (owner string, found bool, err error) {
	if p.holderOf != nil {
		return p.holderOf(ctx, concept, rowID)
	}
	return p.queryHolder(ctx, concept, rowID)
}

// queryHolder is rowHolder's statement: the database's answer, without the seam.
func (p *Provider) queryHolder(ctx context.Context, concept, rowID string) (owner string, found bool, err error) {
	var db *sql.DB
	if p.db != nil {
		db = p.db()
	}
	if db == nil {
		return "", false, fmt.Errorf("games: no database handle on this node")
	}
	err = db.QueryRowContext(ctx, rowHolderSQL, concept, rowID).Scan(&owner)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("games: who holds %s: %w", rowID, err)
	}
	return owner, true, nil
}

// learn creates the skill for (signature, actions) once two qualifying traces
// agree, and returns its id; "" when nothing was learned.
//
// A LOOK FIRST, WITHOUT THE GATE. Most qualifying traces arrive after the skill
// exists, and a live skill of these actions means there is nothing to learn; saying so
// with an ordinary read keeps those traces off the gate pool, whose few connections
// (four by default, each held by a waiter for up to six seconds) every trace's own gate
// is taken from too. The look may come from this node's cache. Stale, it can say "not
// yet" -- then the gate is taken and the question asked again, fresh, inside it -- or
// "live" about a skill that has just retired, which costs the next generation the
// cache's minute and nothing else.
//
// THE GATE IS HELD FOR THE READS AND THE WRITE AND LET GO BEFORE THE EMBEDDING. The
// embedding is a call to a model, which can take as long as a model takes; nothing about
// it needs the lock, and a skill that misses its vector is back-filled by gamesEmbedPending.
func (p *Provider) learn(ctx context.Context, owner, signature string, actions []string) (string, error) {
	live, _, err := p.coveringSkill(ctx, owner, signature, actions)
	if err != nil {
		return "", err
	}
	if live {
		return "", nil
	}

	release, err := p.gate(ctx, "games-learn:"+owner+":"+signature)
	if err != nil {
		return "", fmt.Errorf("games: learning could not be locked; the trace is recorded and will count next time: %w", err)
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)

	// A LIVE SKILL ALREADY COVERS THE PAIR, or the pair is learned anew at
	// the next generation: every sibling retired means the goal came back
	// after its skill went unused for the retirement window.
	live, generation, err := p.coveringSkill(fresh, owner, signature, actions)
	if err != nil {
		return "", err
	}
	if live {
		return "", nil
	}
	skillID := SkillID(owner, signature, actions, generation)
	rows, err := p.store.query(fresh, "gamesLearningTraces", map[string]any{"goalSignature": signature})
	if err != nil {
		return "", err
	}
	var (
		plans    [][]PlanStep
		traceIDs []string
		triggers []string
		goalText string
		say      string
		seenTrig = map[string]bool{}
	)
	for _, r := range rows {
		if !ownedBy(r, owner) || trimmed(r["skillId"]) != "" || !sameStrings(asStrings(r["actions"]), actions) {
			continue
		}
		plan, err := parsePlan(r["steps"])
		if err != nil {
			continue
		}
		plans = append(plans, plan)
		traceIDs = append(traceIDs, bare(trimmed(r["id"])))
		if goalText == "" {
			goalText, say = trimmed(r["goalText"]), trimmed(r["say"])
		}
		if t := trimmed(r["trigger"]); t != "" && !seenTrig[t] {
			seenTrig[t] = true
			triggers = append(triggers, t)
		}
	}
	if len(plans) < 2 {
		return "", nil
	}
	tmpl, err := Generalize(plans)
	if err != nil {
		p.logger().Info("games: traces of one goal and one action sequence did not generalize", "goalSignature", signature, "error", err)
		return "", nil
	}
	tmplMap, err := tmpl.AsMap()
	if err != nil {
		return "", err
	}
	sort.Strings(triggers)
	create := map[string]any{
		"skillId":        skillID,
		"goalText":       goalText,
		"goalSignature":  signature,
		"actions":        stringsAsAny(actions),
		"template":       tmplMap,
		"templateDigest": tmpl.Digest(),
		"triggers":       stringsAsAny(triggers),
		"learnedFrom":    stringsAsAny(traceIDs),
	}
	if say != "" {
		create["confirmation"] = say
	}
	if err := p.store.createSkill(ctx, create); err != nil {
		return "", err
	}
	release() // the skill is written; the model is asked without the gate
	if err := p.space.store(memql.ContextWithBackgroundLane(ctx), canonical(conceptSkill, skillID), conceptSkill, goalText); err != nil {
		p.logger().Info("games: a skill was learned without a goal vector; gamesEmbedPending will retry it", "skillId", skillID, "error", err)
	}
	return skillID, nil
}

// coveringSkill reads the skills of one signature and says whether a live one of these
// actions, the owner's, covers the pair already, and otherwise the generation the next
// skill of the pair would be: one more than the retired ones. A failed read is an error,
// never "no skill": taken for none it would write a live skill's one deterministic id
// again, which puts a skill the ladder has trusted back in shadow with nothing counted.
func (p *Provider) coveringSkill(ctx context.Context, owner, signature string, actions []string) (live bool, generation int, err error) {
	siblings, err := p.store.query(ctx, "gamesSkillsForSignature", map[string]any{"goalSignature": signature})
	if err != nil {
		return false, 0, err
	}
	generation = 1
	for _, s := range siblings {
		if !ownedBy(s, owner) || !sameStrings(asStrings(s["actions"]), actions) {
			continue
		}
		if trimmed(s["rung"]) != string(work.RungRetired) {
			return true, 0, nil
		}
		generation++
	}
	return false, generation, nil
}

// ownedBy reports whether a row read back is owner's. The queries that page name the
// caller's owner in their filter (queries.memql), so a cluster owner's page is its own and
// not a page shared with everyone's rows; this is the Go half's check on top of that, and
// the only one on a read that answers one row by id, which the owned tier's read arm for
// the cluster's owner answers whoever's the row is (gamesSkillById, gamesPlayerById).
// Learning is one owner's alone -- another owner's trace is not evidence for this owner's
// skill (its arguments and trace ids would end up in it), and another owner's skill is not
// this owner's live one (it would stop this owner from ever learning their own) -- and a
// ladder move is its owner's alone (moveLadder).
func ownedBy(row map[string]any, owner string) bool {
	return ownerKey(trimmed(row["ownerUserId"])) == ownerKey(owner)
}

func stringsAsAny(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}
