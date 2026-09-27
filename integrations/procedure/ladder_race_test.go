package procedure

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/work"
)

// ladder_race_test.go -- a ladder move is a read-advance-write, and the read
// must be the one the write is made against (review finding C1, epic
// memql#5408).
//
// A replay can run for minutes between loading its construct and finishing,
// and in that time the construct can be re-lifted, promoted or advanced by
// another comparison. Each test here makes that happen from INSIDE a dispatch
// -- the construct rewritten while the replay is in flight -- and asserts the
// finishing replay does not write its stale state back over it.

// hookDispatcher runs a hook before each dispatch, outside the inner
// dispatcher's lock, so the hook may run a whole replay of its own.
type hookDispatcher struct {
	inner  Dispatcher
	before func(req DispatchRequest)
}

func (h *hookDispatcher) Dispatch(ctx context.Context, req DispatchRequest) (DispatchResult, error) {
	if h.before != nil {
		h.before(req)
	}
	return h.inner.Dispatch(ctx, req)
}

// reliftTo rewrites the live construct the way the lift's re-lift does
// (persist.go): the entry rung on a clean slate FIRST, then the new version.
func reliftTo(w *replayWorld, rung, hash string) {
	w.lc.set("ladder", rung)
	w.lc.set("shadowMatches", float64(0))
	w.lc.set("canaryMatches", float64(0))
	w.lc.set("distinctBindings", map[string]any{})
	w.lc.set("failures", float64(0))
	w.lc.set("insufficient", float64(0))
	w.lc.set("promotionApprovalId", "")
	w.lc.set("procedureHash", hash)
}

// TestAReliftDuringAServedReplayIsNotUndoneByTheReplay (C1a): a trusted
// replay is in flight when the lift re-lifts the procedure -- a NEW version,
// put back on the entry rung because nobody has seen it. The replay of the
// OLD version still serves its goal (it was trusted when it started), but its
// finish must not write the ladder: the rung it loaded is `trusted`, and
// writing that back would serve the new version, never shadowed and never
// approved, with no model. The replay run says why the ladder did not move.
func TestAReliftDuringAServedReplayIsNotUndoneByTheReplay(t *testing.T) {
	w := newReplayWorld(t, "trusted")
	const newVersion = "sha256:the-relifted-version"
	w.d.alter["step0"] = func(*DispatchResult) { reliftTo(w, "shadow", newVersion) }

	out := w.serve(t, ReplayTrusted, map[string]any{"file": goalFile})
	if !out.Served {
		t.Fatalf("outcome = %+v, want the goal served by the version that was trusted when it started", out)
	}
	if got := w.lc.get("ladder"); got != "shadow" {
		t.Fatalf("the construct is %v after the replay, want shadow: the replay wrote its stale rung over the re-lift, "+
			"and the new version would now serve with no model", got)
	}
	if got := w.lc.get("procedureHash"); got != newVersion {
		t.Fatalf("procedureHash = %v", got)
	}
	if !out.VersionReplaced || out.Transition.From != "" {
		t.Fatalf("outcome = %+v, want the version named as replaced and no transition", out)
	}
	o, _ := w.work.run(out.ReplayRunId)["outcome"].(map[string]any)
	if o["versionReplaced"] != true || o["replacedBy"] != newVersion {
		t.Fatalf("the replay run's outcome = %v, want it to say a newer version replaced the one that ran", o)
	}
	if n := len(w.eng.callsTo("recordConstructReliability")); n != 0 {
		t.Fatalf("the reliability of the NEW version moved on a replay of the old one (%d writes)", n)
	}
}

// TestAPromotionDecidedDuringAShadowComparisonIsNotUndoneByIt (C1b): the
// person approves the open promotion while a shadow comparison is in flight,
// so the construct moves to canary with the approval cleared. The comparison's
// finish must not write `shadow` back pointing at the DECIDED approval -- the
// ladder never proposes while an approval is open, so the procedure would sit
// in shadow forever, waiting on a decision that was already made.
func TestAPromotionDecidedDuringAShadowComparisonIsNotUndoneByIt(t *testing.T) {
	w := newReplayWorld(t, "shadow")
	const open = "v1:work:approval:open"
	w.lc.set("promotionApprovalId", open)
	w.lc.set("shadowMatches", float64(5))
	w.d.alter["step0"] = func(*DispatchResult) {
		// DecidePromotion applying the person's yes.
		w.lc.set("ladder", "canary")
		w.lc.set("promotionApprovalId", "")
		w.lc.set("canaryMatches", float64(0))
	}

	out := w.shadowOf(t, "v1:work:run:rec-c", "c.txt")
	if !out.Match {
		t.Fatalf("outcome = %+v, want the comparison itself to match", out)
	}
	if got, approval := w.lc.get("ladder"), w.lc.get("promotionApprovalId"); got != "canary" || approval != "" {
		t.Fatalf("the construct is %v waiting on %q, want canary with nothing open: the comparison wrote its stale state "+
			"over the decision", got, approval)
	}
}

// TestInterleavedShadowComparisonsRaiseOnePromotion (C1c): with m=2 and k=2,
// two comparisons of two recordings are in flight at once and either would
// complete the streak. The one that finishes first proposes; the other must
// see that proposal and raise nothing -- two approvals for one version is two
// cards in a person's inbox, one of which the construct never points at.
func TestInterleavedShadowComparisonsRaiseOnePromotion(t *testing.T) {
	w := newReplayWorld(t, "shadow")
	w.policy(work.LadderPolicy{ShadowMatches: 2, DistinctBindings: 2, CanaryMatches: 5, FailuresToDemote: 2, InsufficientToDemote: 1, RetireAfterDays: 30})
	w.lc.set("shadowMatches", float64(1))
	w.lc.set("distinctBindings", map[string]any{"s0.command.7": []any{work.BindingDigest("a.txt")}})

	var inner ReplayOutcome
	fired := false
	hd := &hookDispatcher{inner: w.d, before: func(req DispatchRequest) {
		if fired || req.StepKey != "step0" {
			return
		}
		fired = true
		// Another recording's comparison runs start to finish while this one
		// is in flight.
		inner = w.shadowOf(t, "v1:work:run:rec-d", "d.txt")
	}}
	w.i.SetDispatcher(work.TargetWorkbench, hd)

	outer := w.shadowOf(t, "v1:work:run:rec-c", "c.txt")
	if !inner.Match || inner.ApprovalId == "" {
		t.Fatalf("the comparison that finished first = %+v, want it to propose", inner)
	}
	if !outer.Match || outer.ApprovalId != "" {
		t.Fatalf("the comparison that finished second = %+v, want a match that proposes nothing", outer)
	}
	if n := len(w.eng.callsTo("createWorkApproval")); n != 1 {
		t.Fatalf("%d approvals raised for one version, want exactly one", n)
	}
	if got := w.lc.get("promotionApprovalId"); got != inner.ApprovalId {
		t.Fatalf("the construct waits on %v, want the one approval raised (%s)", got, inner.ApprovalId)
	}
	if got := w.lc.get("shadowMatches"); got != float64(3) {
		t.Fatalf("shadowMatches = %v, want both comparisons counted (1 + 2)", got)
	}
}

// TestThePromotionApprovalIdIsDerivedFromWhatProposedIt (C1, part 4): the id
// is a digest of the construct, the version and the shadow run that proposed
// it -- so a proposal made twice for the same three writes ONE row, and any
// of the three changing is a different proposal.
func TestThePromotionApprovalIdIsDerivedFromWhatProposedIt(t *testing.T) {
	const construct, version, shadowRun = "v1:authoring:construct:c1", "sha256:v1", "v1:work:run:shadow1"
	id := promotionApprovalId(construct, version, shadowRun)
	if !strings.HasPrefix(id, "v1:work:approval:") || len(id) <= len("v1:work:approval:") {
		t.Fatalf("id = %q, want a v1:work:approval id", id)
	}
	if again := promotionApprovalId(construct, version, shadowRun); again != id {
		t.Fatalf("the same proposal derived %q then %q", id, again)
	}
	if bare := promotionApprovalId("c1", version, "shadow1"); bare != id {
		t.Fatalf("a bare spelling of the same construct and run derived %q, want %q", bare, id)
	}
	for name, other := range map[string]string{
		"another construct":  promotionApprovalId("v1:authoring:construct:c2", version, shadowRun),
		"another version":    promotionApprovalId(construct, "sha256:v2", shadowRun),
		"another shadow run": promotionApprovalId(construct, version, "v1:work:run:shadow2"),
	} {
		if other == id {
			t.Errorf("%s derived the same id", name)
		}
	}

	// Raised twice -- a redelivered proposal -- the same row is written.
	w := newReplayWorld(t, "shadow")
	c, err := w.i.loadForReplay(context.Background(), replayOwner, w.constructId)
	if err != nil {
		t.Fatal(err)
	}
	tr := work.Transition{From: work.RungShadow, To: work.RungShadow, Propose: true, State: work.LadderState{Rung: work.RungShadow, ShadowMatches: 5}}
	first, err := w.i.raisePromotion(context.Background(), replayOwner, c, shadowRun, tr, false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := w.i.raisePromotion(context.Background(), replayOwner, c, shadowRun, tr, false)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || first != promotionApprovalId(w.constructId, w.hash, shadowRun) {
		t.Fatalf("raised %q then %q, want the derived id both times", first, second)
	}
	for _, call := range w.eng.callsTo("createWorkApproval") {
		if got := parseCallArgs(t, call.Query)["approvalId"]; got != first {
			t.Fatalf("an approval was written as %v, want %s", got, first)
		}
	}
}

// TestAReplayTheSweepClosedUnderItStillRecordsWhatHappened: the abandoned-run
// sweep closed the replay's run while it was still working (its node was
// paused past the window). That is not another execution finishing it -- the
// sweep writes no outcome -- so the replay closes its run with what actually
// happened, and the ladder counts it. Only a run another execution closed
// WITH an outcome makes this one a duplicate.
func TestAReplayTheSweepClosedUnderItStillRecordsWhatHappened(t *testing.T) {
	w := newReplayWorld(t, "trusted")
	req := ReplayRequest{OwnerUserId: replayOwner, ConstructId: w.constructId, Mode: ReplayTrusted,
		GoalRunId: goalRunId, StepKey: "replayed", Input: map[string]any{"file": goalFile}}
	runId, _ := (&replay{mode: ReplayTrusted, req: req, c: &loaded{id: w.constructId, hash: w.hash}}).replayRunId()
	w.d.alter["step1"] = func(*DispatchResult) {
		row := copyRow(t, w.work.run(runId))
		row["status"], row["errorCode"], row["finishedAt"] = "abandoned", "heartbeat_expired", testNow.Format(timeLayout)
		w.work.putRun(row)
	}
	out, err := w.i.Replay(context.Background(), req)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if !out.Served || out.AlreadyDone {
		t.Fatalf("outcome = %+v, want the goal served and recorded by this replay", out)
	}
	if run := w.work.run(runId); run["status"] != "succeeded" {
		t.Fatalf("the run = %v, want it closed with what happened", run)
	}
	if w.lc.get("lastReplayAt") != testNow.Format(timeLayout) {
		t.Fatal("the replay that served the goal was not counted")
	}
}

// TestAReplayAnotherExecutionFinishedCountsAndHandsOverNothing: two
// executions of one goal statement ran the same replay run (the goal's run
// was reclaimed while its first executor was still alive). The other one
// finished first -- it closed the run with an outcome and handed the goal to
// the app. This one adopts that answer: it neither counts the replay again nor
// hands the goal over a second time.
func TestAReplayAnotherExecutionFinishedCountsAndHandsOverNothing(t *testing.T) {
	w := newReplayWorld(t, "trusted")
	req := ReplayRequest{OwnerUserId: replayOwner, ConstructId: w.constructId, Mode: ReplayTrusted,
		GoalRunId: goalRunId, StepKey: "replayed", Input: map[string]any{"file": goalFile}}
	runId, _ := (&replay{mode: ReplayTrusted, req: req, c: &loaded{id: w.constructId, hash: w.hash}}).replayRunId()
	w.d.alter["step1"] = func(r *DispatchResult) {
		r.Observation.Contents[0].Digest = "sha256:" + strings.Repeat("0", 64)
		row := copyRow(t, w.work.run(runId))
		row["status"], row["errorCode"] = "failed", codeDiverged
		row["outcome"] = map[string]any{"diverged": true, "divergedStep": float64(1), "repairRunId": "v1:work:run:repair-other"}
		w.work.putRun(row)
	}
	out, err := w.i.Replay(context.Background(), req)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if !out.AlreadyDone || !out.FellBack || out.Fallback.ChildRunId != "v1:work:run:repair-other" {
		t.Fatalf("outcome = %+v, want the other execution's answer adopted", out)
	}
	if n := len(w.f.recorded()); n != 0 {
		t.Fatalf("the goal was handed to the app %d more time(s)", n)
	}
	if n := len(w.eng.callsTo("recordConstructLadder")); n != 0 || w.lc.get("failures") != float64(0) {
		t.Fatalf("the replay was counted a second time: %d ladder writes, failures %v", n, w.lc.get("failures"))
	}
}
