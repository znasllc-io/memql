package procedure

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

// ownerDislikes answers workDescriptionGuidance as the owned query does: the
// goal signature's dislikes, to their owner alone.
func ownerDislikes(t *testing.T, w *replayWorld, reason string) {
	t.Helper()
	w.eng.answerSome("workDescriptionGuidance", func(c recordedCall, _ string) ([]map[string]any, string, bool) {
		sig, _ := parseCallArgs(t, c.Query)["goalSignature"].(string)
		if sig != testSignature || !sameUser(replayOwner, c.Actor) {
			return nil, "", true
		}
		return []map[string]any{{
			"id": "v1:work:observation:dislike-1", "kind": "feedback", "ownerUserId": replayOwner,
			"data": map[string]any{"verdict": "dislike", "reason": reason, "goalSignature": testSignature,
				"axes": map[string]any{"product": true, "process": false, "performance": false}},
		}}, "", true
	})
}

// A goal handed back to the app carries the owner's earlier dislikes on the
// goal (epic memql#5414, D23). Text reaches only a model call, and the app
// taking the goal back is one; the replay that stopped read rows.
func TestAHandBackCarriesTheGoalsDescriptionGuidance(t *testing.T) {
	w := newReplayWorld(t, "trusted")
	ownerDislikes(t, w, "the report left out the refunds")
	w.d.alter["step1"] = func(r *DispatchResult) {
		r.Observation.Contents[0].Digest = "sha256:" + strings.Repeat("0", 64)
	}
	out := w.serve(t, ReplayTrusted, map[string]any{"file": goalFile})
	if !out.Diverged || !out.FellBack {
		t.Fatalf("outcome = %+v, want a divergence handed back to the app", out)
	}
	g := w.f.recorded()[0].Guidance
	want := work.DescriptionGuidanceHeading + "\n- (product) the report left out the refunds"
	if g.DescriptionGuidance != want {
		t.Fatalf("guidance = %q, want the owner's dislike read under their actor", g.DescriptionGuidance)
	}
	if !strings.HasSuffix(g.Prompt, "\n\n"+want) || !strings.Contains(g.Prompt, "It stopped at step 2 because:") {
		t.Fatalf("the prompt does not close on the owner's dislikes:\n%s", g.Prompt)
	}
}

// "Never into a replay" (D23): a replay that serves the goal reads no
// description guidance at all, because nothing it does can act on text.
func TestAServedReplayNeverReadsDescriptionGuidance(t *testing.T) {
	w := newReplayWorld(t, "trusted")
	ownerDislikes(t, w, "the report left out the refunds")
	out := w.serve(t, ReplayTrusted, map[string]any{"file": goalFile})
	if !out.Served || out.FellBack {
		t.Fatalf("outcome = %+v, want the goal served by the replay", out)
	}
	for _, c := range w.eng.recorded() {
		if c.Name() == "workDescriptionGuidance" {
			t.Fatalf("a served replay read the goal's description guidance: %s", c.Query)
		}
	}
}

// A hand-back before any step ran says nothing was done, and still carries
// the owner's dislikes, as its own paragraph after the diagnosis.
func TestAHandBackThatRanNothingStillCarriesTheDislikes(t *testing.T) {
	guidance := work.DescriptionGuidanceHeading + "\n- (process) it asked me three times"
	got := renderGuidance("Summarise the invoices.", Guidance{Diagnosis: "the input file is missing.", DescriptionGuidance: guidance}, false, 0)
	want := "Summarise the invoices.\n\nA learned procedure for this goal did not start, so nothing has been done yet: the input file is missing.\n\n" + guidance
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	if got := renderGuidance("Summarise the invoices.", Guidance{Diagnosis: "the input file is missing."}, false, 0); strings.HasSuffix(got, "\n\n") {
		t.Fatalf("no dislikes must add no empty paragraph: %q", got)
	}
}

// rerunInGoalRun is inGoalRun for the one step a person re-ran: the executor
// puts their override on that step's context and on no other.
func rerunInGoalRun(ov *common.StepOverride) context.Context {
	ctx := auth.ContextWithUserActor(context.Background(), replayOwner)
	return common.ContextWithRun(ctx, common.RunContext{
		RunId: goalRunId, GoalId: goalId, StepKey: "replayed", Mode: common.RunModeLive, OwnerUserId: replayOwner,
		Override: ov,
	})
}

// A RE-RUN WITH SOMETHING A REPLAY CANNOT HONOUR GOES TO THE APP (epic
// memql#5414, D20 and D23). Replayed again, the procedure would answer exactly
// what the person asked to change; so nothing is replayed, nothing on the
// ladder moves, and the app is handed the goal with the person's instructions,
// what was wrong with the version it replaces, and the goal's earlier
// dislikes.
func TestARerunOfAProcedureServedStepIsHandedToTheApp(t *testing.T) {
	w := newReplayWorld(t, "trusted")
	ownerDislikes(t, w, "the report left out the refunds")
	ov := &common.StepOverride{
		Level: "strong", Prompt: "Include the refunds this time.",
		GuidanceAxes: []string{"product"}, GuidanceReason: "the refunds are missing",
	}
	reply, err := decodeServe(t, w, rerunInGoalRun(ov))
	if err != nil {
		t.Fatalf("procedureReplay: %v", err)
	}
	if reply["servedBy"] != "app" || reply["rerun"] != true || reply["repairRunId"] != "v1:work:run:repair-1" {
		t.Fatalf("reply = %v, want the app to have taken the re-run", reply)
	}
	if n := len(w.d.recorded()); n != 0 {
		t.Fatalf("the procedure was replayed %d steps deep; a re-run with an override must not replay", n)
	}
	if w.lc.get("ladder") != "trusted" {
		t.Fatalf("ladder = %v: nothing was tried, so nothing on the ladder may move", w.lc.get("ladder"))
	}
	fb := w.f.recorded()
	if len(fb) != 1 {
		t.Fatalf("the app was handed the goal %d times, want once", len(fb))
	}
	prompt := fb[0].Guidance.Prompt
	if !containsAll(prompt, testStatement, rerunDiagnosis,
		"Instructions for this step from its owner:\nInclude the refunds this time.",
		"What was wrong with the previous version (product): the refunds are missing",
		work.DescriptionGuidanceHeading+"\n- (product) the report left out the refunds") {
		t.Fatalf("the app was not told what the person asked for:\n%s", prompt)
	}
	if fb[0].StepId != journalStepId(goalRunId, "replayed") || fb[0].App != "claude-code" {
		t.Fatalf("hand-back = %+v", fb[0])
	}

	// A WHOLE PROMPT IS NOT REPEATED AS INSTRUCTIONS: the session takes it as
	// it is.
	whole := &common.StepOverride{Prompt: "Write the whole report again, refunds included.", WholePrompt: true}
	if _, err := decodeServe(t, w, rerunInGoalRun(whole)); err != nil {
		t.Fatal(err)
	}
	if got := w.f.recorded()[1].Guidance.Prompt; strings.Contains(got, "Instructions for this step") {
		t.Fatalf("a whole session prompt was repeated as instructions:\n%s", got)
	}
}

// "Run again" with nothing changed still replays: the procedure is the goal's
// answer, and running it again is a request it can serve.
func TestARerunWithNothingChangedStillReplays(t *testing.T) {
	w := newReplayWorld(t, "trusted")
	reply, err := decodeServe(t, w, rerunInGoalRun(&common.StepOverride{RequestedBy: replayOwner}))
	if err != nil {
		t.Fatalf("procedureReplay: %v", err)
	}
	if reply["servedBy"] != "procedure" || len(w.f.recorded()) != 0 {
		t.Fatalf("reply = %v, hand-backs %d: an unchanged re-run must replay", reply, len(w.f.recorded()))
	}
}

// A pin that is not an app is refused before anything opens: a goal a
// procedure served goes to an app session or nowhere.
func TestARerunPinnedToAModelThatIsNotAnAppIsRefused(t *testing.T) {
	w := newReplayWorld(t, "trusted")
	_, err := decodeServe(t, w, rerunInGoalRun(&common.StepOverride{Model: "fleet:llama-3.3"}))
	if err == nil || !strings.Contains(err.Error(), `"fleet:llama-3.3", which is not one`) {
		t.Fatalf("err = %v, want the non-app pin refused by name", err)
	}
	if len(w.f.recorded()) != 0 || len(w.d.recorded()) != 0 {
		t.Fatal("a refused re-run still reached the app or the target")
	}
}
