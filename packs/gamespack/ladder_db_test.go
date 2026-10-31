package gamespack

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

func planArg(plan []PlanStep) []any {
	out := make([]any, len(plan))
	for i, s := range plan {
		out[i] = map[string]any{"action": s.Action, "args": s.Args}
	}
	return out
}

// earnPromotion makes five matching comparisons across two values of every
// hole, which proposes the skill for the owner's approval.
func earnPromotion(t *testing.T, lg *liveGames, ctx context.Context, sig string) {
	t.Helper()
	for i, count := range []float64{4, 4, 4, 4, 6} {
		res := lg.must(t, ctx, "builtin", "gamesShadowCompare", map[string]any{"goalSignature": sig, "plan": planArg(torchPlan(count))})
		if asInt(res[0]["matched"], 0) != 1 {
			t.Fatalf("comparison %d did not match: %v", i+1, res[0])
		}
	}
}

// climbToCanary learns the torch skill, earns its promotion and approves it.
func climbToCanary(t *testing.T, lg *liveGames, ctx context.Context, prefix string) (skillID, sig string) {
	t.Helper()
	sig = GoalSignature("Light up the area!", []string{"spot"})
	skillID = learnTorchSkill(t, lg, ctx, prefix)
	earnPromotion(t, lg, ctx, sig)
	skill := lg.must(t, ctx, "query", "gamesSkillById", map[string]any{"skillId": skillID})[0]
	decided := lg.must(t, ctx, "builtin", "gamesDecideSkillPromotion", map[string]any{"skillId": skillID, "templateDigest": skill["templateDigest"], "approved": true})
	if decided[0]["rung"] != "canary" {
		t.Fatalf("approval must make it canary: %v", decided[0])
	}
	return skillID, sig
}

// The whole ladder, through the real engine: a shadow skill runs nothing and
// has nothing to decide, earns a promotion, takes one approval, becomes
// trusted after five clean canary runs, and two failures send it back.
func TestLiveSkillClimbsTheLadder(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	ctx := asOwner("user-games-ladder-" + n)
	sig := GoalSignature("Light up the area!", []string{"spot"})
	skillID := learnTorchSkill(t, lg, ctx, "t-"+n)
	skill := lg.must(t, ctx, "query", "gamesSkillById", map[string]any{"skillId": skillID})[0]

	if _, err := lg.call(t, ctx, "builtin", "gamesReportSkillOutcome", map[string]any{"skillId": skillID, "ok": true}); err == nil || !strings.Contains(err.Error(), "games_skill_not_serving") {
		t.Fatalf("a shadow skill runs for nobody; reporting a run must be refused, got %v", err)
	}
	if _, err := lg.call(t, ctx, "builtin", "gamesDecideSkillPromotion", map[string]any{"skillId": skillID, "templateDigest": skill["templateDigest"], "approved": true}); err == nil || !strings.Contains(err.Error(), "games_no_promotion_open") {
		t.Fatalf("a skill that earned nothing has nothing to decide, got %v", err)
	}

	earnPromotion(t, lg, ctx, sig)
	skill = lg.must(t, ctx, "query", "gamesSkillById", map[string]any{"skillId": skillID})[0]
	if skill["state"] != "awaiting_approval" {
		t.Fatalf("five matches across two values of every hole must propose: %v", skill)
	}
	if rows := lg.must(t, ctx, "query", "gamesSkillsAwaitingApproval", nil); len(rows) != 1 {
		t.Fatalf("the awaiting list has %d skills, want 1", len(rows))
	}

	if _, err := lg.call(t, ctx, "builtin", "gamesDecideSkillPromotion", map[string]any{"skillId": skillID, "templateDigest": "0000", "approved": true}); err == nil || !strings.Contains(err.Error(), "games_skill_version_changed") {
		t.Fatalf("a decision about another version must be refused, got %v", err)
	}
	decided := lg.must(t, ctx, "builtin", "gamesDecideSkillPromotion", map[string]any{"skillId": skillID, "templateDigest": skill["templateDigest"], "approved": true})
	if decided[0]["rung"] != "canary" {
		t.Fatalf("approval must make it canary: %v", decided[0])
	}
	if open := lg.must(t, ctx, "query", "gamesSkillById", map[string]any{"skillId": skillID})[0]["promotionApprovalId"]; open != "" && open != nil {
		t.Fatalf("a decided promotion must be closed on the row, got promotionApprovalId %v", open)
	}

	for i := 0; i < 5; i++ {
		lg.must(t, ctx, "builtin", "gamesReportSkillOutcome", map[string]any{"skillId": skillID, "ok": true, "binding": map[string]any{"1.count": 5, "2.count": 5}})
	}
	skill = lg.must(t, ctx, "query", "gamesSkillById", map[string]any{"skillId": skillID})[0]
	if skill["rung"] != "trusted" {
		t.Fatalf("five clean canary runs must make it trusted: %v", skill)
	}

	for i := 0; i < 2; i++ {
		lg.must(t, ctx, "builtin", "gamesReportSkillOutcome", map[string]any{"skillId": skillID, "ok": false})
	}
	skill = lg.must(t, ctx, "query", "gamesSkillById", map[string]any{"skillId": skillID})[0]
	if skill["rung"] != "shadow" || skill["state"] != "shadow" {
		t.Fatalf("two failures must demote to shadow: %v", skill)
	}
}

// Two characters report runs of one skill at once: both count.
func TestLiveConcurrentOutcomesBothCount(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	ctx := asOwner("user-games-race-" + n)
	skillID, _ := climbToCanary(t, lg, ctx, "r-"+n)

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := lg.call(t, ctx, "builtin", "gamesReportSkillOutcome", map[string]any{"skillId": skillID, "ok": true}); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	skill := lg.must(t, ctx, "query", "gamesSkillById", map[string]any{"skillId": skillID})[0]
	if asInt(skill["canaryMatches"], 0) != 2 {
		t.Fatalf("two concurrent clean runs counted %v times, want 2", skill["canaryMatches"])
	}
}

// A skill unused past the retirement window retires, and the goal coming back
// teaches a NEW skill: a retired one is terminal.
func TestLiveRetiredSkillIsRelearnedAsANewGeneration(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	owner := "user-games-retire-" + n
	ctx := asOwner(owner)
	sig := GoalSignature("Light up the area!", []string{"spot"})
	actions := []string{"collect", "craft", "placeTorches"}
	if first := learnTorchSkill(t, lg, ctx, "g1-"+n); first != SkillID(owner, sig, actions, 1) {
		t.Fatalf("first generation id = %v", first)
	}
	lg.provider.now = func() time.Time { return time.Now().Add(31 * 24 * time.Hour) }
	swept := lg.must(t, ctx, "builtin", "gamesSweepSkills", nil)
	if asInt(swept[0]["retired"], 0) != 1 {
		t.Fatalf("a skill unused for 31 days must retire: %v", swept[0])
	}
	lg.provider.now = time.Now
	// The goal comes back. Its earlier traces are still within their 30 days,
	// so the first new success already makes two that agree.
	second := lg.must(t, ctx, "builtin", "gamesRecordTrace", trace("g2a-"+n, torchPlan(5), "success", "neutral"))[0]["learnedSkillId"]
	if second != SkillID(owner, sig, actions, 2) {
		t.Fatalf("the goal coming back after retirement must teach generation 2, got %v", second)
	}
	if rows := lg.must(t, ctx, "query", "gamesSkillById", map[string]any{"skillId": second}); len(rows) != 1 || rows[0]["state"] != "shadow" {
		t.Fatalf("generation 2 must start in shadow: %v", rows)
	}
}

// ---- the ladder, through the real engine, a rule at a time ----

// The proposal needs five matches AND two distinct values of every hole. Five matches
// of one value propose nothing, and the sixth match, on a second value, does; a plan of
// the same actions with another constant is not a match and starts the streak again,
// while an open proposal stays. What a match binds is kept as the skill's lastBinding.
func TestLiveAProposalNeedsTwoValuesOfEveryHoleAndAMismatchStartsAgain(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	ctx := asOwner("user-games-streak-" + n)
	sig := GoalSignature("Light up the area!", []string{"spot"})
	skillID := learnTorchSkill(t, lg, ctx, "streak-"+n)
	compare := func(plan []PlanStep) map[string]any {
		return lg.must(t, ctx, "builtin", "gamesShadowCompare", map[string]any{"goalSignature": sig, "plan": planArg(plan)})[0]
	}
	read := func() map[string]any {
		return lg.must(t, ctx, "query", "gamesSkillById", map[string]any{"skillId": skillID})[0]
	}

	for i := 1; i <= 5; i++ {
		if got := compare(torchPlan(4)); asInt(got["matched"], 0) != 1 || len(got["proposed"].([]any)) != 0 {
			t.Fatalf("match %d of one value answered %v", i, got)
		}
	}
	skill := read()
	if asInt(skill["shadowMatches"], 0) != 5 || skill["state"] != "shadow" || skill["promotionApprovalId"] != "" {
		t.Fatalf("five matches of one value must not propose: %v", skill)
	}
	if binding := asObject(skill["lastBinding"]); asInt(binding["1.count"], -1) != 4 || asInt(binding["2.count"], -1) != 4 {
		t.Fatalf("lastBinding = %v, want the hole values of the newest match", skill["lastBinding"])
	}

	// the sixth match, on a second value of each hole, proposes at once
	got := compare(torchPlan(6))
	if proposed := got["proposed"].([]any); len(proposed) != 1 || proposed[0] != skillID {
		t.Fatalf("the sixth match on a second value answered %v, want %s proposed", got, skillID)
	}
	skill = read()
	if skill["state"] != "awaiting_approval" || skill["promotionApprovalId"] != promotionIDFor(trimmed(skill["templateDigest"])) || skill["promotionProposedAt"] == nil {
		t.Fatalf("the proposal is not on the row: %v", skill)
	}
	proposedAt := skill["promotionProposedAt"]

	// another constant: not an instance of the template, so the streak starts again, and
	// the proposal stays, still pinned to the version it was raised for
	other := torchPlan(4)
	other[0].Args["block"] = "iron"
	got = compare(other)
	if asInt(got["compared"], -1) != 1 || asInt(got["matched"], -1) != 0 {
		t.Fatalf("a plan with another constant answered %v, want compared and not matched", got)
	}
	skill = read()
	if asInt(skill["shadowMatches"], -1) != 0 || len(asObject(skill["distinctBindings"])) != 0 || skill["state"] != "awaiting_approval" || skill["promotionProposedAt"] != proposedAt {
		t.Fatalf("a mismatch must clear the streak and keep the open proposal as it was: %v", skill)
	}
	if binding := asObject(skill["lastBinding"]); asInt(binding["1.count"], -1) != 6 {
		t.Fatalf("lastBinding = %v, want the newest MATCH's, which a mismatch does not replace", skill["lastBinding"])
	}

	// the wrong actions are not compared at all, and neither is a plan for another goal
	different := torchPlan(4)
	different[2].Action = "lookAt"
	if got := compare(different); asInt(got["compared"], -1) != 0 {
		t.Fatalf("a plan of other actions was compared: %v", got)
	}
	if got := lg.must(t, ctx, "builtin", "gamesShadowCompare", map[string]any{"goalSignature": GoalSignature("Dig a trench!", nil), "plan": planArg(torchPlan(4))})[0]; asInt(got["compared"], -1) != 0 {
		t.Fatalf("a plan for a goal nobody learned was compared: %v", got)
	}
}

// A declined promotion keeps the skill in shadow with nothing open and its streak spent: it
// must earn a new proposal, and an approval cannot then be made of the old one.
func TestLiveADeclinedPromotionMustBeEarnedAgain(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	ctx := asOwner("user-games-decline-" + n)
	sig := GoalSignature("Light up the area!", []string{"spot"})
	skillID := learnTorchSkill(t, lg, ctx, "decline-"+n)
	earnPromotion(t, lg, ctx, sig)
	digest := lg.must(t, ctx, "query", "gamesSkillById", map[string]any{"skillId": skillID})[0]["templateDigest"]

	declined := lg.must(t, ctx, "builtin", "gamesDecideSkillPromotion", map[string]any{"skillId": skillID, "templateDigest": digest, "approved": false})[0]
	if declined["rung"] != "shadow" || declined["state"] != "shadow" {
		t.Fatalf("a decline answered %v, want the skill to stay in shadow with nothing open", declined)
	}
	skill := lg.must(t, ctx, "query", "gamesSkillById", map[string]any{"skillId": skillID})[0]
	if skill["promotionApprovalId"] != "" || asInt(skill["shadowMatches"], -1) != 0 || len(asObject(skill["distinctBindings"])) != 0 {
		t.Fatalf("a decline must close the proposal and spend the streak: %v", skill)
	}
	if _, err := lg.call(t, ctx, "builtin", "gamesDecideSkillPromotion", map[string]any{"skillId": skillID, "templateDigest": digest, "approved": true}); err == nil || !strings.Contains(err.Error(), "games_no_promotion_open") {
		t.Fatalf("approving a promotion that was declined got %v, want games_no_promotion_open", err)
	}
	if rows := lg.must(t, ctx, "query", "gamesSkillsAwaitingApproval", nil); len(rows) != 0 {
		t.Fatalf("the awaiting list holds %d skills after a decline", len(rows))
	}

	earnPromotion(t, lg, ctx, sig)
	if st := lg.must(t, ctx, "query", "gamesSkillById", map[string]any{"skillId": skillID})[0]["state"]; st != "awaiting_approval" {
		t.Fatalf("five new matches left the skill %v, want a new proposal", st)
	}
}

// A retired skill is terminal (component/work's rule): nothing moves it, nothing counts
// for it, and the sweep leaves it alone. The goal coming back is a new skill at the next
// generation, which starts where every skill starts, and the old one stays retired.
func TestLiveARetiredSkillIsTerminalAndTheNewGenerationIsNotIt(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	owner := "user-games-terminal-" + n
	ctx := asOwner(owner)
	sig := GoalSignature("Light up the area!", []string{"spot"})
	actions := []string{"collect", "craft", "placeTorches"}
	first := learnTorchSkill(t, lg, ctx, "term-"+n)
	digest := lg.must(t, ctx, "query", "gamesSkillById", map[string]any{"skillId": first})[0]["templateDigest"]

	lg.provider.now = func() time.Time { return time.Now().Add(31 * 24 * time.Hour) }
	if swept := lg.must(t, ctx, "builtin", "gamesSweepSkills", nil)[0]; asInt(swept["retired"], -1) != 1 || asInt(swept["examined"], -1) != 1 {
		t.Fatalf("a skill unused for 31 days must retire: %v", swept)
	}
	lg.provider.now = time.Now
	retired := lg.must(t, ctx, "query", "gamesSkillById", map[string]any{"skillId": first})[0]
	if retired["rung"] != "retired" || retired["state"] != "retired" || retired["rungChangedAt"] == nil || !strings.Contains(trimmed(retired["lastReason"]), "retires") {
		t.Fatalf("the retirement is not on the row: %v", retired)
	}
	versions := lg.stored(t, conceptSkill, first)

	// nothing moves it
	if _, err := lg.call(t, ctx, "builtin", "gamesReportSkillOutcome", map[string]any{"skillId": first, "ok": true}); err == nil || !strings.Contains(err.Error(), "games_skill_not_serving") {
		t.Errorf("a run of a retired skill got %v, want games_skill_not_serving", err)
	}
	if _, err := lg.call(t, ctx, "builtin", "gamesDecideSkillPromotion", map[string]any{"skillId": first, "templateDigest": digest, "approved": true}); err == nil || !strings.Contains(err.Error(), "games_no_promotion_open") {
		t.Errorf("a decision about a retired skill got %v, want games_no_promotion_open", err)
	}
	if got := lg.must(t, ctx, "builtin", "gamesShadowCompare", map[string]any{"goalSignature": sig, "plan": planArg(torchPlan(4))})[0]; asInt(got["compared"], -1) != 0 {
		t.Errorf("a plan was compared with a retired skill: %v", got)
	}
	if swept := lg.must(t, ctx, "builtin", "gamesSweepSkills", nil)[0]; asInt(swept["examined"], -1) != 0 {
		t.Errorf("a sweep examined %v, want a retired skill left out of it", swept)
	}
	if got := lg.stored(t, conceptSkill, first); got != versions {
		t.Fatalf("the retired skill has %d versions, was %d: something wrote to it", got, versions)
	}

	// the goal comes back: generation 2 starts in shadow with nothing counted, and
	// generation 1 is exactly as it was
	second := lg.must(t, ctx, "builtin", "gamesRecordTrace", trace("term2-"+n, torchPlan(5), "success", "neutral"))[0]["learnedSkillId"]
	if second != SkillID(owner, sig, actions, 2) {
		t.Fatalf("the goal coming back must teach generation 2, got %v", second)
	}
	gen2 := lg.must(t, ctx, "query", "gamesSkillById", map[string]any{"skillId": second})[0]
	if gen2["rung"] != "shadow" || asInt(gen2["shadowMatches"], -1) != 0 || gen2["promotionApprovalId"] != "" {
		t.Fatalf("generation 2 must start where every skill starts: %v", gen2)
	}
	if got := lg.stored(t, conceptSkill, first); got != versions {
		t.Fatalf("learning generation 2 wrote to generation 1: %d versions, was %d", got, versions)
	}
	if again := lg.must(t, ctx, "query", "gamesSkillById", map[string]any{"skillId": first})[0]; again["rung"] != "retired" {
		t.Fatalf("generation 1 is %v, want it retired still", again["rung"])
	}
	// a comparison now reaches generation 2 and nothing else
	got := lg.must(t, ctx, "builtin", "gamesShadowCompare", map[string]any{"goalSignature": sig, "plan": planArg(torchPlan(4))})[0]
	if asInt(got["compared"], -1) != 1 || asInt(got["matched"], -1) != 1 {
		t.Fatalf("a comparison after relearning answered %v, want generation 2 compared", got)
	}
}

// A sweep writes the skills it moves and nothing else: the ladder's history is a record
// of moves and not of looks.
func TestLiveASweepWritesNothingForASkillItLeavesAlone(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	ctx := asOwner("user-games-looks-" + n)
	skillID, _ := climbToCanary(t, lg, ctx, "look-"+n)
	versions := lg.stored(t, conceptSkill, skillID)
	swept := lg.must(t, ctx, "builtin", "gamesSweepSkills", nil)[0]
	if asInt(swept["examined"], -1) != 1 || asInt(swept["demoted"], -1) != 0 || asInt(swept["retired"], -1) != 0 {
		t.Fatalf("a sweep of one fresh canary skill answered %v", swept)
	}
	if got := lg.stored(t, conceptSkill, skillID); got != versions {
		t.Fatalf("a sweep that found nothing to change wrote %d version(s)", got-versions)
	}
}

// A skill used lately is not retired because it was learned long ago: the retirement
// clock is the last USE (lastReplayAt), and a skill learned lately and never used is not
// retired either. Both are shown with the clock 31 days on: a canary skill that has just
// run, and a skill that has not.
func TestLiveRetirementIsByLastUseAndNotByAge(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	ctx := asOwner("user-games-use-" + n)
	used, _ := climbToCanary(t, lg, ctx, "use-"+n)
	idle, _ := teachGoal(t, lg, ctx, "Dig a trench!", "idle-"+n)

	// 29 days on, the used skill runs once more; at 31 days on it was used two days ago
	lg.provider.now = func() time.Time { return time.Now().Add(29 * 24 * time.Hour) }
	if _, err := lg.call(t, ctx, "builtin", "gamesReportSkillOutcome", map[string]any{"skillId": used, "ok": true}); err != nil {
		t.Fatal(err)
	}
	lg.provider.now = func() time.Time { return time.Now().Add(31 * 24 * time.Hour) }
	swept := lg.must(t, ctx, "builtin", "gamesSweepSkills", nil)[0]
	lg.provider.now = time.Now
	if asInt(swept["examined"], -1) != 2 || asInt(swept["retired"], -1) != 1 {
		t.Fatalf("the sweep answered %v, want both examined and the idle one retired", swept)
	}
	if got := lg.must(t, ctx, "query", "gamesSkillById", map[string]any{"skillId": idle})[0]["rung"]; got != "retired" {
		t.Errorf("the skill nobody used is %v, want retired", got)
	}
	if got := lg.must(t, ctx, "query", "gamesSkillById", map[string]any{"skillId": used})[0]["rung"]; got != "canary" {
		t.Errorf("the skill used two days ago is %v, want it left in canary", got)
	}
}

// The codes reach a caller through the real engine whole, and a refused call moves
// nothing.
func TestLiveTheLadderBuiltinsRefuseWithTheirCodesAndMoveNothing(t *testing.T) {
	lg := newLiveGames(t)
	n := runID()
	ctx := asOwner("user-games-codes-" + n)
	sig := GoalSignature("Light up the area!", []string{"spot"})
	skillID, _ := climbToCanary(t, lg, ctx, "codes-"+n)
	created, payload := lg.skillVersion(t, skillID)
	versions := lg.stored(t, conceptSkill, skillID)

	for _, tc := range []struct {
		call string
		args map[string]any
		want string
	}{
		{"gamesShadowCompare", map[string]any{"goalSignature": "nope", "plan": planArg(torchPlan(4))}, "games_invalid_argument: goalSignature must be 64 lower-case hexadecimal digits"},
		{"gamesShadowCompare", map[string]any{"goalSignature": sig, "plan": []any{}}, "games_invalid_argument: a plan has at least one step"},
		{"gamesShadowCompare", map[string]any{"goalSignature": sig, "plan": []any{map[string]any{"action": 3}}}, "games_invalid_argument: plan step 0 action must be a string"},
		{"gamesReportSkillOutcome", map[string]any{"skillId": skillID, "ok": "yes"}, "games_invalid_argument: skillId and ok are required"},
		{"gamesReportSkillOutcome", map[string]any{"skillId": skillID, "ok": true, "binding": []any{1}}, "games_invalid_argument: binding must be an object"},
		{"gamesReportSkillOutcome", map[string]any{"skillId": skillID, "ok": true, "binding": map[string]any{"1.count": 5}}, "games_invalid_argument: the binding is not one the skill's template takes"},
		{"gamesReportSkillOutcome", map[string]any{"skillId": "gs:no:pe", "ok": true}, "games_invalid_argument: skillId must be a bare id"},
		{"gamesReportSkillOutcome", map[string]any{"skillId": "gs-nope-" + n, "ok": true}, "games_skill_not_found: "},
		{"gamesDecideSkillPromotion", map[string]any{"skillId": skillID, "templateDigest": "", "approved": true}, "games_invalid_argument: skillId, templateDigest and approved are required"},
		{"gamesDecideSkillPromotion", map[string]any{"skillId": "gs-nope-" + n, "templateDigest": "abc", "approved": true}, "games_skill_not_found: "},
		{"gamesDecideSkillPromotion", map[string]any{"skillId": skillID, "templateDigest": "abc", "approved": true}, "games_skill_version_changed: "},
	} {
		if _, err := lg.call(t, ctx, "builtin", tc.call, tc.args); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s(%v) = %v, want it to lead with %q", tc.call, tc.args, err, tc.want)
		}
	}
	if gotCreated, gotPayload := lg.skillVersion(t, skillID); gotCreated != created || gotPayload != payload || lg.stored(t, conceptSkill, skillID) != versions {
		t.Fatalf("a refused call changed the skill: %s %s", gotCreated, gotPayload)
	}
	// the canonical spelling of an id names the same skill
	if res, err := lg.call(t, ctx, "builtin", "gamesReportSkillOutcome", map[string]any{"skillId": canonical(conceptSkill, skillID), "ok": true}); err != nil || res[0]["skillId"] != skillID {
		t.Errorf("a run reported by the skill's canonical id = %v, %v, want it counted for %s", res, err, skillID)
	}
}
