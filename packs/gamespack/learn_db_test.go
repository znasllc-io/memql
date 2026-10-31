package gamespack

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/metrics"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

func stepsArg(plan []PlanStep) []any {
	out := make([]any, len(plan))
	for i, s := range plan {
		out[i] = map[string]any{"action": s.Action, "args": s.Args, "ok": true, "durationMs": 1200}
	}
	return out
}

// trace is one gamesRecordTrace call for the torch goal.
func trace(id string, plan []PlanStep, outcome, reaction string) map[string]any {
	return map[string]any{
		"traceId": id, "characterId": "wren", "goalText": "Light up the area!",
		"trigger": "initiative", "inputs": map[string]any{"spot": "here"},
		"steps": stepsArg(plan), "outcome": outcome, "reaction": reaction,
		"at": rfc3339(time.Now().UTC()), "say": "Torches coming up!",
	}
}

// learnTorchSkill teaches the torch skill from two agreeing successful traces
// and returns its id.
func learnTorchSkill(t *testing.T, lg *liveGames, ctx context.Context, prefix string) string {
	t.Helper()
	lg.must(t, ctx, "builtin", "gamesRecordTrace", trace(prefix+"-a", torchPlan(4), "success", "neutral"))
	res := lg.must(t, ctx, "builtin", "gamesRecordTrace", trace(prefix+"-b", torchPlan(6), "success", "neutral"))
	skillID, _ := res[0]["learnedSkillId"].(string)
	if skillID == "" {
		t.Fatalf("two agreeing successful traces taught no skill: %v", res[0])
	}
	return skillID
}

// One trace teaches nothing, a failure and a disliked success do not count,
// and the second qualifying trace teaches the skill, in shadow, generalized.
func TestLiveTwoSuccessfulTracesTeachASkillInShadow(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	owner := "user-games-learn-" + n
	ctx := asOwner(owner)
	sig := GoalSignature("Light up the area!", []string{"spot"})

	first := lg.must(t, ctx, "builtin", "gamesRecordTrace", trace("t1-"+n, torchPlan(4), "success", "neutral"))
	if first[0]["learnedSkillId"] != "" || first[0]["goalSignature"] != sig {
		t.Fatalf("one trace taught a skill, or the signature is not the contract's: %v", first[0])
	}
	if got := lg.must(t, ctx, "builtin", "gamesRecordTrace", trace("t2-"+n, torchPlan(4), "failure", "neutral")); got[0]["learnedSkillId"] != "" {
		t.Fatalf("a failed plan taught a skill: %v", got[0])
	}
	if got := lg.must(t, ctx, "builtin", "gamesRecordTrace", trace("t3-"+n, torchPlan(6), "success", "negative")); got[0]["learnedSkillId"] != "" {
		t.Fatalf("a success the player disliked taught a skill: %v", got[0])
	}
	second := lg.must(t, ctx, "builtin", "gamesRecordTrace", trace("t4-"+n, torchPlan(6), "success", "positive"))
	skillID, _ := second[0]["learnedSkillId"].(string)
	if skillID != SkillID(owner, sig, []string{"collect", "craft", "placeTorches"}, 1) {
		t.Fatalf("two successful, non-negative traces must teach the skill at its derived id: %v", second[0])
	}

	skill := lg.must(t, ctx, "query", "gamesSkillById", map[string]any{"skillId": skillID})
	if len(skill) != 1 || skill[0]["state"] != "shadow" || skill[0]["rung"] != "shadow" {
		t.Fatalf("a learned skill starts in shadow: %v", skill)
	}
	tmpl, err := templateFrom(skill[0]["template"])
	if err != nil {
		t.Fatal(err)
	}
	if !sameStrings(tmpl.Holes, []string{"1.count", "2.count"}) || skill[0]["templateDigest"] != tmpl.Digest() {
		t.Fatalf("the counts differed between the traces and must be the holes: %+v (digest %v)", tmpl, skill[0]["templateDigest"])
	}
	if skill[0]["confirmation"] != "Torches coming up!" || !sameStrings(asStrings(skill[0]["triggers"]), []string{"initiative"}) {
		t.Fatalf("the skill keeps what the character said and the trigger it was learned under: %v", skill[0])
	}

	again := lg.must(t, ctx, "builtin", "gamesRecordTrace", trace("t5-"+n, torchPlan(9), "success", "neutral"))
	if again[0]["learnedSkillId"] != "" {
		t.Fatalf("a live skill already covers the goal; a third trace must not learn it again: %v", again[0])
	}
}

func TestLiveSignatureMismatchIsRefused(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	tr := trace("t-mismatch-"+n, torchPlan(4), "success", "neutral")
	tr["goalSignature"] = strings.Repeat("a", 64)
	if _, err := lg.call(t, asOwner("user-games-sig-"+n), "builtin", "gamesRecordTrace", tr); err == nil || !strings.Contains(err.Error(), "games_signature_mismatch") {
		t.Fatalf("a supplied signature that differs must be refused, got %v", err)
	}
}

// The ladder writes the ladder: a client cannot create or move a skill.
func TestLiveClientCannotWriteASkill(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	_, err := lg.call(t, asOwner("user-games-forge-"+n), "mutation", "recordGamesSkillLadder", map[string]any{
		"skillId": "gs-forged-" + n, "versionAt": rfc3339(time.Now()), "rung": "trusted", "state": "trusted",
		"shadowMatches": 0, "canaryMatches": 0, "distinctBindings": map[string]any{}, "failures": 0,
		"promotionApprovalId": "", "lastReason": "forged",
	})
	if err == nil {
		t.Fatal("a client-origin call reached recordGamesSkillLadder; it must be @serverOnly")
	}
}

// The internal-origin writers keep the caller's actor: the rows they write
// are the caller's, and nobody else reads them (call_origin's allowlist entry
// for this package rests on it).
func TestLiveInternalOriginWritesStayTheCallers(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	ownerA, ownerB := "user-games-own-a-"+n, "user-games-own-b-"+n
	a, b := asOwner(ownerA), asOwner(ownerB)
	sig := GoalSignature("Light up the area!", []string{"spot"})
	skillID := learnTorchSkill(t, lg, a, "o-"+n)
	skill := lg.must(t, a, "query", "gamesSkillById", map[string]any{"skillId": skillID})
	if len(skill) != 1 || ownerKey(trimmed(skill[0]["ownerUserId"])) != ownerKey(ownerA) {
		t.Fatalf("the learned skill must be owned by the caller %s: %v", ownerA, skill)
	}
	if rows := lg.must(t, b, "query", "gamesSkillById", map[string]any{"skillId": skillID}); len(rows) != 0 {
		t.Fatalf("another owner read the skill: %v", rows)
	}
	if rows := lg.must(t, a, "query", "gamesLearningTraces", map[string]any{"goalSignature": sig}); len(rows) < 2 {
		t.Fatalf("the caller reads %d of their traces, want 2", len(rows))
	}
	if rows := lg.must(t, b, "query", "gamesLearningTraces", map[string]any{"goalSignature": sig}); len(rows) != 0 {
		t.Fatalf("another owner read %d of the caller's traces", len(rows))
	}
}

// Two owners teach the same goal: two skills, each its owner's. Row ids are
// cluster-global, so a skill id that did not carry the owner would merge the
// second owner's internal-origin write into the first owner's row.
func TestLiveTwoOwnersLearnTheSameGoalApart(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	ownerA, ownerB := "user-games-same-a-"+n, "user-games-same-b-"+n
	a, b := asOwner(ownerA), asOwner(ownerB)
	skillA := learnTorchSkill(t, lg, a, "sa-"+n)
	skillB := learnTorchSkill(t, lg, b, "sb-"+n)
	if skillA == skillB {
		t.Fatalf("two owners' skills for one goal share the id %s", skillA)
	}
	for _, c := range []struct {
		ctx   context.Context
		owner string
		id    string
	}{{a, ownerA, skillA}, {b, ownerB, skillB}} {
		rows := lg.must(t, c.ctx, "query", "gamesSkillById", map[string]any{"skillId": c.id})
		if len(rows) != 1 || ownerKey(trimmed(rows[0]["ownerUserId"])) != ownerKey(c.owner) {
			t.Fatalf("skill %s must still be %s's alone: %v", c.id, c.owner, rows)
		}
	}
}

// ---- the concept guard: @serverWritten, proved as the clients it refuses ----
//
// TestLiveClientCannotWriteASkill above shows that a named writer is
// @serverOnly, and that is all it shows: @serverOnly closes only the mutation
// it sits on, and the owned tier lets a row's owner write it (and lets the
// cluster's owner write anything). A raw insert() never consults @serverOnly at
// all. What stops it is @serverWritten on the CONCEPT, checked at the engine's
// one write seam before the tier is asked, so the proof has to be a write that
// does not go through a @serverOnly mutation:
//
//   - a raw insert(), the public expression, for a new row and for a real one
//     (a rewrite: the owner promoting their own shadow skill to trusted);
//   - an update, which has no public expression in the language and reaches the
//     seam only as a lowered `update {}` mutation. The pack has none a client can
//     call (its one update of a skill is @serverOnly), so the test mounts a
//     throwaway domain with the two it deliberately lacks -- a client-reachable
//     update of a skill and of a plan trace -- and calls those.
//
// Every refusal is paired with the same write under internal origin, which is
// accepted: a refusal that the control does not make possible could be a bad
// payload, an unknown id or an unreachable construct.

const scratchDomain = "gamesclientwrite"

// mountScratchWriters mounts the throwaway domain for the length of the test. It
// has to run before newLiveGames, which loads every registered tree.
func mountScratchWriters(t *testing.T) {
	t.Helper()
	registerOnce.Do(func() { Register(Domain) })
	memqldsl.RegisterTree(scratchDomain, fstest.MapFS{
		"memql.toml": {Data: []byte("memql = \"1.0\"\nedition = \"2026\"\n")},
		"mutations.memql": {Data: []byte(`use games.concepts.{ skill, planTrace }

/// Scratch: the client-reachable update of a skill the pack deliberately does not have.
@actor
mutation skill scratchUpdateSkill {
  args {
    skillId     string!
    lastReason  string!
  }
  update {
    accept { lastReason }
    stamp {
      id: args.skillId
      ownerUserId: actor.userId
    }
  }
}

/// Scratch: the client-reachable update of a plan trace the pack deliberately does not have.
@actor
mutation planTrace scratchUpdateTrace {
  args {
    traceId  string!
    say      string!
  }
  update {
    accept { say }
    stamp {
      id: args.traceId
      ownerUserId: actor.userId
    }
  }
}
`)},
	})
	t.Cleanup(func() { memqldsl.UnregisterTree(scratchDomain) })
}

// clusterOwnerOf is the cluster's owner acting as name: the caller the owned
// tier's write guard waves through, so that nothing but @serverWritten refuses it.
func clusterOwnerOf(name string) context.Context {
	return auth.ContextWithAccess(asOwner(name), &auth.AccessContext{UserId: name, Role: auth.RoleOwner})
}

// rawInsert is the public insert() expression for a row of concept.
func rawInsert(concept, id string, payload map[string]any) string {
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf("insert(%s, id=%s, payload=%s)", langparser.QuoteString(concept), langparser.QuoteString(canonical(concept, id)), raw)
}

// seedTorchSkill writes a real skill for owner the way the pack writes one: the
// @serverOnly writer under internal origin.
func (lg *liveGames) seedTorchSkill(t *testing.T, owner, skillID string) {
	t.Helper()
	tmpl, err := Generalize([][]PlanStep{torchPlan(4), torchPlan(6)})
	if err != nil {
		t.Fatal(err)
	}
	tmplMap, err := tmpl.AsMap()
	if err != nil {
		t.Fatal(err)
	}
	lg.must(t, auth.ContextWithInternalOrigin(asOwner(owner)), "mutation", "createGamesSkill", map[string]any{
		"skillId": skillID, "goalText": "Light up the area!", "goalSignature": GoalSignature("Light up the area!", []string{"spot"}),
		"actions": []any{"collect", "craft", "placeTorches"}, "template": tmplMap, "templateDigest": tmpl.Digest(),
	})
}

func TestLiveClientCannotWriteASkillOrATraceByAnyRoute(t *testing.T) {
	mountScratchWriters(t)
	lg := newLiveGames(t)
	n := runID()
	owner := "user-games-guard-" + n
	now := time.Now().UTC()
	sig := GoalSignature("Light up the area!", []string{"spot"})
	const refusal = "@serverWritten requires internal origin"

	// What a forger would send: well-formed, so that only the guard can refuse it.
	forgedSkill := func(extra map[string]any) map[string]any {
		out := map[string]any{
			"ownerUserId": ownerKey(owner), "goalText": "forged", "goalSignature": strings.Repeat("c", 64),
			"actions": []any{"collect"}, "template": map[string]any{"steps": []any{}, "holes": []any{}},
			"templateDigest": strings.Repeat("d", 64), "learnedAt": rfc3339(now), "rung": "trusted", "state": "trusted",
			"shadowMatches": 0, "canaryMatches": 0, "failures": 0,
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	forgedTrace := func(extra map[string]any) map[string]any {
		out := map[string]any{
			"ownerUserId": ownerKey(owner), "characterId": "wren", "goalText": "forged", "goalSignature": strings.Repeat("c", 64),
			"trigger": "initiative", "steps": []any{map[string]any{"action": "look", "args": map[string]any{}, "ok": true, "durationMs": 1}},
			"actions": []any{"look"}, "outcome": "success", "reaction": "positive", "at": rfc3339(now),
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}

	// Real rows to rewrite: a shadow skill and a successful, neutral trace, written
	// as the pack writes them. A second pair for the controls, so that the
	// accepted server writes never touch the rows the refusals are checked on.
	skillID, controlSkill := "gs-guard-"+n, "gs-guard-control-"+n
	lg.seedTorchSkill(t, owner, skillID)
	lg.seedTorchSkill(t, owner, controlSkill)
	traceID, controlTrace := "t-guard-"+n, "t-guard-control-"+n
	for _, id := range []string{traceID, controlTrace} {
		lg.must(t, auth.ContextWithInternalOrigin(asOwner(owner)), "mutation", "createGamesPlanTrace", map[string]any{
			"traceId": id, "characterId": "wren", "goalText": "Light up the area!", "goalSignature": sig, "trigger": "initiative",
			"steps": stepsArg(torchPlan(4)), "actions": []any{"collect", "craft", "placeTorches"}, "outcome": "success",
			"reaction": "neutral", "at": rfc3339(now), "say": "Torches coming up!",
		})
	}
	before := map[string]int{
		conceptSkill + " " + skillID:     lg.stored(t, conceptSkill, skillID),
		conceptPlanTrace + " " + traceID: lg.stored(t, conceptPlanTrace, traceID),
	}
	if before[conceptSkill+" "+skillID] != 1 || before[conceptPlanTrace+" "+traceID] != 1 {
		t.Fatalf("the rows to rewrite were not seeded once each: %v", before)
	}

	// The named writers: @serverOnly, said in the words of that gate. Each is also
	// behind @serverWritten, which would refuse them in other words, so the
	// message is what shows the annotation is still there.
	for name, args := range map[string]map[string]any{
		"createGamesSkill": {
			"skillId": "gs-forged-" + n, "goalText": "forged", "goalSignature": sig, "actions": []any{"look"},
			"template": map[string]any{"steps": []any{}, "holes": []any{}}, "templateDigest": strings.Repeat("d", 64),
		},
		"createGamesPlanTrace": {
			"traceId": "t-forged-" + n, "characterId": "wren", "goalText": "forged", "goalSignature": sig, "trigger": "initiative",
			"steps": stepsArg(torchPlan(4)), "actions": []any{"look"}, "outcome": "success", "reaction": "positive", "at": rfc3339(now),
		},
		"recordGamesSkillLadder": {
			"skillId": skillID, "versionAt": rfc3339(now.Add(time.Hour)), "rung": "trusted", "state": "trusted", "shadowMatches": 0,
			"canaryMatches": 0, "distinctBindings": map[string]any{}, "failures": 0, "lastReason": "forged",
		},
	} {
		for who, ctx := range map[string]context.Context{"an owner": asOwner(owner), "the cluster owner": clusterOwnerOf(owner)} {
			if _, err := lg.call(t, ctx, "mutation", name, args); err == nil || !strings.Contains(err.Error(), "server-only") {
				t.Errorf("%s calling %s = %v, want the @serverOnly refusal", who, name, err)
			}
		}
	}

	for _, caller := range []struct {
		who, key string
		ctx      context.Context
	}{{"an owner", "own", asOwner(owner)}, {"the cluster owner", "clu", clusterOwnerOf(owner)}} {
		ctx := caller.ctx
		t.Run(caller.who, func(t *testing.T) {
			refused := func(label, concept string, err error) {
				t.Helper()
				if err == nil || !strings.Contains(err.Error(), concept+": "+refusal) {
					t.Errorf("%s: %v, want %q", label, err, concept+": "+refusal)
				}
			}

			// A raw insert() of a new row: nothing is stored.
			newSkill, newTrace := "gs-raw-"+caller.key+"-"+n, "t-raw-"+caller.key+"-"+n
			_, err := lg.eng.Execute(ctx, rawInsert(conceptSkill, newSkill, forgedSkill(nil)))
			refused("raw insert of a new skill", conceptSkill, err)
			_, err = lg.eng.Execute(ctx, rawInsert(conceptPlanTrace, newTrace, forgedTrace(nil)))
			refused("raw insert of a new plan trace", conceptPlanTrace, err)
			if got := lg.stored(t, conceptSkill, newSkill) + lg.stored(t, conceptPlanTrace, newTrace); got != 0 {
				t.Errorf("the refused inserts stored %d rows", got)
			}

			// A raw insert() onto a real row is a rewrite of it: an owner promoting
			// their own shadow skill, or turning a trace they recorded into a
			// better one.
			_, err = lg.eng.Execute(ctx, rawInsert(conceptSkill, skillID, map[string]any{"rung": "trusted", "state": "trusted", "lastReason": "forged"}))
			refused("raw rewrite of a real skill", conceptSkill, err)
			_, err = lg.eng.Execute(ctx, rawInsert(conceptPlanTrace, traceID, map[string]any{"reaction": "positive", "say": "forged"}))
			refused("raw rewrite of a real plan trace", conceptPlanTrace, err)

			// An update (through a mutation only this test has): the other verb of
			// the same seam.
			_, err = lg.call(t, ctx, "mutation", "scratchUpdateSkill", map[string]any{"skillId": skillID, "lastReason": "forged"})
			refused("update of a real skill", conceptSkill, err)
			_, err = lg.call(t, ctx, "mutation", "scratchUpdateTrace", map[string]any{"traceId": traceID, "say": "forged"})
			refused("update of a real plan trace", conceptPlanTrace, err)

			// ...and the rows are as they were.
			if got := lg.stored(t, conceptSkill, skillID); got != before[conceptSkill+" "+skillID] {
				t.Errorf("the skill has %d stored versions, want %d", got, before[conceptSkill+" "+skillID])
			}
			if got := lg.stored(t, conceptPlanTrace, traceID); got != before[conceptPlanTrace+" "+traceID] {
				t.Errorf("the plan trace has %d stored versions, want %d", got, before[conceptPlanTrace+" "+traceID])
			}
			skill := lg.must(t, asOwner(owner), "query", "gamesSkillById", map[string]any{"skillId": skillID})
			if len(skill) != 1 || skill[0]["rung"] != "shadow" || skill[0]["state"] != "shadow" || strings.Contains(trimmed(skill[0]["lastReason"]), "forged") {
				t.Errorf("the skill moved: %v", skill)
			}
			for _, row := range lg.must(t, asOwner(owner), "query", "gamesLearningTraces", map[string]any{"goalSignature": sig}) {
				if bare(trimmed(row["id"])) == traceID && (row["reaction"] != "neutral" || row["say"] != "Torches coming up!") {
					t.Errorf("the plan trace changed: %v", row)
				}
			}
		})
	}

	// The control: under internal origin the same writes are accepted -- a raw
	// insert() of a new row, and the same two updates onto rows of their own -- so
	// each refusal above was the guard's.
	internal := auth.ContextWithInternalOrigin(asOwner(owner))
	if _, err := lg.eng.Execute(internal, rawInsert(conceptSkill, "gs-raw-control-"+n, forgedSkill(nil))); err != nil {
		t.Errorf("a raw insert of a skill under internal origin: %v", err)
	}
	if _, err := lg.eng.Execute(internal, rawInsert(conceptPlanTrace, "t-raw-control-"+n, forgedTrace(nil))); err != nil {
		t.Errorf("a raw insert of a plan trace under internal origin: %v", err)
	}
	if _, err := lg.call(t, internal, "mutation", "scratchUpdateSkill", map[string]any{"skillId": controlSkill, "lastReason": "written by the server"}); err != nil {
		t.Errorf("the scratch skill update under internal origin: %v", err)
	}
	if _, err := lg.call(t, internal, "mutation", "scratchUpdateTrace", map[string]any{"traceId": controlTrace, "say": "written by the server"}); err != nil {
		t.Errorf("the scratch trace update under internal origin: %v", err)
	}
	if got := lg.stored(t, conceptSkill, controlSkill); got != 2 {
		t.Errorf("the control skill has %d versions, want its creation and the accepted update", got)
	}
	if got := lg.stored(t, conceptPlanTrace, controlTrace); got != 2 {
		t.Errorf("the control plan trace has %d versions, want its creation and the accepted update", got)
	}
}

// ---- learning is one owner's ----

// sortedCopy returns a sorted copy of in.
func sortedCopy(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return out
}

// The owned tier's read arm for the cluster's owner returns EVERY owner's rows,
// and learning is one owner's alone. Left to the reads, a cluster owner who records
// a trace would generalize their own and another owner's plans into one skill
// (copying the other owner's arguments and trace ids into it), and would be
// told "a live skill already covers this" by another owner's skill, and so never
// learn their own. The Go half compares each row's owner with the caller's.
func TestLiveLearningIsOneOwnersAloneEvenForTheClusterOwner(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	aName, cName := "user-games-learn-a-"+n, "user-games-learn-c-"+n
	a, c := asOwner(aName), clusterOwnerOf(cName)
	actions := []string{"collect", "craft", "placeTorches"}

	// goal returns the torch plan for a goal, as another owner would run it: the
	// block it collects differs, which is exactly what must never become a hole
	// of the cluster owner's skill.
	goal := func(text string) func(id, block string, count float64) map[string]any {
		return func(id, block string, count float64) map[string]any {
			plan := torchPlan(count)
			plan[0].Args["block"] = block
			tr := trace(id, plan, "success", "neutral")
			tr["goalText"] = text
			return tr
		}
	}
	record := func(ctx context.Context, tr map[string]any) string { return lg.record(t, ctx, tr) }

	// Their traces. Another owner has recorded one qualifying trace of the goal:
	// not a skill yet, and not evidence the cluster owner may count.
	traces := goal("Dig a trench!")
	sigTrench := GoalSignature("Dig a trench!", []string{"spot"})
	if got := record(a, traces("a1-"+n, "iron", 9)); got != "" {
		t.Fatalf("one trace taught %s", got)
	}
	if got := record(c, traces("c1-"+n, "coal", 4)); got != "" {
		t.Fatalf("the cluster owner's first trace learned %s from another owner's plan", got)
	}
	skillID := record(c, traces("c2-"+n, "coal", 6))
	if want := SkillID(cName, sigTrench, actions, 1); skillID != want {
		t.Fatalf("the cluster owner's second trace learned %q, want %s", skillID, want)
	}
	skill := lg.must(t, c, "query", "gamesSkillById", map[string]any{"skillId": skillID})
	if len(skill) != 1 || ownerKey(trimmed(skill[0]["ownerUserId"])) != ownerKey(cName) {
		t.Fatalf("the skill is not the cluster owner's: %v", skill)
	}
	if got := asStrings(skill[0]["learnedFrom"]); !sameStrings(sortedCopy(got), []string{"c1-" + n, "c2-" + n}) {
		t.Fatalf("learnedFrom = %v, want the cluster owner's two traces and no one else's", got)
	}
	tmpl, err := templateFrom(skill[0]["template"])
	if err != nil {
		t.Fatal(err)
	}
	if !sameStrings(tmpl.Holes, []string{"1.count", "2.count"}) || tmpl.Steps[0].Args["block"].Const != "coal" {
		t.Fatalf("another owner's plan reached the template (collect.block must stay the constant coal): %+v", tmpl)
	}

	// Their skills. Another owner already has a live skill for another goal; the
	// cluster owner's traces of that goal still teach the cluster owner's own.
	skills := goal("Fill the chest!")
	sigChest := GoalSignature("Fill the chest!", []string{"spot"})
	theirs := record(a, skills("a2-"+n, "iron", 4))
	if theirs != "" {
		t.Fatalf("one trace taught %s", theirs)
	}
	theirs = record(a, skills("a3-"+n, "iron", 6))
	if theirs != SkillID(aName, sigChest, actions, 1) {
		t.Fatalf("the other owner's skill was not learned: %q", theirs)
	}
	record(c, skills("c3-"+n, "coal", 4))
	mine := record(c, skills("c4-"+n, "coal", 6))
	if want := SkillID(cName, sigChest, actions, 1); mine != want {
		t.Fatalf("another owner's live skill stopped the cluster owner from learning their own: got %q, want %s", mine, want)
	}
	if got := lg.stored(t, conceptSkill, theirs); got != 1 {
		t.Fatalf("the other owner's skill has %d versions, want the one it was learned as", got)
	}
}

// ---- helpers for the tests below ----

// record sends one trace and returns the id of the skill it taught, or "".
func (lg *liveGames) record(t *testing.T, ctx context.Context, tr map[string]any) string {
	t.Helper()
	got, _ := lg.must(t, ctx, "builtin", "gamesRecordTrace", tr)[0]["learnedSkillId"].(string)
	return got
}

// replica is another node of the cluster over the same database: its own engine
// (so its own result cache), its own provider and its own gate pool. A test
// process runs no mesh, so what one replica writes is announced to the other by
// nothing -- the window a cross-replica read-modify-write lives in, held open.
func (lg *liveGames) replica(t *testing.T) *liveGames {
	t.Helper()
	eng, err := memql.New(lg.db)
	if err != nil {
		t.Fatalf("engine New: %v", err)
	}
	eng.Logger = lg.eng.Logger
	if err := eng.Init(memoryNodes.DefaultRegistry()); err != nil {
		t.Fatalf("engine.Init: %v", err)
	}
	gates, err := database.NewGatePool(dbtest.DSN(), 0)
	if err != nil {
		t.Fatalf("NewGatePool: %v", err)
	}
	t.Cleanup(func() { _ = gates.Close() })
	r := &liveGames{eng: eng, db: lg.db, table: lg.table}
	r.provider = &Provider{
		store:      engineStore{engine: eng},
		space:      testSpace{db: lg.db.DB, tableName: lg.table},
		db:         func() *sql.DB { return lg.db.DB },
		gates:      func() *sql.DB { return gates },
		invalidate: eng.InvalidateCacheForConcept,
		log:        eng.Logger,
		now:        time.Now,
	}
	if err := eng.RegisterIntegration(r.provider); err != nil {
		t.Fatalf("RegisterIntegration: %v", err)
	}
	return r
}

// warmed reads a query again and again until one read is answered from this
// replica's result cache. The cache is Ristretto, which admits a key it has not
// seen before only once it has been asked for enough (the engine's own
// fresh_read_db_test polls for the same reason), so how many reads that takes is
// not a number to hard-code. The reads before the hit are not evidence of
// anything; the hit is.
func (lg *liveGames) warmed(t *testing.T, ctx context.Context, query string, args map[string]any) {
	t.Helper()
	// Two minutes, not thirty seconds: the number of reads it takes is fixed (about four
	// hundred) and what each costs is not, and on a machine that is running other
	// tests it was measured at ten times what it costs alone.
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		hits := metrics.ResultCacheQueryReadValue(query, true)
		lg.must(t, ctx, "query", query, args)
		if metrics.ResultCacheQueryReadValue(query, true) > hits {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("no read of %s was answered from the replica's result cache within 2m, so the test could never hold its premise", query)
}

// execute runs one rendered construct without a *testing.T, for a goroutine.
func (lg *liveGames) execute(ctx context.Context, kind, name string, args map[string]any) ([]map[string]any, error) {
	call, err := langparser.RenderCall(name, compact(args))
	if err != nil {
		return nil, err
	}
	res, err := lg.eng.Execute(ctx, kind+" "+call)
	if err != nil {
		return nil, err
	}
	return memql.MaterializeRows(res), nil
}

// skillsOf reads the skills of one signature as the owner.
func (lg *liveGames) skillsOf(t *testing.T, ctx context.Context, sig string) []map[string]any {
	t.Helper()
	return lg.must(t, ctx, "query", "gamesSkillsForSignature", map[string]any{"goalSignature": sig})
}

// ---- across replicas ----

// Another replica learned the skill a moment ago and this replica's cache has
// not heard of it: the second replica must not learn it again. A second learn is
// a second insert onto the skill's one deterministic id, which is a new version
// that puts the skill back in shadow with nothing counted -- whatever the first
// replica's ladder had made of it in between.
//
// The premise is OBSERVED, not assumed: the replica's cache is warmed until it
// answers from it, and its ordinary read is shown to still say "no skill" after
// the other replica learned one. Only then does the learner's read mean anything.
func TestLiveAReplicaWithAStaleCacheDoesNotLearnTheSkillAgain(t *testing.T) {
	here := newLiveGames(t)
	there := here.replica(t)
	n := runID()
	owner := "user-games-stale-" + n
	ctx := asOwner(owner)
	sig := GoalSignature("Light up the area!", []string{"spot"})

	here.warmed(t, ctx, "gamesSkillsForSignature", map[string]any{"goalSignature": sig})
	skillID := learnTorchSkill(t, there, ctx, "stale-"+n)
	if got := here.skillsOf(t, ctx, sig); len(got) != 0 {
		t.Fatalf("the premise failed: this replica's cache was evicted by the other replica's write and reads %d skills, so the test proves nothing", len(got))
	}
	if got := here.skillsOf(t, memql.ContextWithFreshRead(ctx), sig); len(got) != 1 {
		t.Fatalf("the control failed: a fresh read sees %d skills, want the one the other replica learned", len(got))
	}

	third := here.record(t, ctx, trace("stale-"+n+"-c", torchPlan(9), "success", "neutral"))
	if third != "" {
		t.Fatalf("the replica learned %q from a cache that had not heard of the skill, a second time", third)
	}
	if got := here.stored(t, conceptSkill, skillID); got != 1 {
		t.Fatalf("the skill has %d stored versions, want the one it was learned as", got)
	}
}

// Traces of one goal arrive on two replicas at once, as they do when a runtime
// retries or two characters finish the same job together. Each trace is written
// first, so each learner sees the pair; the learn gate is what lets exactly one of
// them teach the skill and the rest find it taught. Without the gate several
// learners read "no skill yet" together and each inserts onto the one id.
func TestLiveRacingTracesTeachTheSkillOnce(t *testing.T) {
	replicas := []*liveGames{newLiveGames(t)}
	replicas = append(replicas, replicas[0].replica(t))
	n := runID()
	owner := "user-games-race-" + n
	ctx := asOwner(owner)
	sig := GoalSignature("Light up the area!", []string{"spot"})

	if got := replicas[0].record(t, ctx, trace("race-0-"+n, torchPlan(4), "success", "neutral")); got != "" {
		t.Fatalf("one trace taught %q", got)
	}
	const racers = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		learned []string
		failed  []error
		start   = make(chan struct{})
	)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			rows, err := replicas[i%len(replicas)].execute(ctx, "builtin", "gamesRecordTrace", trace(fmt.Sprintf("race-%d-%s", i+1, n), torchPlan(float64(5+i)), "success", "neutral"))
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failed = append(failed, err)
				return
			}
			if id, _ := rows[0]["learnedSkillId"].(string); id != "" {
				learned = append(learned, id)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if len(failed) != 0 {
		t.Fatalf("%d of %d racing traces failed: %v", len(failed), racers, failed)
	}
	want := SkillID(owner, sig, []string{"collect", "craft", "placeTorches"}, 1)
	if len(learned) != 1 || learned[0] != want {
		t.Fatalf("the skill was taught %d times (%v), want once, as %s", len(learned), learned, want)
	}
	if got := replicas[0].stored(t, conceptSkill, want); got != 1 {
		t.Fatalf("the skill has %d stored versions, want the one it was learned as", got)
	}
}

// Every read the learner makes INSIDE its gate is a fresh read. The stale-cache test
// above shows what that is for on the skills; the evidence read cannot be shown stale
// in the same way here, because a replica's own trace write evicts its cache of
// traces first. So the fact is pinned where it is made: from inside each read, the
// test tries to take the learner's own gate and notes whether it is held. The only
// reads made outside it are the learner's looks for a live skill, one for each
// learning, which may be answered from the cache because they only decide whether the
// gate is worth taking (what they say is read again inside it).
func TestLiveLearningReadsFreshInsideItsGate(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	owner := "user-games-fresh-" + n
	ctx := asOwner(owner)
	sig := GoalSignature("Light up the area!", []string{"spot"})
	probe := &readProbe{rowStore: lg.provider.store, pool: lg.provider.gates(), key: "games-learn:" + owner + ":" + sig}
	lg.provider.store = probe

	learnTorchSkill(t, lg, ctx, "fresh-"+n) // two traces: two learnings
	gated := map[string]int{}
	var looks []string
	for _, c := range probe.calls {
		if !c.gated {
			looks = append(looks, c.name)
			continue
		}
		gated[c.name]++
		if !c.fresh {
			t.Errorf("the learner read %s inside its gate from the result cache, want a fresh read", c.name)
		}
	}
	if !sameStrings(looks, []string{"gamesSkillsForSignature", "gamesSkillsForSignature"}) {
		t.Errorf("the reads outside the gate were %v, want one look for a live skill for each of the two learnings and nothing else", looks)
	}
	if gated["gamesSkillsForSignature"] != 2 || gated["gamesLearningTraces"] != 2 || len(gated) != 2 {
		t.Errorf("the reads inside the gate were %v, want both queries once for each of the two learnings", gated)
	}
}

// readProbe notes, for each query it is asked and in order, whether the read is fresh
// and whether the gate under key is held at that moment.
type readProbe struct {
	rowStore
	pool  *sql.DB
	key   string
	calls []probedRead
}

type probedRead struct {
	name         string
	fresh, gated bool
}

func (r *readProbe) query(ctx context.Context, name string, args map[string]any) ([]map[string]any, error) {
	call := probedRead{name: name, fresh: memql.FreshReadFromContext(ctx)}
	other, err := memql.AcquireWriteGatesWithin(ctx, r.pool, 100*time.Millisecond, r.key)
	if err == nil {
		other()
	} else {
		call.gated = memql.IsWriteGateBusy(err)
	}
	r.calls = append(r.calls, call)
	return r.rowStore.query(ctx, name, args)
}

// Learning waits for its gate and says so when it cannot have it: the trace is
// already recorded, so it will count the next time, and the error is the gate's
// busy answer. The gate is the one the learner names, games-learn:<owner>:<signature>:
// holding exactly that key from outside is what stops a learner.
func TestLiveLearningThatCannotTakeItsGateRecordsTheTraceAndLearnsNextTime(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	owner := "user-games-busy-" + n
	ctx := asOwner(owner)
	sig := GoalSignature("Light up the area!", []string{"spot"})

	lg.record(t, ctx, trace("busy-"+n+"-a", torchPlan(4), "success", "neutral"))

	release, err := memql.AcquireWriteGate(context.Background(), lg.provider.gates(), "games-learn:"+owner+":"+sig)
	if err != nil {
		t.Fatal(err)
	}
	held := true
	defer func() {
		if held {
			release()
		}
	}()
	short, cancel := context.WithTimeout(ctx, 3*time.Second)
	_, err = lg.execute(short, "builtin", "gamesRecordTrace", trace("busy-"+n+"-b", torchPlan(6), "success", "neutral"))
	cancel()
	if err == nil || !memql.IsWriteGateBusy(err) || !strings.Contains(err.Error(), "the trace is recorded and will count next time") {
		t.Fatalf("a learner that cannot take its gate = %v, want the busy answer saying the trace is recorded", err)
	}
	if got := lg.stored(t, conceptPlanTrace, "busy-"+n+"-b"); got != 1 {
		t.Fatalf("the trace was stored %d times, want once: it is recorded before the learner is asked", got)
	}
	if got := lg.skillsOf(t, ctx, sig); len(got) != 0 {
		t.Fatalf("a learner without its gate taught %v", got)
	}

	release()
	held = false
	skillID := lg.record(t, ctx, trace("busy-"+n+"-c", torchPlan(9), "success", "neutral"))
	if want := SkillID(owner, sig, []string{"collect", "craft", "placeTorches"}, 1); skillID != want {
		t.Fatalf("the next trace taught %q, want %s", skillID, want)
	}
	skill := lg.must(t, ctx, "query", "gamesSkillById", map[string]any{"skillId": skillID})
	if got := asStrings(skill[0]["learnedFrom"]); !sameStrings(sortedCopy(got), []string{"busy-" + n + "-a", "busy-" + n + "-b", "busy-" + n + "-c"}) {
		t.Fatalf("learnedFrom = %v, want all three traces: the one that could not learn counted", got)
	}
}

// ---- what the learner learns from ----

// A retired skill is terminal: when the goal comes back it is learned anew, as a
// new skill at the next generation, and the retired one stays what it was. A live
// skill of the same actions still stops a third trace from learning it again.
func TestLiveARetiredSkillIsLearnedAnewAtTheNextGeneration(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	owner := "user-games-gen-" + n
	ctx := asOwner(owner)
	sig := GoalSignature("Light up the area!", []string{"spot"})
	actions := []string{"collect", "craft", "placeTorches"}

	first := learnTorchSkill(t, lg, ctx, "gen-"+n)
	if first != SkillID(owner, sig, actions, 1) {
		t.Fatalf("the first generation is at %s, got %s", SkillID(owner, sig, actions, 1), first)
	}

	// Retiring is the ladder's decision (component/work's rule, applied by the ladder
	// builtins); the test writes its outcome with the ladder's own writer.
	read := lg.must(t, ctx, "query", "gamesSkillById", map[string]any{"skillId": first})
	lg.must(t, auth.ContextWithInternalOrigin(ctx), "mutation", "recordGamesSkillLadder", map[string]any{
		"skillId": first, "versionAt": rfc3339(versionAfter(timeOf(read[0]["createdAt"]), time.Now())), "rung": "retired", "state": "retired",
		"shadowMatches": 0, "canaryMatches": 0, "distinctBindings": map[string]any{}, "failures": 0, "promotionApprovalId": "",
		"lastReason": "unused for the retirement window",
	})
	if got := lg.must(t, ctx, "query", "gamesSkillById", map[string]any{"skillId": first}); got[0]["rung"] != "retired" {
		t.Fatalf("the skill was not retired: %v", got)
	}

	second := lg.record(t, ctx, trace("gen-"+n+"-c", torchPlan(9), "success", "neutral"))
	if want := SkillID(owner, sig, actions, 2); second != want || second == first {
		t.Fatalf("the goal came back and taught %q, want a new skill at generation 2, %s", second, want)
	}
	if got := lg.must(t, ctx, "query", "gamesSkillById", map[string]any{"skillId": first}); got[0]["rung"] != "retired" || lg.stored(t, conceptSkill, first) != 2 {
		t.Fatalf("the retired skill changed or was written again: %v (%d versions)", got, lg.stored(t, conceptSkill, first))
	}
	if again := lg.record(t, ctx, trace("gen-"+n+"-d", torchPlan(11), "success", "neutral")); again != "" {
		t.Fatalf("a live skill of generation 2 already covers the goal, yet a trace taught %q", again)
	}
	if rows := lg.skillsOf(t, ctx, sig); len(rows) != 2 {
		t.Fatalf("the goal has %d skills, want the retired one and its successor", len(rows))
	}
}

// Only traces that can be generalized together are: the same action sequence, and
// no skill runs. The goal has two ways of being done; each is learned from its own
// traces, a skill run (a plan the skill itself produced) is evidence of nothing
// and is left out of what a skill is learned from, and neither way is taught by
// the other's trace.
func TestLiveLearningUsesTheSameActionSequenceAndLeavesOutSkillRuns(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	owner := "user-games-evidence-" + n
	ctx := asOwner(owner)
	sig := GoalSignature("Light up the area!", []string{"spot"})
	full := []string{"collect", "craft", "placeTorches"}
	short := []string{"craft", "placeTorches"}
	shortPlan := func(count float64) []PlanStep { return torchPlan(count)[1:] }
	rec := func(id string, plan []PlanStep, extra map[string]any) string {
		tr := trace(id, plan, "success", "neutral")
		for k, v := range extra {
			tr[k] = v
		}
		return lg.record(t, ctx, tr)
	}

	if got := rec("ev-full-1-"+n, torchPlan(4), nil); got != "" {
		t.Fatalf("one trace taught %q", got)
	}
	if got := rec("ev-short-1-"+n, shortPlan(4), nil); got != "" {
		t.Fatalf("a trace of another action sequence made a pair: taught %q", got)
	}
	if got := rec("ev-run-"+n, torchPlan(5), map[string]any{"skillId": "gs-ran-" + n}); got != "" {
		t.Fatalf("a skill run made a pair: taught %q", got)
	}
	fullSkill := rec("ev-full-2-"+n, torchPlan(6), nil)
	if want := SkillID(owner, sig, full, 1); fullSkill != want {
		t.Fatalf("the second trace of the full sequence taught %q, want %s", fullSkill, want)
	}
	skill := lg.must(t, ctx, "query", "gamesSkillById", map[string]any{"skillId": fullSkill})
	if got := asStrings(skill[0]["learnedFrom"]); !sameStrings(sortedCopy(got), []string{"ev-full-1-" + n, "ev-full-2-" + n}) {
		t.Fatalf("learnedFrom = %v, want the two full-sequence traces and neither the short one nor the skill run", got)
	}
	if got := asStrings(skill[0]["actions"]); !sameStrings(got, full) {
		t.Fatalf("actions = %v, want %v", got, full)
	}

	shortSkill := rec("ev-short-2-"+n, shortPlan(6), nil)
	if want := SkillID(owner, sig, short, 1); shortSkill != want || shortSkill == fullSkill {
		t.Fatalf("the second trace of the short sequence taught %q, want its own skill %s", shortSkill, want)
	}
	skill = lg.must(t, ctx, "query", "gamesSkillById", map[string]any{"skillId": shortSkill})
	if got := asStrings(skill[0]["learnedFrom"]); !sameStrings(sortedCopy(got), []string{"ev-short-1-" + n, "ev-short-2-" + n}) {
		t.Fatalf("learnedFrom = %v, want the two short-sequence traces", got)
	}
	if rows := lg.skillsOf(t, ctx, sig); len(rows) != 2 {
		t.Fatalf("the goal has %d skills, want one for each way of doing it", len(rows))
	}
}

// A trace sent twice is one trace: the id names the row, so the second send is a
// new version of it, and the learner counts rows. A runtime that retries after
// an error never makes a pair out of one plan.
func TestLiveATraceSentTwiceCountsOnce(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	ctx := asOwner("user-games-twice-" + n)
	id := "twice-" + n
	for i := 0; i < 2; i++ {
		if got := lg.record(t, ctx, trace(id, torchPlan(4), "success", "neutral")); got != "" {
			t.Fatalf("send %d of one trace taught %q", i+1, got)
		}
	}
	if got := lg.stored(t, conceptPlanTrace, id); got != 2 {
		t.Fatalf("the trace has %d stored versions, want one for each send", got)
	}
	skillID := lg.record(t, ctx, trace("twice-"+n+"-other", torchPlan(6), "success", "neutral"))
	if skillID == "" {
		t.Fatal("a second, different trace taught nothing")
	}
	skill := lg.must(t, ctx, "query", "gamesSkillById", map[string]any{"skillId": skillID})
	if got := asStrings(skill[0]["learnedFrom"]); !sameStrings(sortedCopy(got), []string{id, "twice-" + n + "-other"}) {
		t.Fatalf("learnedFrom = %v, want each trace once", got)
	}
}

// Every trace is recorded as the engine computed it, whether or not it can teach:
// the signature recomputed (also when the caller supplied the right one), the
// actions derived from the steps, the reaction defaulted, the skill run named by
// its bare id. A failure and a disliked success are kept as evidence of what
// happened and are not what a skill is learned from.
func TestLiveATraceIsStoredAsTheEngineComputedIt(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	ctx := asOwner("user-games-stored-" + n)
	sig := GoalSignature("Light up the area!", []string{"spot"})

	ok := trace("stored-ok-"+n, torchPlan(4), "success", "")
	delete(ok, "reaction")
	ok["goalSignature"] = sig
	lg.record(t, ctx, ok)
	lg.record(t, ctx, trace("stored-fail-"+n, torchPlan(4), "failure", "neutral"))
	lg.record(t, ctx, trace("stored-disliked-"+n, torchPlan(4), "success", "negative"))
	lg.record(t, ctx, trace("stored-abandoned-"+n, torchPlan(4), "abandoned", "neutral"))
	for _, id := range []string{"stored-ok-" + n, "stored-fail-" + n, "stored-disliked-" + n, "stored-abandoned-" + n} {
		if got := lg.stored(t, conceptPlanTrace, id); got != 1 {
			t.Errorf("trace %s has %d stored versions, want 1", id, got)
		}
	}

	rows := lg.must(t, ctx, "query", "gamesLearningTraces", map[string]any{"goalSignature": sig})
	if len(rows) != 1 || bare(trimmed(rows[0]["id"])) != "stored-ok-"+n {
		t.Fatalf("the learner's evidence is %v, want only the success the player did not mind", rows)
	}
	row := rows[0]
	steps, _ := row["steps"].([]any)
	if row["goalSignature"] != sig || row["reaction"] != "neutral" || !sameStrings(asStrings(row["actions"]), []string{"collect", "craft", "placeTorches"}) ||
		row["outcome"] != "success" || row["trigger"] != "initiative" || row["characterId"] != "wren" || row["say"] != "Torches coming up!" ||
		len(steps) != 3 || ownerKey(trimmed(row["ownerUserId"])) != ownerKey(actorUserID(ctx)) {
		t.Fatalf("the stored trace is not what the engine computed: %v", row)
	}
	if inputs := asObject(row["inputs"]); len(inputs) != 1 || inputs["spot"] != "here" {
		t.Fatalf("inputs = %v", row["inputs"])
	}
	first, _ := steps[0].(map[string]any)
	if first["action"] != "collect" || asObject(first["args"])["block"] != "coal" || first["ok"] != true || asInt(first["durationMs"], 0) != 1200 {
		t.Fatalf("the steps are not stored as sent: %v", steps)
	}
}

// A plan the engine cannot learn from is refused as the builtin's own refusal, and
// nothing is stored: not a trace, and so not evidence. (Task 6's strict parsePlan,
// reached through the engine's own argument handling.)
func TestLiveATraceItCannotLearnFromIsRefusedAndNothingIsStored(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	ctx := asOwner("user-games-refused-" + n)
	for name, action := range map[string]any{
		"a number for an action":      42,
		"a comma in an action name":   "collect,craft",
		"a vertical bar in an action": "collect|craft",
		"a blank action":              "  ",
	} {
		t.Run(name, func(t *testing.T) {
			id := "refused-" + strings.Fields(name)[1] + "-" + n
			tr := trace(id, torchPlan(4), "success", "neutral")
			tr["steps"] = []any{map[string]any{"action": action, "args": map[string]any{"block": "coal"}, "ok": true, "durationMs": 1}}
			if _, err := lg.call(t, ctx, "builtin", "gamesRecordTrace", tr); err == nil || !strings.Contains(err.Error(), "games_invalid_argument") {
				t.Fatalf("error = %v, want games_invalid_argument", err)
			}
			if got := lg.stored(t, conceptPlanTrace, id); got != 0 {
				t.Fatalf("the refused trace was stored %d times", got)
			}
		})
	}
}

// A skill learned while the model is away is still learned -- the embedding is
// the skill's way to be found by meaning, not part of knowing how -- and
// gamesEmbedPending gives it its vector when the model is back.
func TestLiveASkillLearnedWhileTheModelIsAwayIsEmbeddedLater(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	ctx := asOwner("user-games-skillvec-" + n)
	vectors := func(skillID string) int {
		t.Helper()
		var count int
		if err := lg.db.DB.QueryRow(`SELECT count(*) FROM `+lg.table+` WHERE id = $1 AND vector_field = 'content'`, canonical(conceptSkill, skillID)).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}

	// distance is how far the stored vector is from the embedding of a text.
	distance := func(skillID, text string) float64 {
		t.Helper()
		var d float64
		if err := lg.db.DB.QueryRow(`SELECT embedding <=> $1::vector FROM `+lg.table+` WHERE id = $2 AND vector_field = 'content'`,
			vectorLiteral(bagOfWords(text)), canonical(conceptSkill, skillID)).Scan(&d); err != nil {
			t.Fatal(err)
		}
		return d
	}

	lg.provider.space = failingSpace{testSpace{db: lg.db.DB, tableName: lg.table}}
	away := learnTorchSkill(t, lg, ctx, "away-"+n)
	if got := vectors(away); got != 0 {
		t.Fatalf("a skill learned while the model was away has %d vectors", got)
	}

	lg.provider.space = testSpace{db: lg.db.DB, tableName: lg.table}
	if res := lg.must(t, ctx, "builtin", "gamesEmbedPending", nil); asInt(res[0]["embedded"], 0) != 1 || vectors(away) != 1 {
		t.Fatalf("embedPending = %v (vectors %d), want the skill embedded once the model is back", res[0], vectors(away))
	}
	if d := distance(away, "Light up the area!"); d > 1e-4 {
		t.Fatalf("the back-filled vector is %v from the goal's embedding, want the goal's", d)
	}

	// With the model there, the skill is embedded as it is learned, from its goal
	// (another owner's: this one already has a live skill for the goal).
	here := learnTorchSkill(t, lg, asOwner("user-games-skillvec-b-"+n), "here-"+n)
	if got := vectors(here); got != 1 {
		t.Fatalf("a skill learned with the model present has %d vectors, want 1", got)
	}
	if d := distance(here, "Light up the area!"); d > 1e-4 {
		t.Fatalf("the vector stored as the skill was learned is %v from the goal's embedding, want the goal's", d)
	}
}

// A skill speaks in the words of the NEWEST trace it was learned from (the goal as
// that trace stated it, and what the character said) and keeps every trigger the
// traces were recorded under, once each, in order. Two goal texts that fold to one
// signature are one goal.
func TestLiveASkillTakesTheNewestTracesWordsAndEveryTrigger(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	owner := "user-games-words-" + n
	ctx := asOwner(owner)

	older := trace("words-a-"+n, torchPlan(4), "success", "neutral")
	older["goalText"], older["trigger"], older["say"] = "light up the area", "addressedChat", "On it."
	time.Sleep(5 * time.Millisecond)
	newer := trace("words-b-"+n, torchPlan(6), "success", "neutral")
	newer["goalText"], newer["trigger"], newer["say"] = "Light up the area!", "initiative", "Torches coming up!"
	if got := lg.record(t, ctx, older); got != "" {
		t.Fatalf("one trace taught %q", got)
	}
	skillID := lg.record(t, ctx, newer)
	if skillID == "" {
		t.Fatal("two traces of one goal taught nothing: the goal texts fold to one signature")
	}
	skill := lg.must(t, ctx, "query", "gamesSkillById", map[string]any{"skillId": skillID})
	if skill[0]["goalText"] != "Light up the area!" || skill[0]["confirmation"] != "Torches coming up!" {
		t.Fatalf("goalText %v, confirmation %v, want the newest trace's words", skill[0]["goalText"], skill[0]["confirmation"])
	}
	if got := asStrings(skill[0]["triggers"]); !sameStrings(got, []string{"addressedChat", "initiative"}) {
		t.Fatalf("triggers = %v, want both, sorted", got)
	}
}

// Traces of one goal that agree on their actions but not on the argument keys of a
// step cannot be generalized into one template. That is no error of the trace's:
// it is recorded, it teaches nothing, and the call that sent it succeeds.
func TestLiveTracesThatDisagreeOnArgumentKeysAreRecordedAndTeachNothing(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	owner := "user-games-keys-" + n
	ctx := asOwner(owner)
	sig := GoalSignature("Light up the area!", []string{"spot"})

	extra := torchPlan(6)
	extra[1].Args["near"] = "home"
	if got := lg.record(t, ctx, trace("keys-a-"+n, torchPlan(4), "success", "neutral")); got != "" {
		t.Fatalf("one trace taught %q", got)
	}
	if got := lg.record(t, ctx, trace("keys-b-"+n, extra, "success", "neutral")); got != "" {
		t.Fatalf("traces that cannot be generalized taught %q", got)
	}
	if got := lg.stored(t, conceptPlanTrace, "keys-b-"+n); got != 1 {
		t.Fatalf("the second trace was stored %d times, want once", got)
	}
	if got := lg.skillsOf(t, ctx, sig); len(got) != 0 {
		t.Fatalf("a skill was learned from traces with different argument keys: %v", got)
	}
}

// ---- a trace id belongs to one owner ----

// traceVersion reads the newest stored version of one trace row, whoever owns it:
// when it was written and its whole payload. The test reads around the owned tier
// on purpose -- it is asking what is in the table.
func (lg *liveGames) traceVersion(t *testing.T, bareID string) (createdAt, payload string) {
	t.Helper()
	if err := lg.db.DB.QueryRow(`SELECT "createdAt"::text, payload::text FROM "MemoryNodes"
WHERE concept = $1 AND id = $2 ORDER BY "createdAt" DESC LIMIT 1`, conceptPlanTrace, canonical(conceptPlanTrace, bareID)).Scan(&createdAt, &payload); err != nil {
		t.Fatalf("read the newest version of trace %s: %v", bareID, err)
	}
	return createdAt, payload
}

// traceVersionsOwnedBy counts the stored versions of one trace row that name this owner.
func (lg *liveGames) traceVersionsOwnedBy(t *testing.T, bareID, owner string) int {
	t.Helper()
	var n int
	if err := lg.db.DB.QueryRow(`SELECT count(*) FROM "MemoryNodes" WHERE concept = $1 AND id = $2 AND payload->>'ownerUserId' = $3`,
		conceptPlanTrace, canonical(conceptPlanTrace, bareID), ownerKey(owner)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A trace id names one row for the whole cluster, and the write that records a trace
// runs under internal origin, which the owned tier's write guard lets through
// without asking whose row it lands on. So the pack asks: a trace id another owner
// holds is refused, nothing is written, and the refusal does not say who holds it.
// The owner's own re-send still works (it is a new version of their row), whichever
// way the id is spelled, and everyone else's ids are unaffected.
func TestLiveATraceIdAnotherOwnerHoldsIsRefused(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	aName, bName, cName := "user-games-taken-a-"+n, "user-games-taken-b-"+n, "user-games-taken-c-"+n
	a, b, c := asOwner(aName), asOwner(bName), clusterOwnerOf(cName)
	sig := GoalSignature("Light up the area!", []string{"spot"})
	id := "taken-" + n

	mine := trace(id, torchPlan(4), "success", "neutral")
	mine["say"] = "A's words"
	lg.record(t, a, mine)
	versionBefore, payloadBefore := lg.traceVersion(t, id)
	if got := lg.stored(t, conceptPlanTrace, id); got != 1 {
		t.Fatalf("the trace has %d stored versions, want 1", got)
	}

	// Another owner, and the cluster's owner (who may READ A's trace and is still not
	// A), with the id spelled bare and spelled as the row's own canonical id.
	for who, ctx := range map[string]context.Context{"another owner": b, "the cluster owner": c} {
		for spelling, sent := range map[string]string{"bare": id, "canonical": canonical(conceptPlanTrace, id)} {
			tr := trace(sent, torchPlan(6), "success", "positive")
			tr["say"] = "taken over"
			_, err := lg.call(t, ctx, "builtin", "gamesRecordTrace", tr)
			if err == nil || !strings.HasPrefix(err.Error(), "games_invalid_argument: ") || !strings.Contains(err.Error(), "that trace id is taken") {
				t.Errorf("%s sending A's trace id spelled %s: %v, want games_invalid_argument: that trace id is taken", who, spelling, err)
				continue
			}
			for _, secret := range []string{aName, ownerKey(aName), "A's words"} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("the refusal to %s says %q, which belongs to the owner who holds the id", who, secret)
				}
			}
		}
	}

	// A's row is as it was: one version, the very same newest version, the same
	// payload; and nobody else has a row.
	if got := lg.stored(t, conceptPlanTrace, id); got != 1 {
		t.Errorf("the trace has %d stored versions after the refusals, want 1", got)
	}
	if createdAt, payload := lg.traceVersion(t, id); createdAt != versionBefore || payload != payloadBefore {
		t.Errorf("A's trace changed:\n before %s %s\n after  %s %s", versionBefore, payloadBefore, createdAt, payload)
	}
	for who, owner := range map[string]string{"B": bName, "the cluster owner": cName} {
		if got := lg.traceVersionsOwnedBy(t, id, owner); got != 0 {
			t.Errorf("%s holds %d versions of A's trace", who, got)
		}
	}
	if rows := lg.must(t, a, "query", "gamesLearningTraces", map[string]any{"goalSignature": sig}); len(rows) != 1 || rows[0]["say"] != "A's words" {
		t.Errorf("A reads %v, want the one trace A recorded, unchanged", rows)
	}
	if rows := lg.must(t, b, "query", "gamesLearningTraces", map[string]any{"goalSignature": sig}); len(rows) != 0 {
		t.Errorf("B reads %d traces, want none: they recorded none", len(rows))
	}

	// A sends it again, with something new: that is a new version of their row.
	again := trace(id, torchPlan(5), "success", "neutral")
	again["say"] = "A's new words"
	lg.record(t, a, again)
	if got := lg.stored(t, conceptPlanTrace, id); got != 2 {
		t.Errorf("A's re-send left %d stored versions, want 2", got)
	}
	if rows := lg.must(t, a, "query", "gamesLearningTraces", map[string]any{"goalSignature": sig}); len(rows) != 1 || rows[0]["say"] != "A's new words" {
		t.Errorf("A reads %v after the re-send, want the one trace with the new words", rows)
	}
	if got := lg.traceVersionsOwnedBy(t, id, aName); got != 2 {
		t.Errorf("A holds %d versions of their trace, want both", got)
	}

	// Being refused one id is no bar to recording another.
	if _, err := lg.call(t, b, "builtin", "gamesRecordTrace", trace("taken-b-"+n, torchPlan(4), "success", "neutral")); err != nil {
		t.Errorf("B recording an id of their own: %v", err)
	}
	if got := lg.traceVersionsOwnedBy(t, "taken-b-"+n, bName); got != 1 {
		t.Errorf("B holds %d versions of their own trace, want 1", got)
	}
}

// Two owners, on two replicas, send the same NEW trace id at the same moment: the
// id is free when each looks, and only the gate on the id makes the look and the
// write one step. Exactly one is recorded and the other is refused; the row is one
// version, the winner's. Without the gate both look, both find it free and both
// write, and the second write is a new version of a row it does not hold.
//
// THE RACE IS MADE, NOT HOPED FOR. On this harness a second concurrent statement
// waits for a connection for longer than the first racer's whole write takes, so two
// plain racers never overlap and a missing gate would go unseen (measured: the
// second look ended 11 ms after it began, the first write 5 ms). So each racer does
// the real look and then waits, for a moment, for the other racer's look before it
// writes. With the gate the other racer cannot look until the first is done, the wait
// runs out, and nothing changes; without it both looks happen, both find the id free,
// and both write.
func TestLiveTwoOwnersRacingForOneTraceIdOnlyOneRecordsIt(t *testing.T) {
	replicas := []*liveGames{newLiveGames(t)}
	replicas = append(replicas, replicas[0].replica(t))
	n := runID()
	aName, bName := "user-games-tie-a-"+n, "user-games-tie-b-"+n
	owners := []context.Context{asOwner(aName), asOwner(bName)}
	names := []string{aName, bName}

	var meeting atomic.Pointer[traceMeeting]
	for _, r := range replicas {
		r.provider.holderOf = func(ctx context.Context, concept, id string) (string, bool, error) {
			owner, found, err := r.provider.queryHolder(ctx, concept, id)
			if m := meeting.Load(); m != nil {
				m.meet()
			}
			return owner, found, err
		}
	}

	for round := 0; round < 4; round++ {
		id := fmt.Sprintf("tie-%d-%s", round, n)
		spellings := []string{id, id}
		if round%2 == 1 { // the same row, spelled two ways
			spellings[1] = canonical(conceptPlanTrace, id)
		}
		meeting.Store(newTraceMeeting())
		errs := make([]error, 2)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				// A failure, so that nothing learns and only the id is at stake.
				_, errs[i] = replicas[i].execute(owners[i], "builtin", "gamesRecordTrace", trace(spellings[i], torchPlan(float64(4+i)), "failure", "neutral"))
			}()
		}
		close(start)
		wg.Wait()

		winners, refused := 0, 0
		for i, err := range errs {
			switch {
			case err == nil:
				winners++
			case strings.Contains(err.Error(), "games_invalid_argument: that trace id is taken"):
				refused++
			default:
				t.Errorf("round %d, %s: %v, want recorded or refused as taken", round, names[i], err)
			}
		}
		if winners != 1 || refused != 1 {
			t.Errorf("round %d: %d recorded and %d refused, want one of each (errors %v)", round, winners, refused, errs)
			continue
		}
		if got := replicas[0].stored(t, conceptPlanTrace, id); got != 1 {
			t.Errorf("round %d: the trace has %d stored versions, want the winner's one", round, got)
		}
		held := 0
		for _, name := range names {
			held += replicas[0].traceVersionsOwnedBy(t, id, name)
		}
		if held != 1 {
			t.Errorf("round %d: the owners of the row's versions add up to %d, want one version with one owner", round, held)
		}
	}
}

// traceMeeting makes two racers meet between their looks and their writes: the
// first to arrive waits for the second, but only for a moment, since a gate can keep
// the second from ever arriving.
type traceMeeting struct {
	mu      sync.Mutex
	arrived int
	both    chan struct{}
}

func newTraceMeeting() *traceMeeting { return &traceMeeting{both: make(chan struct{})} }

func (m *traceMeeting) meet() {
	m.mu.Lock()
	m.arrived++
	if m.arrived == 2 {
		close(m.both)
	}
	m.mu.Unlock()
	select {
	case <-m.both:
	case <-time.After(300 * time.Millisecond):
	}
}

// Who holds a trace is who its NEWEST version names. A row can carry versions of two
// owners if it was written before the claim existed (the takeover this change
// closes), and then the owner of the newest version is the one the owned tier shows
// the row to: that owner may send it again and the other may not.
func TestLiveTheNewestVersionOfATraceDecidesWhoHoldsIt(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	aName, bName := "user-games-newest-a-"+n, "user-games-newest-b-"+n
	a, b := asOwner(aName), asOwner(bName)
	id := "newest-" + n

	lg.record(t, a, trace(id, torchPlan(4), "failure", "neutral"))
	time.Sleep(5 * time.Millisecond)
	lg.writeTrace(t, bName, traceAbout{id: id}) // B's version, written the way the pack once let it happen
	if got := lg.stored(t, conceptPlanTrace, id); got != 2 {
		t.Fatalf("the trace has %d versions, want A's and B's", got)
	}

	if _, err := lg.call(t, b, "builtin", "gamesRecordTrace", trace(id, torchPlan(5), "failure", "neutral")); err != nil {
		t.Errorf("B holds the newest version and sending it again was refused: %v", err)
	}
	if _, err := lg.call(t, a, "builtin", "gamesRecordTrace", trace(id, torchPlan(6), "failure", "neutral")); err == nil || !strings.Contains(err.Error(), "that trace id is taken") {
		t.Errorf("A holds only an older version and sending it again got %v, want it refused", err)
	}
	if got := lg.stored(t, conceptPlanTrace, id); got != 3 {
		t.Errorf("the trace has %d versions, want B's re-send added one and A's refusal none", got)
	}
}

// ---- the learner's gate is short ----

// blockingSpace is an embedder that does not answer until the test lets it: the model
// taking its time. The first embedding it is asked for says so.
type blockingSpace struct {
	testSpace
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingSpace(lg *liveGames) *blockingSpace {
	return &blockingSpace{testSpace: testSpace{db: lg.db.DB, tableName: lg.table}, entered: make(chan struct{}), release: make(chan struct{})}
}

func (b *blockingSpace) store(ctx context.Context, nodeID, concept, text string) error {
	b.once.Do(func() { close(b.entered) })
	select {
	case <-b.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return b.testSpace.store(ctx, nodeID, concept, text)
}

// The gate is held for the reads and the write and let go BEFORE the skill's goal is
// embedded: the embedding is a call to a model, the gate is one of a pool of four
// that every trace's own gate is taken from too, and a waiter holds its connection for
// up to six seconds. A slow model with the gate held across it, and three more traces
// of the goal waiting for it, would leave other owners' traces unable to take theirs.
//
// So the model here does not answer until the test says. While it waits: the skill is
// written, the learner's gate is free, four more traces of the same goal and another
// owner's trace are all recorded, and none waits for a gate.
func TestLiveTheLearnGateIsFreeWhileTheSkillIsEmbedded(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	owner, other := "user-games-embed-a-"+n, "user-games-embed-b-"+n
	ctx := asOwner(owner)
	sig := GoalSignature("Light up the area!", []string{"spot"})
	actions := []string{"collect", "craft", "placeTorches"}

	lg.record(t, ctx, trace("embed-a-"+n, torchPlan(4), "success", "neutral"))
	space := newBlockingSpace(lg)
	lg.provider.space = space
	type learned struct {
		id  string
		err error
	}
	done := make(chan learned, 1)
	go func() {
		rows, err := lg.execute(ctx, "builtin", "gamesRecordTrace", trace("embed-b-"+n, torchPlan(6), "success", "neutral"))
		var id string
		if err == nil {
			id, _ = rows[0]["learnedSkillId"].(string)
		}
		done <- learned{id, err}
	}()
	select {
	case <-space.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the skill was never embedded")
	}
	letGo := sync.OnceFunc(func() { close(space.release) })
	defer letGo() // never leave the learner waiting, whatever the test finds

	// The skill is written, and its goal is being embedded.
	skillID := SkillID(owner, sig, actions, 1)
	if rows := lg.skillsOf(t, ctx, sig); len(rows) != 1 || bare(trimmed(rows[0]["id"])) != skillID {
		t.Fatalf("while the goal is embedded the skills are %v, want the one that was written", rows)
	}
	// The learner's gate is free.
	if free, err := memql.AcquireWriteGatesWithin(ctx, lg.provider.gates(), 500*time.Millisecond, "games-learn:"+owner+":"+sig); err != nil {
		t.Errorf("the learner's gate is held while the skill is embedded: %v", err)
	} else {
		free()
	}
	// Nobody waits for a gate: more traces of the goal, and another owner's.
	started := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, 5)
	send := func(who context.Context, id string, count float64) {
		defer wg.Done()
		if _, err := lg.execute(who, "builtin", "gamesRecordTrace", trace(id, torchPlan(count), "success", "neutral")); err != nil {
			errs <- fmt.Errorf("%s: %w", id, err)
		}
	}
	for i := range 4 {
		wg.Add(1)
		go send(ctx, fmt.Sprintf("embed-more-%d-%s", i, n), float64(7+i))
	}
	wg.Add(1)
	go send(asOwner(other), "embed-other-"+n, 4)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("a trace recorded while the model is slow failed: %v", err)
	}
	if waited := time.Since(started); waited > 5*time.Second {
		t.Errorf("five traces took %v while the model was slow, want them not to wait for a gate", waited)
	}

	// The model answers: the learner finishes, with the skill, and the skill has its vector.
	letGo()
	select {
	case got := <-done:
		if got.err != nil || got.id != skillID {
			t.Errorf("the learner finished with %q, %v, want the skill %s", got.id, got.err, skillID)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the learner did not finish once the model answered")
	}
	var vectors int
	if err := lg.db.DB.QueryRow(`SELECT count(*) FROM `+lg.table+` WHERE id = $1 AND vector_field = 'content'`, canonical(conceptSkill, skillID)).Scan(&vectors); err != nil || vectors != 1 {
		t.Errorf("the skill has %d vectors (%v), want the one the model answered with", vectors, err)
	}
}

// Most qualifying traces arrive after the skill exists. They take the gate on their own
// trace id and nothing else: the learner finds the skill with an ordinary read and
// does not take its gate. A goal with no skill yet still takes it, to read again inside
// it and to make the skill if the pair is there.
func TestLiveATraceAfterTheSkillExistsTakesNoLearnGate(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	owner := "user-games-nogate-" + n
	ctx := asOwner(owner)

	var mu sync.Mutex
	var keys []string
	lg.provider.lock = func(ctx context.Context, key string) (func(), error) {
		mu.Lock()
		keys = append(keys, key)
		mu.Unlock()
		return memql.AcquireWriteGate(ctx, lg.provider.gates(), key)
	}
	taken := func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := slices.Clone(keys)
		keys = nil
		return out
	}

	learnTorchSkill(t, lg, ctx, "nogate-"+n) // two traces, a skill: every gate is taken
	if got := taken(); len(got) != 4 || !strings.HasPrefix(got[1], "games-learn:") || !strings.HasPrefix(got[3], "games-learn:") {
		t.Fatalf("the two traces that taught the skill took the gates %v, want each trace's own and then the learner's", got)
	}
	for i := range 3 {
		if got := lg.record(t, ctx, trace(fmt.Sprintf("nogate-%d-%s", i, n), torchPlan(float64(7+i)), "success", "neutral")); got != "" {
			t.Fatalf("a live skill covers the goal, yet a trace taught %q", got)
		}
	}
	got := taken()
	if len(got) != 3 {
		t.Fatalf("three traces after the skill took the gates %v, want only each trace's own", got)
	}
	for _, key := range got {
		if !strings.HasPrefix(key, "games-trace:") {
			t.Errorf("a trace after the skill took the gate %s, want only its trace id's", key)
		}
	}

	// A goal with no skill yet does take the learner's gate.
	other := trace("nogate-other-"+n, torchPlan(4), "success", "neutral")
	other["goalText"] = "Dig a trench!"
	lg.record(t, ctx, other)
	if got := taken(); len(got) != 2 || !strings.HasPrefix(got[1], "games-learn:") {
		t.Errorf("a trace of a goal with no skill took the gates %v, want its own and the learner's", got)
	}
}

// ---- a trace is checked like an episode ----

// A trace's time is stored in one fixed UTC form, because gamesLearningTraces sorts by
// it as text and a skill takes the words of the NEWEST trace. Written as sent, an
// offset sorts by its clock face: 14:00 at +02:00 (which is 12:00Z) sorts ahead of
// 12:30Z, so the older trace read as the newer and the skill took its words.
// The later-timed trace is recorded FIRST here, so that neither the order of
// recording nor the order of the text can be what decides.
func TestLiveTracesAreOrderedByWhenTheyHappenedNotByHowTheTimeIsWritten(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	ctx := asOwner("user-games-order-" + n)
	sig := GoalSignature("Light up the area!", []string{"spot"})

	later := trace("order-later-"+n, torchPlan(6), "success", "neutral")
	later["at"], later["say"], later["goalText"] = "2026-10-19T12:30:00Z", "newer words", "Light up the area!"
	earlier := trace("order-earlier-"+n, torchPlan(4), "success", "neutral")
	earlier["at"], earlier["say"], earlier["goalText"] = "2026-10-19T14:00:00+02:00", "older words", "light up the area"
	if got := lg.record(t, ctx, later); got != "" {
		t.Fatalf("one trace taught %q", got)
	}
	skillID := lg.record(t, ctx, earlier)
	if skillID == "" {
		t.Fatal("two traces of one goal taught nothing")
	}

	rows := lg.must(t, ctx, "query", "gamesLearningTraces", map[string]any{"goalSignature": sig})
	var ats []string
	for _, r := range rows {
		ats = append(ats, trimmed(r["at"]))
	}
	if !sameStrings(ats, []string{"2026-10-19T12:30:00.000Z", "2026-10-19T12:00:00.000Z"}) {
		t.Errorf("the traces are read in the order %v, want the later first, both in the fixed UTC form", ats)
	}
	skill := lg.must(t, ctx, "query", "gamesSkillById", map[string]any{"skillId": skillID})
	if skill[0]["confirmation"] != "newer words" || skill[0]["goalText"] != "Light up the area!" {
		t.Errorf("the skill speaks as %q / %q, want the words of the trace that happened last", skill[0]["confirmation"], skill[0]["goalText"])
	}
}

// What createGamesPlanTrace constrains, and what a bare string-typed argument lets
// through the engine's own argument handling, is checked before the gate and refused with a
// code: a field over its limit (never echoed), a number, a boolean or a list where
// text goes (it would be stored as its printed form), a time that is not a datetime, and
// inputs that are not an object (they would be read as no inputs, which changes the
// signature). Nothing is stored.
func TestLiveATraceThatIsNotWellFormedIsRefusedWithACodeAndNothingIsStored(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	ctx := asOwner("user-games-wellformed-" + n)
	for i, tc := range []struct {
		name   string
		mutate func(map[string]any)
		want   string
	}{
		{"a characterId of 65 characters", func(a map[string]any) { a["characterId"] = strings.Repeat("c", 65) }, "characterId is 65 characters, at most 64"},
		{"a goalText of 201 characters", func(a map[string]any) { a["goalText"] = strings.Repeat("g", 201) }, "goalText is 201 characters, at most 200"},
		{"a trigger of 65 characters", func(a map[string]any) { a["trigger"] = strings.Repeat("r", 65) }, "trigger is 65 characters, at most 64"},
		{"a say of 201 characters", func(a map[string]any) { a["say"] = strings.Repeat("s", 201) }, "say is 201 characters, at most 200"},
		{"a goalText that is a number", func(a map[string]any) { a["goalText"] = 42 }, "goalText must be a string"},
		{"a say that is a number", func(a map[string]any) { a["say"] = 42 }, "say must be a string"},
		{"a trigger that is a boolean", func(a map[string]any) { a["trigger"] = true }, "trigger must be a string"},
		{"an at that is not a datetime", func(a map[string]any) { a["at"] = "yesterday" }, "at must be an RFC 3339 datetime"},
		{"an at that is a number", func(a map[string]any) { a["at"] = 42 }, "at must be a string"},
		{"inputs that are a string", func(a map[string]any) { a["inputs"] = "spot" }, "inputs must be an object"},
		{"inputs that are a list", func(a map[string]any) { a["inputs"] = []any{"spot"} }, "inputs must be an object"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := fmt.Sprintf("wellformed-%d-%s", i, n)
			tr := trace(id, torchPlan(4), "failure", "neutral")
			tc.mutate(tr)
			_, err := lg.call(t, ctx, "builtin", "gamesRecordTrace", tr)
			if err == nil || !strings.HasPrefix(err.Error(), "games_invalid_argument: ") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want games_invalid_argument: %s", err, tc.want)
			}
			if got := lg.stored(t, conceptPlanTrace, id); got != 0 {
				t.Fatalf("the refused trace was stored %d times", got)
			}
		})
	}
}
