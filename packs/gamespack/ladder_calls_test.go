package gamespack

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/work"
)

// builtinFn is the shape of a builtin's handler.
type builtinFn = func(context.Context, map[string]any, int) ([]memorynodes.MemoryNode, error)

// ladder_calls_test.go -- the four builtins over the in-memory store of
// ladder_moves_test.go: the arguments each refuses before anything is touched, which
// skills each moves, and the whole state each move writes.

// wrote holds one write's arguments to what a move must have written: every key in want
// equal to it (a number is compared as the engine reads it back), and none of absent
// there at all.
func wrote(t *testing.T, label string, got, want map[string]any, absent ...string) {
	t.Helper()
	for k, w := range want {
		if g, ok := got[k]; !ok || !reflect.DeepEqual(cloneValue2(g), cloneValue2(w)) {
			t.Errorf("%s: %s = %v (%T), want %v", label, k, got[k], got[k], w)
		}
	}
	for _, k := range absent {
		if v, ok := got[k]; ok {
			t.Errorf("%s: %s = %v was written, want it left alone", label, k, v)
		}
	}
}

// The whole ladder, one move at a time, with the arguments of every write that matters:
// a move writes the WHOLE state Advance returned and not a delta -- the cleared streak
// as an EXPLICIT empty object, the closed promotion as the empty string -- because the
// mutation is a read-merge that leaves alone any field it is not given, and a streak
// the writer forgot to clear is evidence the ladder never earned. Fields that did not
// change are left out where they would mislead: a rung that did not change has no
// rungChangedAt, and only a proposal has a promotionProposedAt.
func TestTheLadderWritesTheWholeStateEachMoveReturned(t *testing.T) {
	row := skillFull(ladderOwner, "gs-1", nil)
	st := newLadderStore(row)
	st.listed = []map[string]any{row}
	p := st.provider()
	ctx := asOwner(ladderOwner)
	sig, digest := trimmed(row["goalSignature"]), digestOf(row)
	d4, d6 := work.BindingDigest("4"), work.BindingDigest("6")
	at := rfc3339(ladderNow)

	compare := func(count float64) map[string]any {
		return builtinAnswer(t, p.shadowCompare, ctx, map[string]any{"goalSignature": sig, "plan": planArg(torchPlan(count))})
	}
	last := func() map[string]any { return st.writes[len(st.writes)-1] }
	var versions []string

	// Four matches on one value of each hole, then a fifth on another.
	for i, count := range []float64{4, 4, 4, 4, 6} {
		got := compare(count)
		versions = append(versions, last()["versionAt"].(string))
		if asInt(got["compared"], -1) != 1 || asInt(got["matched"], -1) != 1 {
			t.Fatalf("comparison %d answered %v", i+1, got)
		}
		proposed, _ := got["proposed"].([]any)
		if want := i == 4; (len(proposed) == 1) != want {
			t.Fatalf("comparison %d proposed %v, want a proposal only at the fifth", i+1, proposed)
		}
		switch i {
		case 0:
			wrote(t, "the first match", last(), map[string]any{
				"skillId": "gs-1", "rung": "shadow", "state": "shadow", "shadowMatches": 1, "canaryMatches": 0, "failures": 0,
				"distinctBindings": map[string]any{"1.count": []any{d4}, "2.count": []any{d4}}, "promotionApprovalId": "",
				"lastReplayAt": at, "lastBinding": map[string]any{"1.count": 4, "2.count": 4},
			}, "rungChangedAt", "promotionProposedAt")
			if !strings.Contains(trimmed(last()["lastReason"]), "1 of the 5") {
				t.Errorf("the first match's reason = %q", last()["lastReason"])
			}
		case 4:
			wrote(t, "the proposal", last(), map[string]any{
				"rung": "shadow", "state": "awaiting_approval", "shadowMatches": 5, "failures": 0,
				"distinctBindings":    map[string]any{"1.count": []any{d4, d6}, "2.count": []any{d4, d6}},
				"promotionApprovalId": promotionIDFor(digest), "promotionProposedAt": at, "lastReplayAt": at,
				"lastBinding": map[string]any{"1.count": 6, "2.count": 6},
			}, "rungChangedAt")
		}
	}
	// five moves in one instant of the clock: each version is stamped after the one before
	for i := 1; i < len(versions); i++ {
		if !timeOf(versions[i]).After(timeOf(versions[i-1])) {
			t.Errorf("the versions of five moves in one instant are %v, want each after the one before", versions)
			break
		}
	}

	// The owner's one decision: canary, the promotion closed, the evidence of canary clear.
	decided := builtinAnswer(t, p.decidePromotion, ctx, map[string]any{"skillId": "gs-1", "templateDigest": digest, "approved": true})
	if decided["rung"] != "canary" || decided["state"] != "canary" || decided["skillId"] != "gs-1" {
		t.Fatalf("approval answered %v", decided)
	}
	wrote(t, "the approval", last(), map[string]any{
		"rung": "canary", "state": "canary", "promotionApprovalId": "", "canaryMatches": 0, "failures": 0, "rungChangedAt": at,
	}, "promotionProposedAt")

	// Five clean runs: trusted on the fifth, with the binding of the run kept.
	binding := map[string]any{"1.count": 5, "2.count": 5}
	for i := 1; i <= 5; i++ {
		got := builtinAnswer(t, p.reportSkillOutcome, ctx, map[string]any{"skillId": "gs-1", "ok": true, "binding": binding})
		if i < 5 {
			wrote(t, fmt.Sprintf("clean run %d", i), last(), map[string]any{"rung": "canary", "canaryMatches": i, "lastBinding": binding, "lastReplayAt": at}, "rungChangedAt")
			if got["rung"] != "canary" {
				t.Fatalf("clean run %d answered %v", i, got)
			}
		}
	}
	wrote(t, "the fifth clean run", last(), map[string]any{"rung": "trusted", "state": "trusted", "canaryMatches": 0, "rungChangedAt": at, "lastBinding": binding})

	// A run that failed counts against it, without a binding; the second sends it back.
	builtinAnswer(t, p.reportSkillOutcome, ctx, map[string]any{"skillId": "gs-1", "ok": false, "binding": binding})
	wrote(t, "the first failure", last(), map[string]any{"rung": "trusted", "failures": 1, "canaryMatches": 0}, "rungChangedAt", "lastBinding")
	demoted := builtinAnswer(t, p.reportSkillOutcome, ctx, map[string]any{"skillId": "gs-1", "ok": false})
	if demoted["rung"] != "shadow" || demoted["state"] != "shadow" {
		t.Fatalf("the second failure answered %v", demoted)
	}
	wrote(t, "the demotion", last(), map[string]any{
		"rung": "shadow", "state": "shadow", "shadowMatches": 0, "canaryMatches": 0, "failures": 0,
		"distinctBindings": map[string]any{}, "promotionApprovalId": "", "rungChangedAt": at,
	})
	if m, ok := last()["distinctBindings"].(map[string]any); !ok || m == nil {
		t.Errorf("the cleared streak was written as %#v, want an explicit empty object (an omitted field is left alone)", last()["distinctBindings"])
	}
}

// A comparison that differs resets the streak and keeps an open proposal: the proposal
// pins a version a person can still decide about, and what it showed was true when it
// was raised. A decline spends the streak that earned the proposal.
func TestAMismatchResetsTheStreakAndADeclineSpendsIt(t *testing.T) {
	row := skillFull(ladderOwner, "gs-1", nil)
	st := newLadderStore(row)
	st.listed = []map[string]any{row}
	p := st.provider()
	ctx := asOwner(ladderOwner)
	sig, digest := trimmed(row["goalSignature"]), digestOf(row)
	for _, count := range []float64{4, 4, 4, 4, 6} {
		builtinAnswer(t, p.shadowCompare, ctx, map[string]any{"goalSignature": sig, "plan": planArg(torchPlan(count))})
	}
	last := func() map[string]any { return st.writes[len(st.writes)-1] }

	// The same actions with another constant (the block collected): not an instance.
	other := torchPlan(4)
	other[0].Args["block"] = "iron"
	got := builtinAnswer(t, p.shadowCompare, ctx, map[string]any{"goalSignature": sig, "plan": planArg(other)})
	if asInt(got["compared"], -1) != 1 || asInt(got["matched"], -1) != 0 {
		t.Fatalf("a plan with another constant answered %v, want compared and not matched", got)
	}
	wrote(t, "the mismatch", last(), map[string]any{
		"rung": "shadow", "state": "awaiting_approval", "shadowMatches": 0, "distinctBindings": map[string]any{},
		"promotionApprovalId": promotionIDFor(digest),
	}, "lastBinding", "promotionProposedAt", "rungChangedAt")
	if got := st.skills["gs-1"]["lastBinding"]; !reflect.DeepEqual(got, map[string]any{"1.count": float64(6), "2.count": float64(6)}) {
		t.Errorf("the last binding became %v, want the last plan that matched", got)
	}

	declined := builtinAnswer(t, p.decidePromotion, ctx, map[string]any{"skillId": "gs-1", "templateDigest": digest, "approved": false})
	if declined["rung"] != "shadow" || declined["state"] != "shadow" {
		t.Fatalf("a decline answered %v, want the skill to stay in shadow with no proposal open", declined)
	}
	wrote(t, "the decline", last(), map[string]any{"rung": "shadow", "state": "shadow", "shadowMatches": 0, "promotionApprovalId": "", "distinctBindings": map[string]any{}},
		"rungChangedAt", "promotionProposedAt")

	// ...and it must earn a new proposal: four matches are not enough.
	for _, count := range []float64{4, 6, 4, 6} {
		got := builtinAnswer(t, p.shadowCompare, ctx, map[string]any{"goalSignature": sig, "plan": planArg(torchPlan(count))})
		if proposed, _ := got["proposed"].([]any); len(proposed) != 0 {
			t.Fatalf("a declined skill proposed again after fewer than five matches: %v", got)
		}
	}
	got = builtinAnswer(t, p.shadowCompare, ctx, map[string]any{"goalSignature": sig, "plan": planArg(torchPlan(4))})
	if proposed, _ := got["proposed"].([]any); len(proposed) != 1 {
		t.Fatalf("five new matches answered %v, want a new proposal", got)
	}
}

// ---- which skills a comparison moves ----

// A plan is compared with the SHADOW skills of its goal that are the CALLER'S and take
// the same actions in the same order, and with no other: a canary skill is not
// compared, another owner's is not even locked, and a skill of other actions is not a
// candidate. A skill the list had and the store has lost since (a forget deleted it)
// is passed over and the others are compared.
func TestAComparisonMovesOnlyTheCallersShadowSkillsOfTheSameActions(t *testing.T) {
	mine := skillFull(ladderOwner, "gs-mine", nil)
	canary := skillFull(ladderOwner, "gs-canary", inRung("canary", nil))
	retired := skillFull(ladderOwner, "gs-retired", inRung("retired", nil))
	others := skillFull(ladderOwner, "gs-other-actions", func(r map[string]any) { r["actions"] = []any{"look", "wave"} })
	foreign := skillFull("user-games-other", "gs-foreign", nil)
	gone := skillFull(ladderOwner, "gs-gone", nil)
	st := newLadderStore(mine, canary, retired, others, foreign, gone)
	st.listed = []map[string]any{foreign, canary, others, gone, retired, mine}
	st.vanish["gs-gone"] = true
	p := st.provider()

	got := builtinAnswer(t, p.shadowCompare, clusterOwnerOf(ladderOwner), map[string]any{"goalSignature": trimmed(mine["goalSignature"]), "plan": planArg(torchPlan(4))})
	if asInt(got["compared"], -1) != 1 || asInt(got["matched"], -1) != 1 {
		t.Fatalf("the comparison answered %v, want exactly the caller's own shadow skill of these actions compared", got)
	}
	if len(st.writes) != 1 || st.writes[0]["skillId"] != "gs-mine" {
		t.Fatalf("the comparison wrote %v, want one write, to gs-mine", st.writes)
	}
	for _, id := range []string{"gs-foreign", "gs-canary", "gs-other-actions", "gs-retired"} {
		if n := st.locks["games-skill:"+id]; n != 0 {
			t.Errorf("the comparison took the gate of %s (%d times), though it is not a skill it moves", id, n)
		}
	}
	if n := st.locks["games-skill:gs-gone"]; n != 1 {
		t.Errorf("the comparison took the gate of the skill that was deleted %d times, want it tried once", n)
	}
	if got := st.skills["gs-foreign"]["ownerUserId"]; got != ownerKey("user-games-other") {
		t.Errorf("another owner's skill now belongs to %v", got)
	}
}

// A skill that is no longer in shadow when its gate is taken (a decision, a demotion or
// a retirement landed since the list was read) is skipped and not counted: the list is
// the caller's look and may be stale; the row read inside the gate decides.
func TestAComparisonSkipsASkillThatLeftShadowSinceTheList(t *testing.T) {
	listedAsShadow := skillFull(ladderOwner, "gs-1", nil)
	moved := skillFull(ladderOwner, "gs-1", inRung("canary", nil))
	st := newLadderStore(moved)
	st.listed = []map[string]any{listedAsShadow}
	got := builtinAnswer(t, st.provider().shadowCompare, asOwner(ladderOwner), map[string]any{"goalSignature": trimmed(moved["goalSignature"]), "plan": planArg(torchPlan(4))})
	if asInt(got["compared"], -1) != 0 || len(st.writes) != 0 {
		t.Fatalf("the comparison answered %v and wrote %d, want the skill skipped", got, len(st.writes))
	}
}

// ---- what each builtin refuses before it touches anything ----

func TestTheLadderBuiltinsRefuseWhatTheyCannotUseBeforeTouchingAnything(t *testing.T) {
	sig := GoalSignature("Light up the area!", []string{"spot"})
	good := map[string]map[string]any{
		"shadowCompare":      {"goalSignature": sig, "plan": planArg(torchPlan(4))},
		"reportSkillOutcome": {"skillId": "gs-1", "ok": true},
		"decidePromotion":    {"skillId": "gs-1", "templateDigest": strings.Repeat("a", 64), "approved": true},
	}
	with := func(base string, k string, v any) map[string]any {
		out := map[string]any{}
		for key, val := range good[base] {
			out[key] = val
		}
		if v == nil {
			delete(out, k)
		} else {
			out[k] = v
		}
		return out
	}
	calls := func(p *Provider) map[string]builtinFn {
		return map[string]builtinFn{
			"shadowCompare": p.shadowCompare, "reportSkillOutcome": p.reportSkillOutcome,
			"decidePromotion": p.decidePromotion, "sweepSkills": p.sweepSkills,
		}
	}
	cases := []struct {
		call string
		name string
		args map[string]any
		ctx  context.Context
		want string
	}{
		{"shadowCompare", "no actor", good["shadowCompare"], context.Background(), "games_unauthenticated: "},
		{"reportSkillOutcome", "no actor", good["reportSkillOutcome"], context.Background(), "games_unauthenticated: "},
		{"decidePromotion", "no actor", good["decidePromotion"], context.Background(), "games_unauthenticated: "},
		{"sweepSkills", "no actor", nil, context.Background(), "games_unauthenticated: "},

		{"shadowCompare", "no goalSignature", with("shadowCompare", "goalSignature", nil), nil, "games_invalid_argument: goalSignature is required"},
		{"shadowCompare", "a goalSignature that is a number", with("shadowCompare", "goalSignature", 42), nil, "games_invalid_argument: goalSignature must be a string"},
		{"shadowCompare", "a goalSignature in capitals", with("shadowCompare", "goalSignature", strings.ToUpper(sig)), nil, "games_invalid_argument: goalSignature must be 64 lower-case hexadecimal digits"},
		{"shadowCompare", "a goalSignature that is short", with("shadowCompare", "goalSignature", sig[:63]), nil, "games_invalid_argument: goalSignature must be 64 lower-case hexadecimal digits"},
		{"shadowCompare", "a goalSignature that is not hexadecimal", with("shadowCompare", "goalSignature", strings.Repeat("g", 64)), nil, "games_invalid_argument: goalSignature must be 64 lower-case hexadecimal digits"},
		{"shadowCompare", "no plan", with("shadowCompare", "plan", nil), nil, "games_invalid_argument: a plan must be a list"},
		{"shadowCompare", "an empty plan", with("shadowCompare", "plan", []any{}), nil, "games_invalid_argument: a plan has at least one step"},
		{"shadowCompare", "a step that is not an object", with("shadowCompare", "plan", []any{"collect"}), nil, "games_invalid_argument: plan step 0 is not an object"},
		{"shadowCompare", "an action with a comma", with("shadowCompare", "plan", []any{map[string]any{"action": "a,b"}}), nil, "games_invalid_argument: plan step 0 action"},

		{"reportSkillOutcome", "no skillId", with("reportSkillOutcome", "skillId", nil), nil, "games_invalid_argument: skillId and ok are required"},
		{"reportSkillOutcome", "a blank skillId", with("reportSkillOutcome", "skillId", "  "), nil, "games_invalid_argument: skillId and ok are required"},
		{"reportSkillOutcome", "no ok", with("reportSkillOutcome", "ok", nil), nil, "games_invalid_argument: skillId and ok are required"},
		{"reportSkillOutcome", "an ok that is a string", with("reportSkillOutcome", "ok", "true"), nil, "games_invalid_argument: skillId and ok are required"},
		{"reportSkillOutcome", "a skillId that is a number", with("reportSkillOutcome", "skillId", 42), nil, "games_invalid_argument: skillId must be a string"},
		{"reportSkillOutcome", "a skillId that is another concept's id", with("reportSkillOutcome", "skillId", canonical(conceptEpisode, "gs-1")), nil, "games_invalid_argument: skillId must be a bare id"},
		{"reportSkillOutcome", "a skillId with colons", with("reportSkillOutcome", "skillId", "gs:1:2"), nil, "games_invalid_argument: skillId must be a bare id"},
		{"reportSkillOutcome", "a skillId with a NUL character", with("reportSkillOutcome", "skillId", "gs-\x00"), nil, "games_invalid_argument: skillId must be a bare id"},
		{"reportSkillOutcome", "a skillId of 65 characters", with("reportSkillOutcome", "skillId", strings.Repeat("g", 65)), nil, "games_invalid_argument: skillId must be a bare id"},
		{"reportSkillOutcome", "a binding that is a string", with("reportSkillOutcome", "binding", "4"), nil, "games_invalid_argument: binding must be an object"},
		{"reportSkillOutcome", "a binding that is a list", with("reportSkillOutcome", "binding", []any{4}), nil, "games_invalid_argument: binding must be an object"},

		{"decidePromotion", "no skillId", with("decidePromotion", "skillId", nil), nil, "games_invalid_argument: skillId, templateDigest and approved are required"},
		{"decidePromotion", "no templateDigest", with("decidePromotion", "templateDigest", nil), nil, "games_invalid_argument: skillId, templateDigest and approved are required"},
		{"decidePromotion", "a blank templateDigest", with("decidePromotion", "templateDigest", " "), nil, "games_invalid_argument: skillId, templateDigest and approved are required"},
		{"decidePromotion", "no approved", with("decidePromotion", "approved", nil), nil, "games_invalid_argument: skillId, templateDigest and approved are required"},
		{"decidePromotion", "an approved that is a string", with("decidePromotion", "approved", "yes"), nil, "games_invalid_argument: skillId, templateDigest and approved are required"},
		{"decidePromotion", "a templateDigest that is a number", with("decidePromotion", "templateDigest", 42), nil, "games_invalid_argument: templateDigest must be a string"},
		{"decidePromotion", "a skillId that is a list", with("decidePromotion", "skillId", []any{"gs-1"}), nil, "games_invalid_argument: skillId must be a string"},
	}
	for _, tc := range cases {
		t.Run(tc.call+"/"+tc.name, func(t *testing.T) {
			st := newLadderStore(skillFull(ladderOwner, "gs-1", inRung("canary", nil)))
			ctx := tc.ctx
			if ctx == nil {
				ctx = asOwner(ladderOwner)
			}
			if _, err := calls(st.provider())[tc.call](ctx, tc.args, 0); err == nil || !strings.HasPrefix(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one that begins %q", err, tc.want)
			}
			if !st.untouched() {
				t.Fatalf("a refused call touched the store: %v", st.events)
			}
		})
	}
}

// ---- the three refusals a skill can earn, and what they leave alone ----

func TestAnOutcomeIsReportedOnlyOfASkillThatRunsForReal(t *testing.T) {
	for _, tc := range []struct {
		rung    string
		serving bool
	}{{"shadow", false}, {"canary", true}, {"trusted", true}, {"retired", false}} {
		t.Run(tc.rung, func(t *testing.T) {
			st := newLadderStore(skillFull(ladderOwner, "gs-1", inRung(tc.rung, nil)))
			_, err := st.provider().reportSkillOutcome(asOwner(ladderOwner), map[string]any{"skillId": "gs-1", "ok": true}, 0)
			if tc.serving {
				if err != nil || len(st.writes) != 1 {
					t.Fatalf("a run of a %s skill = %v (wrote %d), want it counted", tc.rung, err, len(st.writes))
				}
				return
			}
			if err == nil || !strings.HasPrefix(err.Error(), "games_skill_not_serving: ") || !strings.Contains(err.Error(), tc.rung) || len(st.writes) != 0 {
				t.Fatalf("a run of a %s skill = %v (wrote %d), want games_skill_not_serving and nothing written", tc.rung, err, len(st.writes))
			}
			if last := st.events[len(st.events)-1]; last != "release games-skill:gs-1" {
				t.Fatalf("events = %v, want the gate let go", st.events)
			}
		})
	}
}

// A clean run's binding is what a runtime that runs the skill without a model
// instantiates the template with, so it must be a binding the template takes: every
// hole filled, no other name, no null. A run that failed keeps no binding, and one that
// is refused counts for nothing.
func TestARunsBindingIsKeptOnlyWhenTheTemplateTakesIt(t *testing.T) {
	good := map[string]any{"1.count": 5, "2.count": 5}
	for _, tc := range []struct {
		name    string
		ok      bool
		binding map[string]any
		refused string
		kept    bool
	}{
		{"a clean run with a binding", true, good, "", true},
		{"a clean run with none", true, nil, "", false},
		{"a failed run with one", false, good, "", false},
		{"a failed run with a binding that does not fit", false, map[string]any{"x": 1}, "", false},
		{"a hole left out", true, map[string]any{"1.count": 5}, "does not fill hole 2.count", false},
		{"a name that is no hole", true, map[string]any{"1.count": 5, "2.count": 5, "3.count": 5}, "no hole of the template", false},
		{"a null at a hole", true, map[string]any{"1.count": 5, "2.count": nil}, "no value (null)", false},
		{"an empty binding", true, map[string]any{}, "does not fill hole 1.count", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newLadderStore(skillFull(ladderOwner, "gs-1", inRung("canary", nil)))
			args := map[string]any{"skillId": "gs-1", "ok": tc.ok}
			if tc.binding != nil {
				args["binding"] = tc.binding
			}
			_, err := st.provider().reportSkillOutcome(asOwner(ladderOwner), args, 0)
			if tc.refused != "" {
				if err == nil || !strings.HasPrefix(err.Error(), "games_invalid_argument: the binding is not one the skill's template takes") || !strings.Contains(err.Error(), tc.refused) || len(st.writes) != 0 {
					t.Fatalf("error = %v (wrote %d), want a refusal naming %q and nothing counted", err, len(st.writes), tc.refused)
				}
				return
			}
			if err != nil || len(st.writes) != 1 {
				t.Fatalf("error = %v (wrote %d), want the run counted", err, len(st.writes))
			}
			if _, has := st.writes[0]["lastBinding"]; has != tc.kept {
				t.Fatalf("lastBinding written = %v, want %v", has, tc.kept)
			}
		})
	}
}

func TestAPromotionDecisionIsAboutTheVersionTheOwnerWasShown(t *testing.T) {
	open := func(extra map[string]any) map[string]any {
		row := skillFull(ladderOwner, "gs-1", nil)
		row["state"], row["promotionApprovalId"] = "awaiting_approval", promotionIDFor(digestOf(row))
		for k, v := range extra {
			row[k] = v
		}
		return row
	}
	digest := digestOf(open(nil))
	for _, tc := range []struct {
		name   string
		row    map[string]any
		digest string
		want   string // "" accepted
	}{
		{"the promotion that is open", open(nil), digest, ""},
		{"the digest of another version", open(nil), strings.Repeat("0", 64), "games_skill_version_changed: "},
		{"a digest with another version's promotion open", open(map[string]any{"promotionApprovalId": "promotion-0000000000000000"}), digest, "games_skill_version_changed: "},
		{"no promotion open", skillFull(ladderOwner, "gs-1", nil), digest, "games_no_promotion_open: "},
		{"a skill in canary", skillFull(ladderOwner, "gs-1", inRung("canary", nil)), digest, "games_no_promotion_open: "},
		{"a retired skill", skillFull(ladderOwner, "gs-1", inRung("retired", nil)), digest, "games_no_promotion_open: "},
		// the ladder closes a promotion on every move out of shadow, so a skill on another rung
		// with one open is a row that was written some other way, and it is not decided
		{"a canary skill that still names a promotion", skillFull(ladderOwner, "gs-1", inRung("canary", map[string]any{"promotionApprovalId": promotionIDFor(digest)})), digest, "games_no_promotion_open: "},
		// the version is checked first: a decision about another version of a skill that
		// has no promotion open is about another version
		{"another version, and none open", skillFull(ladderOwner, "gs-1", nil), strings.Repeat("0", 64), "games_skill_version_changed: "},
		{"a digest that is not a digest at all", open(nil), strings.Repeat("0", 5000), "games_skill_version_changed: "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newLadderStore(tc.row)
			_, err := st.provider().decidePromotion(asOwner(ladderOwner), map[string]any{"skillId": "gs-1", "templateDigest": tc.digest, "approved": true}, 0)
			if tc.want == "" {
				if err != nil || len(st.writes) != 1 {
					t.Fatalf("error = %v (wrote %d), want the decision made", err, len(st.writes))
				}
				return
			}
			if err == nil || !strings.HasPrefix(err.Error(), tc.want) || len(st.writes) != 0 {
				t.Fatalf("error = %v (wrote %d), want %q and nothing written", err, len(st.writes), tc.want)
			}
			if len(err.Error()) > 400 {
				t.Fatalf("the refusal is %d characters: it echoes what the caller sent, unbounded", len(err.Error()))
			}
		})
	}
}

// ---- the sweep ----

// The sweep is the ladder's maintenance: it reads every page of the skills not yet
// retired, moves only those that are due, and writes only for them. Retirement is by the
// last USE of a skill -- lastReplayAt, or its birth for one that was never used -- so a
// skill learned long ago and used yesterday is not retired, and one learned yesterday
// and never used is not either. Demotion is re-evaluated under the policy as it is now.
func TestASweepMovesOnlyTheSkillsThatAreDue(t *testing.T) {
	day := 24 * time.Hour
	old := rfc3339(ladderNow.Add(-40 * day))
	used := rfc3339(ladderNow.Add(-2 * day))
	skills := map[string]map[string]any{
		"gs-idle":    skillFull(ladderOwner, "gs-idle", func(r map[string]any) { r["learnedAt"] = old }),
		"gs-used":    skillFull(ladderOwner, "gs-used", func(r map[string]any) { r["learnedAt"], r["lastReplayAt"] = old, used }),
		"gs-new":     skillFull(ladderOwner, "gs-new", nil),
		"gs-failing": skillFull(ladderOwner, "gs-failing", inRung("trusted", map[string]any{"failures": float64(2), "learnedAt": used})),
		"gs-awaiting": skillFull(ladderOwner, "gs-awaiting", func(r map[string]any) {
			r["learnedAt"], r["promotionApprovalId"], r["state"] = old, "promotion-abc", "awaiting_approval"
		}),
		"gs-retired":   skillFull(ladderOwner, "gs-retired", inRung("retired", map[string]any{"learnedAt": old})),
		"gs-foreign":   skillFull("user-games-other", "gs-foreign", func(r map[string]any) { r["learnedAt"] = old }),
		"gs-vanishing": skillFull(ladderOwner, "gs-vanishing", func(r map[string]any) { r["learnedAt"] = old }),
	}
	var rows []map[string]any
	for _, id := range []string{"gs-idle", "gs-used", "gs-new", "gs-failing", "gs-awaiting", "gs-retired", "gs-foreign", "gs-vanishing"} {
		rows = append(rows, skills[id])
	}
	st := newLadderStore(rows...)
	st.vanish["gs-vanishing"] = true
	// Two pages, as the engine hands them over: the sweep has to read both.
	st.pages = [][]map[string]any{rows[:3], rows[3:]}
	got := builtinAnswer(t, st.provider().sweepSkills, clusterOwnerOf(ladderOwner), nil)

	// idle and awaiting retire (an open proposal is closed by the retirement); failing is
	// demoted (two failures under the policy of two); used and new are left alone, and so
	// is the one that had retired since the pages were read (examined, then skipped under its
	// gate); the foreign skill is not looked at and the vanished one is passed over.
	if asInt(got["examined"], -1) != 6 || asInt(got["retired"], -1) != 2 || asInt(got["demoted"], -1) != 1 {
		t.Fatalf("the sweep answered %v, want 6 examined (the caller's, not vanished), 2 retired and 1 demoted", got)
	}
	var wroteTo []string
	for _, w := range st.writes {
		wroteTo = append(wroteTo, trimmed(w["skillId"]))
	}
	if !sameStrings(wroteTo, []string{"gs-idle", "gs-failing", "gs-awaiting"}) {
		t.Fatalf("the sweep wrote to %v, want the three that changed, in the order the pages gave them", wroteTo)
	}
	if st.locks["games-skill:gs-foreign"] != 0 {
		t.Errorf("the sweep took the gate of another owner's skill")
	}
	if st.skills["gs-foreign"]["rung"] != "shadow" || st.skills["gs-foreign"]["ownerUserId"] != ownerKey("user-games-other") {
		t.Errorf("another owner's skill was moved: %v", st.skills["gs-foreign"])
	}
	wrote(t, "the retirement of an idle skill", st.writes[0], map[string]any{"rung": "retired", "state": "retired", "rungChangedAt": rfc3339(ladderNow), "promotionApprovalId": ""}, "lastBinding", "lastReplayAt")
	wrote(t, "the demotion of a failing one", st.writes[1], map[string]any{"rung": "shadow", "state": "shadow", "failures": 0, "canaryMatches": 0, "distinctBindings": map[string]any{}, "rungChangedAt": rfc3339(ladderNow)})
	wrote(t, "the retirement of one with a proposal open", st.writes[2], map[string]any{"rung": "retired", "state": "retired", "promotionApprovalId": ""})
}

// A sweep under a policy of ten days retires what the default of thirty leaves alone:
// the values are the row's, read at the time.
func TestASweepRetiresByTheWindowOfTheCurrentPolicy(t *testing.T) {
	week := skillFull(ladderOwner, "gs-1", func(r map[string]any) { r["learnedAt"] = rfc3339(ladderNow.Add(-7 * 24 * time.Hour)) })
	for _, tc := range []struct {
		days    float64
		retired int
	}{{0, 0}, {30, 0}, {10, 0}, {6, 1}} {
		st := newLadderStore(cloneRow(week))
		st.pages = [][]map[string]any{{week}}
		st.policy = []map[string]any{{"retireAfterDays": tc.days}}
		got := builtinAnswer(t, st.provider().sweepSkills, asOwner(ladderOwner), nil)
		if asInt(got["retired"], -1) != tc.retired {
			t.Errorf("a skill idle for 7 days under a window of %v days: %v, want %d retired", tc.days, got, tc.retired)
		}
	}
}

// A sweep that cannot read its list moves nothing; one that finds a skill it cannot move
// for a reason of its own says so, having moved those before it; and a caller that has
// gone away stops it between skills.
func TestASweepStopsOnWhatItCannotDoAndReportsIt(t *testing.T) {
	old := func(id string) map[string]any {
		return skillFull(ladderOwner, id, func(r map[string]any) { r["learnedAt"] = rfc3339(ladderNow.Add(-40 * 24 * time.Hour)) })
	}
	t.Run("a list that cannot be read", func(t *testing.T) {
		st := newLadderStore(old("gs-1"))
		st.walkErr = errors.New("the pages ran out of cursor")
		if _, err := st.provider().sweepSkills(asOwner(ladderOwner), nil, 0); err == nil || !strings.Contains(err.Error(), "the pages ran out of cursor") || len(st.writes) != 0 || len(st.locks) != 0 {
			t.Fatalf("error = %v (wrote %d, took %d gates), want the failure and nothing moved", err, len(st.writes), len(st.locks))
		}
	})
	t.Run("a write that fails", func(t *testing.T) {
		st := newLadderStore(old("gs-1"), old("gs-2"))
		st.pages = [][]map[string]any{{st.skills["gs-1"], st.skills["gs-2"]}}
		st.writeE = errors.New("the mutation was refused")
		if _, err := st.provider().sweepSkills(asOwner(ladderOwner), nil, 0); err == nil || !strings.Contains(err.Error(), "the mutation was refused") || len(st.writes) != 1 {
			t.Fatalf("error = %v (wrote %d), want the failure after the first write", err, len(st.writes))
		}
	})
	t.Run("a caller that went away", func(t *testing.T) {
		st := newLadderStore(old("gs-1"), old("gs-2"))
		st.pages = [][]map[string]any{{st.skills["gs-1"], st.skills["gs-2"]}}
		ctx, cancel := context.WithCancel(asOwner(ladderOwner))
		cancel()
		got := builtinAnswer(t, st.provider().sweepSkills, ctx, nil)
		if asInt(got["examined"], -1) != 0 || len(st.writes) != 0 {
			t.Fatalf("a sweep for a caller that was gone answered %v and wrote %d", got, len(st.writes))
		}
	})
}

// ---- pure helpers ----

func TestBindingsArgIsNeverNil(t *testing.T) {
	if got := bindingsArg(nil); got == nil || len(got) != 0 {
		t.Fatalf("bindingsArg(nil) = %#v, want an empty object", got)
	}
	got := bindingsArg(map[string][]string{"1.count": {"sha256:a", "sha256:b"}})
	if !reflect.DeepEqual(got, map[string]any{"1.count": []any{"sha256:a", "sha256:b"}}) {
		t.Fatalf("bindingsArg = %#v", got)
	}
}

func TestLadderStateReadsWhatTheEngineStoresAndNothingElse(t *testing.T) {
	// A rung nobody wrote is on no rung (the ladder moves nothing there), and what is not a
	// list of digests counts no digest.
	s := ladderStateOf(map[string]any{
		"rung": "promoted", "distinctBindings": map[string]any{"1.count": "sha256:a", "2.count": []any{"sha256:b", 7, ""}},
	})
	if s.Rung != work.RungNone || len(s.DistinctBindings["1.count"]) != 0 || !sameStrings(s.DistinctBindings["2.count"], []string{"sha256:b", "7"}) {
		t.Fatalf("state = %+v", s)
	}
	if !s.LastReplayAt.IsZero() || s.ShadowMatches != 0 || s.PromotionApprovalId != "" {
		t.Fatalf("absent fields are not zero: %+v", s)
	}
	if got := ladderStateOf(map[string]any{"rung": " canary ", "promotionApprovalId": " promotion-x "}); got.Rung != work.RungCanary || got.PromotionApprovalId != "promotion-x" {
		t.Fatalf("state = %+v, want the values trimmed", got)
	}
}

// The pages are read while other things move skills, and a skill moved in the meantime
// has a newer version further on: it turns up twice, and is examined once.
func TestASweepExaminesASkillThePagesReturnTwiceOnce(t *testing.T) {
	old := skillFull(ladderOwner, "gs-1", func(r map[string]any) { r["learnedAt"] = rfc3339(ladderNow.Add(-40 * 24 * time.Hour)) })
	st := newLadderStore(old)
	st.pages = [][]map[string]any{{old}, {old}, {old}}
	got := builtinAnswer(t, st.provider().sweepSkills, asOwner(ladderOwner), nil)
	if asInt(got["examined"], -1) != 1 || asInt(got["retired"], -1) != 1 || len(st.writes) != 1 || st.locks["games-skill:gs-1"] != 1 {
		t.Fatalf("the sweep answered %v (wrote %d, took the gate %d times), want the one skill examined and retired once", got, len(st.writes), st.locks["games-skill:gs-1"])
	}
	if want := []string{"page 1", "page 2", "page 3"}; !sameStrings(st.events[:3], want) {
		t.Fatalf("events = %v, want every page read before the first move", st.events)
	}
}
