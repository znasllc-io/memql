package gamespack

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// traceLog is a rowStore that remembers what recordTrace and learn asked it to
// write and answers each read with the rows it was given (none, by default); it is
// also the pack's gate, its look at who holds a row and its embedding space
// (Provider.lock, Provider.holderOf and Provider.space, via provider()). It keeps
// one list of everything that happened, in order, because the order is the point:
// the gate on a trace id, then who holds it, then the write, then the gate let
// go; the gate on a goal, the reads inside it, the skill, the gate let go, and only
// then the embedding.
type traceLog struct {
	traces  []map[string]any
	skills  []map[string]any
	queries []string
	events  []string
	embeds  []string // the text of each embedding asked for

	rows      map[string][]map[string]any // what a read answers, by the name of the query
	calls     map[string]int              // how many times each query has been asked
	readFresh []bool                      // whether each read, in order, was marked a fresh read
	queryErr  func(name string, call int) error
	skillErr  error // createSkill fails
	embedErr  error // the model is away

	holders        map[string]string // canonical row id -> the owner its newest version names
	holderFresh    []bool            // whether each look at a holder was marked a fresh read
	holderConcepts []string          // the concept each look at a holder asked about
	holderErr      error
	lockErr        error // the gate on a trace id cannot be taken
	learnErr       error // the gate of the learner cannot be taken
}

func (l *traceLog) provider() *Provider {
	return &Provider{store: l, space: l, lock: l.lock, holderOf: l.holder}
}

func (l *traceLog) lock(_ context.Context, key string) (func(), error) {
	if strings.HasPrefix(key, "games-trace:") && l.lockErr != nil {
		return nil, l.lockErr
	}
	if strings.HasPrefix(key, "games-learn:") && l.learnErr != nil {
		return nil, l.learnErr
	}
	l.events = append(l.events, "lock "+key)
	released := false
	return func() { // as the real one: safe to call more than once
		if !released {
			released = true
			l.events = append(l.events, "release "+key)
		}
	}, nil
}

func (l *traceLog) holder(ctx context.Context, concept, id string) (string, bool, error) {
	l.events = append(l.events, "holder "+id)
	l.holderFresh = append(l.holderFresh, memql.FreshReadFromContext(ctx))
	l.holderConcepts = append(l.holderConcepts, concept)
	if l.holderErr != nil {
		return "", false, l.holderErr
	}
	owner, found := l.holders[id]
	return owner, found, nil
}

func (l *traceLog) query(ctx context.Context, name string, _ map[string]any) ([]map[string]any, error) {
	l.queries = append(l.queries, name)
	l.readFresh = append(l.readFresh, memql.FreshReadFromContext(ctx))
	l.events = append(l.events, "read "+name)
	if l.calls == nil {
		l.calls = map[string]int{}
	}
	l.calls[name]++
	if l.queryErr != nil {
		if err := l.queryErr(name, l.calls[name]); err != nil {
			return nil, err
		}
	}
	return l.rows[name], nil
}

func (l *traceLog) write(context.Context, string, map[string]any) error { return nil }

func (l *traceLog) embedStore(context.Context, string, string, string) error { return nil }

func (l *traceLog) createTrace(_ context.Context, args map[string]any) error {
	l.traces = append(l.traces, args)
	l.events = append(l.events, "write "+trimmed(args["traceId"]))
	return nil
}

func (l *traceLog) createSkill(_ context.Context, args map[string]any) error {
	l.skills = append(l.skills, args)
	l.events = append(l.events, "create skill")
	return l.skillErr
}

func (l *traceLog) recordLadder(context.Context, map[string]any) error { return nil }

func (l *traceLog) walk(context.Context, string, map[string]any, func([]map[string]any) error) error {
	return nil
}

// the embeddingSpace of a pack that has no model to ask
func (l *traceLog) table(context.Context) (string, error) { return "", nil }

func (l *traceLog) embedQuery(context.Context, string) ([]float32, error) { return nil, nil }

func (l *traceLog) store(_ context.Context, nodeID, _, text string) error {
	l.events = append(l.events, "embed "+nodeID)
	l.embeds = append(l.embeds, text)
	return l.embedErr
}

// untouched is true when nothing at all happened: no gate, no look, no read, no write.
func (l *traceLog) untouched() bool {
	return len(l.events) == 0 && len(l.traces) == 0 && len(l.skills) == 0 && len(l.queries) == 0
}

// evidenceRow is a trace as gamesLearningTraces reads it back: the shaped row, with
// its owner in the canonical spelling and the plan as the engine hands it over.
func evidenceRow(owner, id string, plan []PlanStep, goalText, say, trigger string) map[string]any {
	return map[string]any{
		"id": canonical(conceptPlanTrace, id), "ownerUserId": ownerKey(owner), "goalText": goalText, "say": say,
		"trigger": trigger, "actions": stringsAsAny(Actions(plan)), "steps": stepsArg(plan), "skillId": "",
	}
}

// skillRow is a skill as gamesSkillsForSignature reads it back.
func skillRow(owner, id string, actions []string, rung string) map[string]any {
	return map[string]any{"id": canonical(conceptSkill, id), "ownerUserId": ownerKey(owner), "actions": stringsAsAny(actions), "rung": rung}
}

// answerOf decodes the one node a builtin answers with.
func answerOf(t *testing.T, nodes []memorynodes.MemoryNode) map[string]any {
	t.Helper()
	if len(nodes) != 1 {
		t.Fatalf("a builtin answers with one node, got %d", len(nodes))
	}
	var out map[string]any
	if err := json.Unmarshal(nodes[0].Payload, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// ---- refusals ----

// A trace is checked whole before anything is written or read: a trace the
// learner could not use, or a signature it cannot trust, never reaches the
// store. The G3 rows are what Task 6's strict parsePlan refuses, here as the
// builtin's refusals: an action that is not a string, and one that holds a comma
// or a vertical bar (the separators of a skill id).
func TestRecordTraceRefusesWhatItCannotUseAndWritesNothing(t *testing.T) {
	step := func(action any) map[string]any {
		return map[string]any{"action": action, "args": map[string]any{}, "ok": true, "durationMs": 1}
	}
	cases := []struct {
		name   string
		mutate func(a map[string]any)
		want   string
	}{
		{"no traceId", func(a map[string]any) { delete(a, "traceId") }, "games_invalid_argument: traceId, characterId, goalText, trigger and at are required"},
		{"a blank traceId", func(a map[string]any) { a["traceId"] = "  " }, "games_invalid_argument: traceId, characterId, goalText, trigger and at are required"},
		{"no characterId", func(a map[string]any) { delete(a, "characterId") }, "games_invalid_argument: traceId, characterId, goalText, trigger and at are required"},
		{"no goalText", func(a map[string]any) { delete(a, "goalText") }, "games_invalid_argument: traceId, characterId, goalText, trigger and at are required"},
		{"no trigger", func(a map[string]any) { delete(a, "trigger") }, "games_invalid_argument: traceId, characterId, goalText, trigger and at are required"},
		{"no at", func(a map[string]any) { delete(a, "at") }, "games_invalid_argument: traceId, characterId, goalText, trigger and at are required"},
		{"a traceId with colons", func(a map[string]any) { a["traceId"] = "wren:tick:5" }, "games_invalid_argument: traceId must be a bare id with no colons"},
		{"a traceId that is another concept's id", func(a map[string]any) { a["traceId"] = canonical(conceptSkill, "gs-1") }, "games_invalid_argument: traceId must be a bare id with no colons"},
		{"a traceId with colons after the row's own prefix", func(a map[string]any) { a["traceId"] = conceptPlanTrace + ":a:b" }, "games_invalid_argument: traceId must be a bare id with no colons"},
		{"a traceId with a NUL character", func(a map[string]any) { a["traceId"] = "t-\x00-1" }, "games_invalid_argument: traceId must be a bare id with no colons"},
		{"a traceId of 65 characters", func(a map[string]any) { a["traceId"] = strings.Repeat("t", 65) }, "games_invalid_argument: traceId must be a bare id with no colons"},
		{"a characterId of 65 characters", func(a map[string]any) { a["characterId"] = strings.Repeat("c", 65) }, "games_invalid_argument: characterId is 65 characters, at most 64"},
		{"a characterId of 65 characters that are not ASCII", func(a map[string]any) { a["characterId"] = strings.Repeat("\u00e9", 65) }, "games_invalid_argument: characterId is 65 characters, at most 64"},
		{"a goalText of 201 characters", func(a map[string]any) { a["goalText"] = strings.Repeat("g", 201) }, "games_invalid_argument: goalText is 201 characters, at most 200"},
		{"a trigger of 65 characters", func(a map[string]any) { a["trigger"] = strings.Repeat("r", 65) }, "games_invalid_argument: trigger is 65 characters, at most 64"},
		{"a say of 201 characters", func(a map[string]any) { a["say"] = strings.Repeat("s", 201) }, "games_invalid_argument: say is 201 characters, at most 200"},
		{"a goalText that is a number", func(a map[string]any) { a["goalText"] = 42 }, "games_invalid_argument: goalText must be a string"},
		{"a say that is a number", func(a map[string]any) { a["say"] = 42 }, "games_invalid_argument: say must be a string"},
		{"a trigger that is a boolean", func(a map[string]any) { a["trigger"] = true }, "games_invalid_argument: trigger must be a string"},
		{"a characterId that is a list", func(a map[string]any) { a["characterId"] = []any{"wren"} }, "games_invalid_argument: characterId must be a string"},
		{"an at that is a number", func(a map[string]any) { a["at"] = 42 }, "games_invalid_argument: at must be a string"},
		{"an at that is not a datetime", func(a map[string]any) { a["at"] = "yesterday" }, "games_invalid_argument: at must be an RFC 3339 datetime"},
		{"an at with no zone", func(a map[string]any) { a["at"] = "2026-10-19T12:00:00" }, "games_invalid_argument: at must be an RFC 3339 datetime"},
		{"an at on a day that is not on the calendar", func(a map[string]any) { a["at"] = "2026-02-30T12:00:00Z" }, "games_invalid_argument: at must be an RFC 3339 datetime"},
		{"inputs that are a string", func(a map[string]any) { a["inputs"] = "spot" }, "games_invalid_argument: inputs must be an object"},
		{"inputs that are a list", func(a map[string]any) { a["inputs"] = []any{"spot"} }, "games_invalid_argument: inputs must be an object"},
		{"inputs that are a number", func(a map[string]any) { a["inputs"] = 7 }, "games_invalid_argument: inputs must be an object"},
		{"a skillId that is a number", func(a map[string]any) { a["skillId"] = 42 }, "games_invalid_argument: skillId must be a string"},
		{"a skillId that is another concept's id", func(a map[string]any) { a["skillId"] = canonical(conceptPlanTrace, "x") }, "games_invalid_argument: skillId must be a bare id with no colons"},
		{"a skillId with colons", func(a map[string]any) { a["skillId"] = "gs:1:2" }, "games_invalid_argument: skillId must be a bare id with no colons"},
		{"no outcome", func(a map[string]any) { delete(a, "outcome") }, "games_invalid_argument: outcome must be success, failure or abandoned"},
		{"an outcome outside the set", func(a map[string]any) { a["outcome"] = "won" }, "games_invalid_argument: outcome must be success, failure or abandoned"},
		{"a reaction outside the set", func(a map[string]any) { a["reaction"] = "loved" }, "games_invalid_argument: reaction must be positive, neutral or negative"},
		{"no steps", func(a map[string]any) { delete(a, "steps") }, "games_invalid_argument: a plan must be a list"},
		{"steps that are not a list", func(a map[string]any) { a["steps"] = "collect" }, "games_invalid_argument: a plan must be a list"},
		{"an empty list of steps", func(a map[string]any) { a["steps"] = []any{} }, "games_invalid_argument: a trace has at least one step"},
		{"a step that is not an object", func(a map[string]any) { a["steps"] = []any{step("collect"), "craft"} }, "games_invalid_argument: plan step 1 is not an object"},
		{"a step with no action", func(a map[string]any) { a["steps"] = []any{map[string]any{"args": map[string]any{}}} }, "games_invalid_argument: plan step 0 names no action"},
		{"a blank action", func(a map[string]any) { a["steps"] = []any{step("collect"), step("   ")} }, "games_invalid_argument: plan step 1 names no action"},
		{"an action that is a number", func(a map[string]any) { a["steps"] = []any{step("collect"), step(42)} }, "games_invalid_argument: plan step 1 action must be a string"},
		{"an action that is a boolean", func(a map[string]any) { a["steps"] = []any{step(true)} }, "games_invalid_argument: plan step 0 action must be a string"},
		{"an action that is a list", func(a map[string]any) { a["steps"] = []any{step([]any{"collect"})} }, "games_invalid_argument: plan step 0 action must be a string"},
		{"an action with a comma", func(a map[string]any) { a["steps"] = []any{step("collect,craft")} }, `games_invalid_argument: plan step 0 action "collect,craft" holds a comma or a vertical bar`},
		{"an action with a vertical bar", func(a map[string]any) { a["steps"] = []any{step("collect"), step("craft|place")} }, `games_invalid_argument: plan step 1 action "craft|place" holds a comma or a vertical bar`},
		{"args that are not an object", func(a map[string]any) {
			a["steps"] = []any{map[string]any{"action": "collect", "args": []any{"coal"}}}
		}, "games_invalid_argument: plan step 0 args is not an object"},
		{"a signature that differs from the engine's", func(a map[string]any) { a["goalSignature"] = strings.Repeat("a", 64) }, "games_signature_mismatch: the supplied goalSignature " + strings.Repeat("a", 64)},
		{"a signature spelled in capitals", func(a map[string]any) {
			a["goalSignature"] = strings.ToUpper(GoalSignature("Light up the area!", []string{"spot"}))
		}, "games_signature_mismatch: "},
		{"a signature of other input keys", func(a map[string]any) {
			a["goalSignature"] = GoalSignature("Light up the area!", []string{"spot", "count"})
		}, "games_signature_mismatch: "},
		{"a signature of another goal", func(a map[string]any) { a["goalSignature"] = GoalSignature("Light up the cave!", []string{"spot"}) }, "games_signature_mismatch: "},
	}
	ctx := asOwner("user-games-trace-validate")
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			log := &traceLog{}
			p := log.provider()
			args := trace("t-1", torchPlan(4), "success", "neutral")
			tc.mutate(args)
			_, err := p.recordTrace(ctx, args, 0)
			if err == nil || !strings.HasPrefix(err.Error(), "games_") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one that leads with a games_ code and contains %q", err, tc.want)
			}
			if !log.untouched() {
				t.Fatalf("a refused trace reached the gate, the database or the store, before it was refused: %v", log.events)
			}
		})
	}

	// The control: with nothing mutated the same call is recorded, so every
	// refusal above is its mutation's doing. (A failure, so that it does not
	// reach the learner.)
	log := &traceLog{}
	if _, err := log.provider().recordTrace(ctx, trace("t-1", torchPlan(4), "failure", "neutral"), 0); err != nil || len(log.traces) != 1 {
		t.Fatalf("the unmutated trace = %v (wrote %d), want it recorded", err, len(log.traces))
	}
	// The edges of what a trace id may be: 64 characters, and the row's own
	// canonical id.
	for _, ok := range []string{strings.Repeat("t", 64), canonical(conceptPlanTrace, "t-2")} {
		log := &traceLog{}
		if _, err := log.provider().recordTrace(ctx, trace(ok, torchPlan(4), "failure", "neutral"), 0); err != nil || len(log.traces) != 1 {
			t.Fatalf("the trace id %.40q = %v (wrote %d), want it recorded", ok, err, len(log.traces))
		}
	}
}

// The limits are on CHARACTERS, as the mutation's @maxLength counts them, and on the
// value as it is written, trimmed: a field at its limit is accepted whatever its
// bytes and whatever blanks surround it.
func TestRecordTraceAcceptsEveryFieldAtItsLimit(t *testing.T) {
	ctx := asOwner("user-games-trace-limits")
	for _, f := range []struct {
		key string
		max int
	}{{"characterId", 64}, {"goalText", 200}, {"trigger", 64}, {"say", 200}} {
		for name, value := range map[string]string{
			"ascii":              strings.Repeat("a", f.max),
			"two bytes each":     strings.Repeat("\u00e9", f.max),
			"padded with blanks": "  " + strings.Repeat("a", f.max) + "  ",
		} {
			t.Run(f.key+"/"+name, func(t *testing.T) {
				log := &traceLog{}
				args := trace("t-1", torchPlan(4), "failure", "neutral")
				args[f.key] = value
				if _, err := log.provider().recordTrace(ctx, args, 0); err != nil || len(log.traces) != 1 {
					t.Fatalf("recordTrace = %v (wrote %d), want a field of %d characters recorded", err, len(log.traces), f.max)
				}
				if got := log.traces[0][f.key]; got != strings.TrimSpace(value) {
					t.Fatalf("%s was written as %.30q, want it trimmed", f.key, got)
				}
			})
		}
	}
}

// A refusal names the field and says how long it was; it never says the words. The
// free text of a trace is what a player and a character said, and a refusal is
// logged. What is echoed of an outcome or a reaction, which are not free text, is
// bounded.
func TestRecordTraceNeverEchoesWhatItRefusesToBeLong(t *testing.T) {
	ctx := asOwner("user-games-trace-echo")
	filler := strings.Repeat("\u00dc", 201)
	for _, key := range []string{"characterId", "goalText", "trigger", "say"} {
		log := &traceLog{}
		args := trace("t-1", torchPlan(4), "failure", "neutral")
		args[key] = filler
		_, err := log.provider().recordTrace(ctx, args, 0)
		if err == nil || !strings.HasPrefix(err.Error(), "games_invalid_argument: "+key+" is 201 characters, at most ") || strings.Contains(err.Error(), "\u00dc\u00dc\u00dc\u00dc\u00dc") {
			t.Errorf("%s of 201 characters = %v, want it named and its count given, and no word of it", key, err)
		}
	}
	for _, key := range []string{"outcome", "reaction"} {
		log := &traceLog{}
		args := trace("t-1", torchPlan(4), "failure", "neutral")
		args[key] = strings.Repeat("x", 300)
		_, err := log.provider().recordTrace(ctx, args, 0)
		if err == nil || !strings.HasPrefix(err.Error(), "games_invalid_argument: "+key+" must be ") || strings.Contains(err.Error(), strings.Repeat("x", 60)) {
			t.Errorf("a %s of 300 characters = %v, want a refusal that echoes at most 40 of them", key, err)
		}
	}
}

// A call with no person behind it is refused before anything else is looked at.
// The provider is the zero value: reaching its store would panic.
func TestRecordTraceRefusesACallWithNoActor(t *testing.T) {
	for label, ctx := range map[string]context.Context{
		"no actor in the context":         context.Background(),
		"an actor whose user id is blank": auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "   "}),
	} {
		t.Run(label, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("the call got past the actor check and reached the store: %v", r)
				}
			}()
			if _, err := (&Provider{}).recordTrace(ctx, trace("t-1", torchPlan(4), "failure", "neutral"), 0); err == nil || !strings.HasPrefix(err.Error(), "games_unauthenticated: ") {
				t.Fatalf("error = %v, want games_unauthenticated", err)
			}
		})
	}
}

// ---- what is written ----

// The row is what the engine computed, not what the caller claimed: the
// signature is recomputed (and a right one accepted), the actions are derived
// from the steps whatever the caller says they are, and the optional fields
// take their defaults.
func TestRecordTraceWritesWhatTheEngineComputed(t *testing.T) {
	ctx := asOwner("user-games-trace-write")
	sig := GoalSignature("Light up the area!", []string{"spot"})
	wantActions := []any{"collect", "craft", "placeTorches"}

	record := func(t *testing.T, mutate func(a map[string]any)) (map[string]any, map[string]any) {
		t.Helper()
		log := &traceLog{}
		p := log.provider()
		args := trace("  t-1  ", torchPlan(4), "failure", "")
		mutate(args)
		nodes, err := p.recordTrace(ctx, args, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(log.traces) != 1 || len(log.skills) != 0 || len(log.queries) != 0 {
			t.Fatalf("a trace that teaches nothing wrote %d traces, %d skills and read %v, want one trace", len(log.traces), len(log.skills), log.queries)
		}
		return log.traces[0], answerOf(t, nodes)
	}

	t.Run("the defaults and the derived fields", func(t *testing.T) {
		w, ans := record(t, func(a map[string]any) {
			a["actions"] = []any{"never", "trusted"} // not an argument of the builtin; never believed
			delete(a, "say")
		})
		want := map[string]any{
			"traceId": "t-1", "characterId": "wren", "goalText": "Light up the area!", "goalSignature": sig, "trigger": "initiative",
			"inputs": map[string]any{"spot": "here"}, "steps": stepsArg(torchPlan(4)), "actions": wantActions,
			"outcome": "failure", "reaction": "neutral", "at": w["at"],
		}
		if !reflect.DeepEqual(w, want) {
			t.Fatalf("written = %#v\nwant      %#v", w, want)
		}
		if ans["traceId"] != "t-1" || ans["goalSignature"] != sig || ans["learnedSkillId"] != "" || len(ans) != 3 {
			t.Fatalf("answer = %v, want {traceId, goalSignature, learnedSkillId: \"\"}", ans)
		}
	})
	t.Run("the signature the caller supplied, when it is right", func(t *testing.T) {
		w, _ := record(t, func(a map[string]any) { a["goalSignature"] = " " + sig + " " })
		if w["goalSignature"] != sig {
			t.Fatalf("goalSignature = %v, want %s", w["goalSignature"], sig)
		}
	})
	t.Run("no inputs, or a null, is an empty object, and a signature over no keys", func(t *testing.T) {
		noKeys := GoalSignature("Light up the area!", nil)
		for name, mutate := range map[string]func(a map[string]any){
			"absent": func(a map[string]any) { delete(a, "inputs") },
			"null":   func(a map[string]any) { a["inputs"] = nil },
		} {
			w, ans := record(t, mutate)
			if !reflect.DeepEqual(w["inputs"], map[string]any{}) || w["goalSignature"] != noKeys || ans["goalSignature"] != noKeys {
				t.Fatalf("%s: inputs %#v and signature %v, want {} and %s", name, w["inputs"], w["goalSignature"], noKeys)
			}
		}
	})
	t.Run("the time is stored in one fixed UTC form, however it was written", func(t *testing.T) {
		for sent, want := range map[string]string{
			"2026-10-19T14:00:00.123456789+02:00": "2026-10-19T12:00:00.123Z",
			"2026-10-19t12:00:00z":                "2026-10-19T12:00:00.000Z",
			"2026-10-19T12:00:00Z":                "2026-10-19T12:00:00.000Z",
			"2026-10-19T12:00:00.5-03:30":         "2026-10-19T15:30:00.500Z",
			" 2026-10-19T12:00:00Z ":              "2026-10-19T12:00:00.000Z",
		} {
			w, _ := record(t, func(a map[string]any) { a["at"] = sent })
			if w["at"] != want {
				t.Errorf("at %q was written as %v, want %s", sent, w["at"], want)
			}
		}
	})
	t.Run("the keys of the inputs make the signature, not their values", func(t *testing.T) {
		a, _ := record(t, func(a map[string]any) { a["inputs"] = map[string]any{"spot": "the cave"} })
		b, _ := record(t, func(a map[string]any) { a["inputs"] = map[string]any{"spot": "the den"} })
		c, _ := record(t, func(a map[string]any) { a["inputs"] = map[string]any{"spot": "the den", "count": 2} })
		if a["goalSignature"] != b["goalSignature"] || a["goalSignature"] == c["goalSignature"] {
			t.Fatalf("signatures %v, %v, %v: equal keys must agree and another key must not", a["goalSignature"], b["goalSignature"], c["goalSignature"])
		}
	})
	t.Run("a skill run names its skill by the bare id", func(t *testing.T) {
		w, _ := record(t, func(a map[string]any) { a["skillId"] = " " + canonical(conceptSkill, "gs-abc") + " " })
		if w["skillId"] != "gs-abc" {
			t.Fatalf("skillId = %v, want gs-abc", w["skillId"])
		}
	})
	t.Run("what the character said is kept, trimmed, only when there is something", func(t *testing.T) {
		w, _ := record(t, func(a map[string]any) { a["say"] = "  On it!  " })
		if w["say"] != "On it!" {
			t.Fatalf("say = %v", w["say"])
		}
		w, _ = record(t, func(a map[string]any) { a["say"] = "   " })
		if _, ok := w["say"]; ok {
			t.Fatalf("a blank say was written: %v", w["say"])
		}
	})
	t.Run("steps are stored as sent, and the actions are trimmed names", func(t *testing.T) {
		steps := stepsArg(torchPlan(4))
		steps[1].(map[string]any)["action"] = "  craft  "
		w, _ := record(t, func(a map[string]any) { a["steps"] = steps })
		if !reflect.DeepEqual(w["steps"], steps) || !reflect.DeepEqual(w["actions"], wantActions) {
			t.Fatalf("steps %#v actions %#v", w["steps"], w["actions"])
		}
	})
}

// ---- when it learns ----

// Learning needs two traces that were successes, were not disliked and did not
// run a skill. This is the part of that decision a trace makes by itself: only a
// trace that qualifies reaches the learner, and the learner's own gate is taken
// AFTER the gate on the trace id was let go, never inside it. A learner whose gate
// cannot be taken leaves the trace recorded and says so.
func TestRecordTraceReachesTheLearnerOnlyWhenItCanTeach(t *testing.T) {
	ctx := asOwner("user-games-trace-learns")
	sig := GoalSignature("Light up the area!", []string{"spot"})
	for _, tc := range []struct {
		name   string
		mutate func(a map[string]any)
		learns bool
	}{
		{"a success the player did not mind", func(a map[string]any) {}, true},
		{"a success the player liked", func(a map[string]any) { a["reaction"] = "positive" }, true},
		{"a success with no reaction recorded", func(a map[string]any) { delete(a, "reaction") }, true},
		{"a failure", func(a map[string]any) { a["outcome"] = "failure" }, false},
		{"an abandoned plan", func(a map[string]any) { a["outcome"] = "abandoned" }, false},
		{"a success the player disliked", func(a map[string]any) { a["reaction"] = "negative" }, false},
		{"a skill run", func(a map[string]any) { a["skillId"] = "gs-abc" }, false},
		{"a skill run named by its canonical id", func(a map[string]any) { a["skillId"] = canonical(conceptSkill, "gs-abc") }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := &traceLog{}
			args := trace("t-1", torchPlan(4), "success", "neutral")
			tc.mutate(args)
			nodes, err := log.provider().recordTrace(ctx, args, 0)
			if err != nil || answerOf(t, nodes)["learnedSkillId"] != "" {
				t.Fatalf("recordTrace = %v, %v, want it recorded and nothing learned from one trace", nodes, err)
			}
			recorded := []string{"lock games-trace:t-1", "holder " + canonical(conceptPlanTrace, "t-1"), "write t-1", "release games-trace:t-1"}
			if !tc.learns {
				if !sameStrings(log.events, recorded) {
					t.Fatalf("events = %v, want the trace recorded and nothing else", log.events)
				}
				return
			}
			learner := "games-learn:user-games-trace-learns:" + sig
			want := append(slices.Clone(recorded), "read gamesSkillsForSignature", "lock "+learner, "read gamesSkillsForSignature", "read gamesLearningTraces", "release "+learner)
			if !sameStrings(log.events, want) {
				t.Fatalf("events = %v\nwant     %v\nthe learner looks for a live skill without its gate, takes it after the trace id's is let go, and reads again inside it", log.events, want)
			}
		})
	}

	// A learner whose gate cannot be taken: the trace is recorded and the error says so.
	log := &traceLog{learnErr: fmt.Errorf("%w: the learner's lock is taken", memql.ErrWriteGateBusy)}
	_, err := log.provider().recordTrace(ctx, trace("t-1", torchPlan(4), "success", "neutral"), 0)
	if !errors.Is(err, memql.ErrWriteGateBusy) || !strings.Contains(err.Error(), "the trace is recorded and will count next time") || len(log.traces) != 1 {
		t.Fatalf("a learner without its gate = %v (wrote %d), want the busy answer saying the trace is recorded", err, len(log.traces))
	}
}

// A gate with no pool wired says which wiring is missing, the way the other
// packs and integrations that take gates do.
func TestGateWithNoPoolNamesTheWiringItLacks(t *testing.T) {
	release, err := (&Provider{}).gate(context.Background(), "games-learn:o:s")
	if release != nil || !errors.Is(err, memql.ErrWriteGatePoolMissing) || !strings.Contains(err.Error(), "PluginContext.GateDB") {
		t.Fatalf("a pack built without a gate pool = %v, want ErrWriteGatePoolMissing naming PluginContext.GateDB", err)
	}
	release, err = (&Provider{gates: func() *sql.DB { return nil }}).gate(context.Background(), "games-learn:o:s")
	if release != nil || !errors.Is(err, memql.ErrWriteGatePoolMissing) || !strings.Contains(err.Error(), "failed to open the write-gate pool") {
		t.Fatalf("a resolver with no pool = %v, want ErrWriteGatePoolMissing saying the pool was never opened", err)
	}
}

// ---- the stamp ----

// recordingEngine is the engine handle of a pack: it remembers each statement and
// the origin and actor of the context it arrived on.
type recordingEngine struct {
	memql.IntegrationEngineAccess
	statements []string
	internal   []bool
	actors     []string
	err        error
}

func (r *recordingEngine) Execute(ctx context.Context, statement string) (*memql.ExecuteResult, error) {
	r.statements = append(r.statements, statement)
	r.internal = append(r.internal, auth.OriginFromContext(ctx).IsInternal())
	r.actors = append(r.actors, actorUserID(ctx))
	return nil, r.err
}

// Each internal-origin writer stamps ONE call, of its own construct, and leaves the
// caller's context alone: the stamp is a local of the call, so nothing the caller
// does next inherits it, and the actor is the caller's own (the allowlist entry in
// call_origin_conformance_test.go rests on both).
func TestTheInternalOriginWritersStampOneCallAndKeepTheActor(t *testing.T) {
	writers := map[string]func(s engineStore, ctx context.Context, args map[string]any) error{
		"createGamesPlanTrace":   engineStore.createTrace,
		"createGamesSkill":       engineStore.createSkill,
		"recordGamesSkillLadder": engineStore.recordLadder,
	}
	for construct, write := range writers {
		t.Run(construct, func(t *testing.T) {
			engine := &recordingEngine{}
			ctx := asOwner("user-games-stamp")
			err := write(engineStore{engine: engine}, ctx, map[string]any{"id": "x-1", "absent": nil, "count": 3})
			if err != nil {
				t.Fatal(err)
			}
			if len(engine.statements) != 1 || !engine.internal[0] || engine.actors[0] != "user-games-stamp" {
				t.Fatalf("calls %v internal %v actors %v, want one call, stamped, as the caller", engine.statements, engine.internal, engine.actors)
			}
			if st := engine.statements[0]; !strings.HasPrefix(st, "mutation "+construct+"(") || !strings.Contains(st, `id: "x-1"`) || strings.Contains(st, "absent") {
				t.Fatalf("statement = %s, want a call of %s that leaves out the absent argument", st, construct)
			}
			if auth.OriginFromContext(ctx).IsInternal() {
				t.Fatal("the caller's own context was stamped")
			}

			// A failure names the construct and keeps its cause.
			cause := errors.New("the database is away")
			err = write(engineStore{engine: &recordingEngine{err: cause}}, ctx, map[string]any{"id": "x-1"})
			if !errors.Is(err, cause) || !strings.HasPrefix(err.Error(), fmt.Sprintf("games: mutation %s: ", construct)) {
				t.Fatalf("error = %v, want games: mutation %s: wrapping the cause", err, construct)
			}
			// No engine on this node is an error, not a panic.
			if err := write(engineStore{}, ctx, map[string]any{"id": "x-1"}); err == nil || !strings.Contains(err.Error(), "no engine handle") {
				t.Fatalf("error = %v, want no engine handle", err)
			}
		})
	}
}

// ---- a trace id is one owner's ----

// A trace is written in a fixed order: the gate on the BARE id (a bare and a canonical
// spelling are one row), then who holds the row now -- read fresh, by the row's
// canonical id -- then the write, then the gate let go. Looking and writing are one
// step only if the write happens inside the gate and after the look.
func TestRecordTraceClaimsTheIdBeforeItWritesAndLetsGoAfter(t *testing.T) {
	ctx := asOwner("user-games-claim")
	for name, sent := range map[string]string{"bare": "t-1", "canonical": canonical(conceptPlanTrace, "t-1")} {
		t.Run(name, func(t *testing.T) {
			log := &traceLog{}
			if _, err := log.provider().recordTrace(ctx, trace(sent, torchPlan(4), "failure", "neutral"), 0); err != nil {
				t.Fatal(err)
			}
			want := []string{"lock games-trace:t-1", "holder " + canonical(conceptPlanTrace, "t-1"), "write " + sent, "release games-trace:t-1"}
			if !sameStrings(log.events, want) {
				t.Fatalf("events = %v, want %v", log.events, want)
			}
			if len(log.holderFresh) != 1 || !log.holderFresh[0] {
				t.Fatalf("who holds the row was looked up with fresh = %v, want one fresh read", log.holderFresh)
			}
			if !sameStrings(log.holderConcepts, []string{conceptPlanTrace}) {
				t.Fatalf("who holds the row was asked about %v, want the plan trace concept", log.holderConcepts)
			}
		})
	}
}

// A different owner holds the row: refused, whichever way that owner's id is spelled
// on the row, and also a row that names no owner at all (nothing is written on a
// guess). Nothing is written, the learner is not asked, the gate is let go, and the
// refusal says nothing about who holds the id.
func TestRecordTraceRefusesATraceIdAnotherOwnerHolds(t *testing.T) {
	ctx := asOwner("user-games-claim-b")
	for name, holder := range map[string]string{
		"their canonical id":  ownerKey("user-games-claim-a"),
		"their bare id":       "user-games-claim-a",
		"a row with no owner": "",
	} {
		t.Run(name, func(t *testing.T) {
			log := &traceLog{holders: map[string]string{canonical(conceptPlanTrace, "t-1"): holder}}
			nodes, err := log.provider().recordTrace(ctx, trace("t-1", torchPlan(4), "success", "neutral"), 0)
			if err == nil || nodes != nil || !strings.HasPrefix(err.Error(), "games_invalid_argument: that trace id is taken") {
				t.Fatalf("recordTrace = %v, %v, want games_invalid_argument: that trace id is taken", nodes, err)
			}
			if holder != "" && strings.Contains(err.Error(), "user-games-claim-a") {
				t.Fatalf("the refusal names the owner who holds the id: %v", err)
			}
			want := []string{"lock games-trace:t-1", "holder " + canonical(conceptPlanTrace, "t-1"), "release games-trace:t-1"}
			if !sameStrings(log.events, want) || len(log.traces) != 0 {
				t.Fatalf("events = %v (wrote %d), want the look and the release and no write", log.events, len(log.traces))
			}
		})
	}
}

// The owner's own id is theirs to send again, whichever way the row spells them.
func TestRecordTraceAcceptsATraceIdTheCallerHolds(t *testing.T) {
	ctx := asOwner("user-games-claim-a")
	for name, holder := range map[string]string{"canonical": ownerKey("user-games-claim-a"), "bare": "user-games-claim-a"} {
		t.Run(name, func(t *testing.T) {
			log := &traceLog{holders: map[string]string{canonical(conceptPlanTrace, "t-1"): holder}}
			if _, err := log.provider().recordTrace(ctx, trace("t-1", torchPlan(4), "failure", "neutral"), 0); err != nil || len(log.traces) != 1 {
				t.Fatalf("recordTrace = %v (wrote %d), want the owner's re-send recorded", err, len(log.traces))
			}
		})
	}
}

// When who holds the id cannot be read, or the id cannot be locked, nothing is
// written: the pack does not record on a guess. A lock that is busy keeps its
// type, so a caller can tell "try again" from a failure.
func TestRecordTraceWritesNothingWhenTheIdCannotBeClaimed(t *testing.T) {
	ctx := asOwner("user-games-claim-a")

	log := &traceLog{holderErr: errors.New("the database is away")}
	_, err := log.provider().recordTrace(ctx, trace("t-1", torchPlan(4), "failure", "neutral"), 0)
	if err == nil || !strings.Contains(err.Error(), "the database is away") || len(log.traces) != 0 {
		t.Fatalf("a look that failed = %v (wrote %d), want its error and no write", err, len(log.traces))
	}
	if want := []string{"lock games-trace:t-1", "holder " + canonical(conceptPlanTrace, "t-1"), "release games-trace:t-1"}; !sameStrings(log.events, want) {
		t.Fatalf("events = %v, want %v: the gate is let go on the way out", log.events, want)
	}

	log = &traceLog{lockErr: fmt.Errorf("%w: another replica holds it", memql.ErrWriteGateBusy)}
	_, err = log.provider().recordTrace(ctx, trace("t-1", torchPlan(4), "failure", "neutral"), 0)
	if !errors.Is(err, memql.ErrWriteGateBusy) || !memql.IsWriteGateBusy(err) || !strings.Contains(err.Error(), "the trace was not recorded") || !log.untouched() {
		t.Fatalf("a gate that is busy = %v (events %v), want the busy answer saying the trace was not recorded, and nothing done", err, log.events)
	}
}

// Who holds a row is read from the database, and without one it cannot be read.
func TestRowHolderNeedsADatabaseOrItsSeam(t *testing.T) {
	ctx := context.Background()
	for name, p := range map[string]*Provider{
		"a pack with no database handle": {},
		"a handle that answers nil":      {db: func() *sql.DB { return nil }},
	} {
		if _, _, err := p.rowHolder(ctx, conceptPlanTrace, "x"); err == nil || !strings.Contains(err.Error(), "no database handle") {
			t.Errorf("%s: %v, want no database handle", name, err)
		}
	}
	p := &Provider{holderOf: func(context.Context, string, string) (string, bool, error) { return "someone", true, nil }}
	if owner, found, err := p.rowHolder(ctx, conceptPlanTrace, "x"); owner != "someone" || !found || err != nil {
		t.Errorf("the seam was not used: %q %v %v", owner, found, err)
	}
}

// ---- the learner ----

// torchActions are the actions of the torch plans the tests learn from.
var torchActions = []string{"collect", "craft", "placeTorches"}

// twoAgreeingTraces are the evidence of one goal as gamesLearningTraces reads it:
// the newest first. They differ in the counts (the holes), in what was said, in the
// trigger, and in how the goal was spelled (one signature).
func twoAgreeingTraces(owner string) []map[string]any {
	return []map[string]any{
		evidenceRow(owner, "t-2", torchPlan(6), "Light up the area!", "Torches coming up!", "initiative"),
		evidenceRow(owner, "t-1", torchPlan(4), "light up the area", "On it.", "addressedChat"),
	}
}

// The learner looks for a live skill WITHOUT its gate, takes the gate only when there
// may be a skill to make, reads again inside it, writes the skill, and lets the gate
// go BEFORE it embeds the skill's goal. The embedding is a call to a model; the gate
// is one of a pool of four that every trace's own gate is taken from too, and
// nothing about an embedding needs the lock. What the skill is made of is the newest
// trace's words, every trigger once, the trace ids, and the template of the plans.
func TestLearnCreatesTheSkillInsideItsGateAndEmbedsAfterLettingGo(t *testing.T) {
	const owner = "user-games-learn-unit"
	sig := GoalSignature("Light up the area!", []string{"spot"})
	log := &traceLog{rows: map[string][]map[string]any{"gamesLearningTraces": twoAgreeingTraces(owner)}}

	got, err := log.provider().learn(asOwner(owner), owner, sig, torchActions)
	skillID := SkillID(owner, sig, torchActions, 1)
	if err != nil || got != skillID {
		t.Fatalf("learn = %q, %v, want the skill at %s", got, err, skillID)
	}
	gate := "games-learn:" + owner + ":" + sig
	want := []string{
		"read gamesSkillsForSignature", // the look for a live skill, with no gate
		"lock " + gate,
		"read gamesSkillsForSignature", // and again, inside it
		"read gamesLearningTraces",
		"create skill",
		"release " + gate,
		"embed " + canonical(conceptSkill, skillID),
	}
	if !sameStrings(log.events, want) {
		t.Fatalf("events = %v\nwant     %v\nthe gate is let go as soon as the skill is written, and before the model is asked", log.events, want)
	}
	if !slices.Equal(log.readFresh, []bool{false, true, true}) {
		t.Fatalf("the reads were fresh = %v, want the look from the cache and both reads inside the gate fresh", log.readFresh)
	}

	tmpl, err := Generalize([][]PlanStep{torchPlan(6), torchPlan(4)})
	if err != nil {
		t.Fatal(err)
	}
	tmplMap, err := tmpl.AsMap()
	if err != nil {
		t.Fatal(err)
	}
	wantRow := map[string]any{
		"skillId": skillID, "goalText": "Light up the area!", "goalSignature": sig, "actions": stringsAsAny(torchActions),
		"template": tmplMap, "templateDigest": tmpl.Digest(), "triggers": []any{"addressedChat", "initiative"},
		"learnedFrom": []any{"t-2", "t-1"}, "confirmation": "Torches coming up!",
	}
	if len(log.skills) != 1 || !reflect.DeepEqual(log.skills[0], wantRow) {
		t.Fatalf("the skill written was %#v\nwant %#v", log.skills, wantRow)
	}
	if !sameStrings(log.embeds, []string{"Light up the area!"}) {
		t.Fatalf("the embedding was asked for %v, want the goal text of the newest trace", log.embeds)
	}
}

// A skill that is live and of these actions means there is nothing to learn, and the
// learner says so without the gate: most qualifying traces arrive after the skill
// exists, and each of them taking one of the four gate connections for a look is the
// pool that every trace's own gate shares. What does not stop the learner: another
// owner's skill (a cluster owner sees them), another sequence of actions, and a skill
// that is retired (the goal is learned anew at the next generation).
func TestLearnTakesNoGateWhenALiveSkillAlreadyCoversTheGoal(t *testing.T) {
	const owner = "user-games-learn-unit"
	sig := GoalSignature("Light up the area!", []string{"spot"})
	gate := "games-learn:" + owner + ":" + sig
	for _, tc := range []struct {
		name       string
		skills     []map[string]any
		gated      bool
		generation int
	}{
		{"a shadow skill of these actions", []map[string]any{skillRow(owner, "gs-1", torchActions, "shadow")}, false, 0},
		{"a canary skill", []map[string]any{skillRow(owner, "gs-1", torchActions, "canary")}, false, 0},
		{"a trusted skill", []map[string]any{skillRow(owner, "gs-1", torchActions, "trusted")}, false, 0},
		{"a retired skill and a live one of the second generation", []map[string]any{skillRow(owner, "gs-1", torchActions, "retired"), skillRow(owner, "gs-2", torchActions, "shadow")}, false, 0},
		{"a retired skill", []map[string]any{skillRow(owner, "gs-1", torchActions, "retired")}, true, 2},
		{"another owner's live skill", []map[string]any{skillRow("user-games-someone-else", "gs-1", torchActions, "shadow")}, true, 1},
		{"a live skill of other actions", []map[string]any{skillRow(owner, "gs-1", []string{"collect"}, "shadow")}, true, 1},
		{"no skill", nil, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := &traceLog{rows: map[string][]map[string]any{
				"gamesSkillsForSignature": tc.skills,
				"gamesLearningTraces":     twoAgreeingTraces(owner),
			}}
			got, err := log.provider().learn(asOwner(owner), owner, sig, torchActions)
			if err != nil {
				t.Fatal(err)
			}
			if !tc.gated {
				if got != "" || !sameStrings(log.events, []string{"read gamesSkillsForSignature"}) {
					t.Fatalf("learn = %q, events %v, want the one look and no gate", got, log.events)
				}
				return
			}
			if want := SkillID(owner, sig, torchActions, tc.generation); got != want || !slices.Contains(log.events, "lock "+gate) {
				t.Fatalf("learn = %q (events %v), want the skill at generation %d, %s, learned inside the gate", got, log.events, tc.generation, want)
			}
		})
	}
}

// Every failure of the learner is the learner's answer, and none of them is read as
// "nothing there". A failed read of the skills taken for none would write the skill
// again at its one deterministic id, which puts a skill the ladder has trusted back
// in shadow with nothing counted; a failed read of the evidence taken for none would
// hide that there was a pair; a failed write taken for a success would embed, and
// say, a skill that is not there. In each the gate that was taken is let go, nothing
// is written that should not be, and nothing is embedded.
func TestLearnReturnsItsFailuresAndWritesNothing(t *testing.T) {
	const owner = "user-games-learn-unit"
	sig := GoalSignature("Light up the area!", []string{"spot"})
	gate := "games-learn:" + owner + ":" + sig
	away := errors.New("the database is away")
	failRead := func(name string, call int) func(*traceLog) {
		return func(l *traceLog) {
			l.queryErr = func(n string, c int) error {
				if n == name && c == call {
					return away
				}
				return nil
			}
		}
	}
	for _, tc := range []struct {
		name    string
		fail    func(l *traceLog)
		want    string
		gated   bool // the gate was taken, so it has to have been let go
		created int  // skills written before the failure
	}{
		{"the look for a live skill fails", failRead("gamesSkillsForSignature", 1), "the database is away", false, 0},
		{"the read of the skills inside the gate fails", failRead("gamesSkillsForSignature", 2), "the database is away", true, 0},
		{"the read of the evidence fails", failRead("gamesLearningTraces", 1), "the database is away", true, 0},
		{"the skill cannot be written", func(l *traceLog) { l.skillErr = away }, "the database is away", true, 1},
		{"the gate cannot be taken", func(l *traceLog) { l.learnErr = fmt.Errorf("%w: another replica holds it", memql.ErrWriteGateBusy) }, "learning could not be locked; the trace is recorded and will count next time", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := &traceLog{rows: map[string][]map[string]any{"gamesLearningTraces": twoAgreeingTraces(owner)}}
			tc.fail(log)
			got, err := log.provider().learn(asOwner(owner), owner, sig, torchActions)
			if err == nil || got != "" || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("learn = %q, %v, want no skill and an error saying %q", got, err, tc.want)
			}
			if len(log.skills) != tc.created || len(log.embeds) != 0 {
				t.Fatalf("learn wrote %d skills and embedded %d, want %d and none (events %v)", len(log.skills), len(log.embeds), tc.created, log.events)
			}
			if released := slices.Contains(log.events, "release "+gate); released != tc.gated {
				t.Fatalf("events = %v: the gate was released = %v, want %v", log.events, released, tc.gated)
			}
		})
	}

	// The model away is no failure of the learner: the skill is learned, the
	// embedding is tried after the gate is let go, and gamesEmbedPending back-fills it.
	log := &traceLog{rows: map[string][]map[string]any{"gamesLearningTraces": twoAgreeingTraces(owner)}, embedErr: errors.New("no model is reachable")}
	got, err := log.provider().learn(asOwner(owner), owner, sig, torchActions)
	if want := SkillID(owner, sig, torchActions, 1); err != nil || got != want || len(log.skills) != 1 || len(log.embeds) != 1 {
		t.Fatalf("learn with the model away = %q, %v (wrote %d, tried to embed %d), want the skill learned", got, err, len(log.skills), len(log.embeds))
	}
}

// ---- the walk ----

// pagingEngine is the engine handle of a pack that answers a paginated read page by
// page: the page for the cursor on the context, the first for none. It remembers each
// call's statement, cursor, actor and whether the read was marked fresh.
type pagingEngine struct {
	memql.IntegrationEngineAccess
	pages      map[string]*memql.ExecuteResult // by the cursor that asks for the page
	statements []string
	cursors    []string
	actors     []string
	fresh      []bool
	failAt     int // the n-th call (from 1) fails; 0: none
	failure    error
}

func (e *pagingEngine) Execute(ctx context.Context, statement string) (*memql.ExecuteResult, error) {
	e.statements = append(e.statements, statement)
	cursor := memql.CursorFromContext(ctx)
	e.cursors = append(e.cursors, cursor)
	e.actors = append(e.actors, actorUserID(ctx))
	e.fresh = append(e.fresh, memql.FreshReadFromContext(ctx))
	if e.failAt != 0 && len(e.statements) == e.failAt {
		return nil, e.failure
	}
	page, ok := e.pages[cursor]
	if !ok {
		return nil, fmt.Errorf("no page for the cursor %q", cursor)
	}
	return page, nil
}

func page(cursor string, hasMore bool) *memql.ExecuteResult {
	return &memql.ExecuteResult{Meta: &memql.ResultMeta{Cursor: cursor, HasMore: hasMore}}
}

// walk reads every page, from the first to the last, each as the caller and fresh, by
// the cursor the page before it gave; and the rows of each page reach the visitor in
// order. A walk that cannot reach the end says so: a page that has more after it and no
// way to reach them is an error and not a short list, because a list of skills to sweep
// that stopped quietly would leave the rest unswept.
func TestWalkReadsEveryPageFromTheFirstToTheLastAsTheCaller(t *testing.T) {
	eng := &pagingEngine{pages: map[string]*memql.ExecuteResult{"": page("c1", true), "c1": page("c2", true), "c2": page("", false)}}
	visits := 0
	err := engineStore{engine: eng}.walk(asOwner("user-games-walk"), "gamesSkillsToSweep", nil, func([]map[string]any) error {
		visits++
		return nil
	})
	if err != nil || visits != 3 {
		t.Fatalf("walk = %v after %d pages, want 3 and no error", err, visits)
	}
	if !sameStrings(eng.cursors, []string{"", "c1", "c2"}) {
		t.Fatalf("the pages were asked for with the cursors %q, want none, then each the page before gave", eng.cursors)
	}
	for i := range eng.statements {
		if eng.statements[i] != "query gamesSkillsToSweep()" || eng.actors[i] != "user-games-walk" || !eng.fresh[i] {
			t.Errorf("page %d: %q as %q, fresh %v", i+1, eng.statements[i], eng.actors[i], eng.fresh[i])
		}
	}
}

func TestWalkFailsWhenItCannotReachTheEnd(t *testing.T) {
	ctx := asOwner("user-games-walk")
	for name, tc := range map[string]struct {
		eng     *pagingEngine
		visit   func([]map[string]any) error
		want    string
		visited int
	}{
		"a page that has more and no cursor": {
			eng:  &pagingEngine{pages: map[string]*memql.ExecuteResult{"": page("c1", true), "c1": page("", true)}},
			want: "cannot continue", visited: 1,
		},
		"a cursor that does not advance": {
			eng:  &pagingEngine{pages: map[string]*memql.ExecuteResult{"": page("c1", true), "c1": page("c1", true)}},
			want: "did not advance", visited: 1,
		},
		"an engine that fails on the second page": {
			eng:  &pagingEngine{pages: map[string]*memql.ExecuteResult{"": page("c1", true)}, failAt: 2, failure: errors.New("the database is away")},
			want: "the database is away", visited: 1,
		},
		"a visitor that refuses": {
			eng:   &pagingEngine{pages: map[string]*memql.ExecuteResult{"": page("c1", true), "c1": page("", false)}},
			visit: func([]map[string]any) error { return errors.New("the visitor stopped") },
			want:  "the visitor stopped", visited: 1,
		},
	} {
		t.Run(name, func(t *testing.T) {
			visited := 0
			visit := tc.visit
			if visit == nil {
				visit = func([]map[string]any) error { return nil }
			}
			err := engineStore{engine: tc.eng}.walk(ctx, "gamesSkillsToSweep", nil, func(rows []map[string]any) error {
				visited++
				return visit(rows)
			})
			if err == nil || !strings.HasPrefix(err.Error(), "games: query gamesSkillsToSweep: ") || !strings.Contains(err.Error(), tc.want) || visited != tc.visited {
				t.Fatalf("walk = %v after %d pages, want games: query gamesSkillsToSweep: ...%s after %d", err, visited, tc.want, tc.visited)
			}
		})
	}

	// a walk that never ends is stopped, with an error naming the budget
	endless := &pagingEngine{pages: map[string]*memql.ExecuteResult{}}
	endless.pages[""] = page("c0", true)
	for i := 0; i < maxWalkPages+1; i++ {
		endless.pages[fmt.Sprintf("c%d", i)] = page(fmt.Sprintf("c%d", i+1), true)
	}
	if err := (engineStore{engine: endless}).walk(ctx, "gamesSkillsToSweep", nil, func([]map[string]any) error { return nil }); err == nil || !strings.Contains(err.Error(), "page budget") {
		t.Fatalf("a walk of more pages than the budget = %v, want an error naming the page budget", err)
	}
	if len(endless.statements) != maxWalkPages {
		t.Fatalf("the walk read %d pages, want it stopped at the budget of %d", len(endless.statements), maxWalkPages)
	}

	// no engine on this node is an error, not a panic
	if err := (engineStore{}).walk(ctx, "gamesSkillsToSweep", nil, func([]map[string]any) error { return nil }); err == nil || !strings.Contains(err.Error(), "no engine handle") {
		t.Fatalf("walk with no engine = %v, want no engine handle", err)
	}
}
