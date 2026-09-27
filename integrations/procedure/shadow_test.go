package procedure

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/work"
)

// shadow_test.go -- a succeeded recording compared beside the app (epic
// memql#5408, D15; plan Task 5 step 3). The recording is served to the
// comparison exactly as the corpus loader reads it, through the fixture rows
// seedCorpus answers, and the construct is the one the real lift wrote.

// shadowWorld is a replay world whose construct is in shadow and whose
// engine also answers every read a recording and the lookup by goal need.
func shadowWorld(t *testing.T, recs ...recFixture) *replayWorld {
	t.Helper()
	w := newReplayWorld(t, "shadow")
	seedCorpus(t, w.eng, recs...)
	for _, r := range recs {
		w.work.putRun(r.runRow())
	}
	w.eng.answer("procedureConstructsForGoalSignature", func(recordedCall, string) ([]map[string]any, string) {
		w.lc.mu.Lock()
		defer w.lc.mu.Unlock()
		return []map[string]any{copyRow(t, w.lc.row)}, ""
	})
	return w
}

// TestASucceededRecordingIsComparedBesideTheApp: the recording binds the
// procedure's parameter from the app's own action, the procedure replays in
// the workbench SANDBOX -- never the person's machine -- and every step
// matches what the app did, so the streak grows and the binding is counted
// by its digest, never its value.
func TestASucceededRecordingIsComparedBesideTheApp(t *testing.T) {
	rec := recording1("c.txt", testNow.Add(-10*time.Minute))
	w := shadowWorld(t, rec)

	outs, err := w.i.ShadowCompare(personCtx(testOwner), rec.runId)
	if err != nil {
		t.Fatalf("ShadowCompare: %v", err)
	}
	if len(outs) != 1 || !outs[0].Match {
		t.Fatalf("outcomes = %+v, want one match", outs)
	}
	calls := w.d.recorded()
	if len(calls) != 2 {
		t.Fatalf("dispatched %v, want both steps", dispatchedKeys(calls))
	}
	for _, c := range calls {
		if !c.Sandbox || c.Target != work.TargetWorkbench {
			t.Fatalf("a shadow step ran %+v: a shadow never touches anybody's machine", c)
		}
	}
	if got := calls[0].Args["command"]; got != "mkdir -p out && echo hello > c.txt" {
		t.Fatalf("the parameter was bound as %q, want the app's own c.txt", got)
	}
	if w.lc.get("shadowMatches") != float64(1) {
		t.Fatalf("shadowMatches = %v", w.lc.get("shadowMatches"))
	}
	bindings, _ := w.lc.get("distinctBindings").(map[string]any)
	digests, _ := bindings["s0.command.7"].([]any)
	if len(digests) != 1 || digests[0] != work.BindingDigest("c.txt") {
		t.Fatalf("distinctBindings = %v, want the one digest", bindings)
	}
	if run := w.work.run(outs[0].ReplayRunId); run["triggeredBy"] != "procedure:shadow" || run["parentRunId"] != rec.runId {
		t.Fatalf("the shadow replay run = %v", run)
	}
}

// TestARecordingTheProcedureDoesNotFitIsAShadowMismatch (#5410): the app did
// this goal some other way -- a command the procedure holds constant said
// something else, or the procedure's steps are not in the recording at all.
// That is a MISMATCH: the streak starts again, the mismatch is recorded on a
// replay run, and nothing is dispatched. A streak that ignored such a
// recording would promote a procedure on the recordings it happens to fit.
func TestARecordingTheProcedureDoesNotFitIsAShadowMismatch(t *testing.T) {
	for name, cmd := range map[string]string{
		"a constant differs":      "mkdir -p out && echo bye > c.txt",
		"the steps are not there": "make build",
	} {
		t.Run(name, func(t *testing.T) {
			rec := recording1("c.txt", testNow.Add(-10*time.Minute))
			rec.execCommand = cmd
			w := shadowWorld(t, rec)
			w.lc.set("shadowMatches", float64(1))
			w.lc.set("distinctBindings", map[string]any{"s0.command.7": []any{work.BindingDigest("a.txt")}})

			outs, err := w.i.ShadowCompare(personCtx(testOwner), rec.runId)
			if err != nil {
				t.Fatalf("ShadowCompare: %v", err)
			}
			if len(outs) != 1 || outs[0].Match || !outs[0].Diverged || !strings.Contains(outs[0].Diagnosis, "some other way") {
				t.Fatalf("outcomes = %+v, want one mismatch saying why", outs)
			}
			if len(w.d.recorded()) != 0 {
				t.Fatalf("a recording the procedure does not fit dispatched %v", dispatchedKeys(w.d.recorded()))
			}
			if w.lc.get("shadowMatches") != float64(0) {
				t.Fatalf("shadowMatches = %v: a mismatch starts the streak again", w.lc.get("shadowMatches"))
			}
			if b, _ := w.lc.get("distinctBindings").(map[string]any); len(b) != 0 {
				t.Fatalf("distinctBindings = %v, want the streak's bindings cleared", b)
			}
			run := w.work.run(outs[0].ReplayRunId)
			if run["status"] != "failed" || run["errorCode"] != codeMismatch {
				t.Fatalf("the mismatch was not recorded: %v", run)
			}
		})
	}
}

// TestWhatIsNotARecordingIsNotCompared: the goal's own run (statements, no
// actions), a replay run and a recording that did not succeed say nothing
// about whether the procedure does what the app does -- no comparison, and
// no mismatch either.
func TestWhatIsNotARecordingIsNotCompared(t *testing.T) {
	rec := recording1("c.txt", testNow.Add(-10*time.Minute))
	w := shadowWorld(t, rec)

	replay := rec.runRow()
	replay["id"], replay["triggeredBy"] = "v1:work:run:replay-x", "procedure:trusted"
	failed := rec.runRow()
	failed["id"], failed["status"] = "v1:work:run:failed-x", "failed"
	goal := rec.runRow()
	goal["id"] = "v1:work:run:the-goal"
	w.eng.replyWhen("workStepsForOwnerRun", `"v1:work:run:the-goal"`, map[string]any{"key": "call0", "seq": float64(0), "stepType": "builtin"})
	for _, run := range []map[string]any{replay, failed, goal} {
		outs, err := w.i.shadowCompareRun(context.Background(), run)
		if err != nil || len(outs) != 0 {
			t.Fatalf("%s was compared: %+v, %v", run["id"], outs, err)
		}
	}
	if len(w.eng.callsTo("recordConstructLadder")) != 0 {
		t.Fatal("something that is not a recording moved the ladder")
	}
}

// TestAPersonCannotCompareSomebodyElsesRecording: the recording is read
// through the owned read and belongs to its owner's ladder alone.
func TestAPersonCannotCompareSomebodyElsesRecording(t *testing.T) {
	rec := recording1("c.txt", testNow.Add(-10*time.Minute))
	w := shadowWorld(t, rec)
	if _, err := w.i.ShadowCompare(personCtx("v1:identity:user:mallory"), rec.runId); err == nil {
		t.Fatal("mallory compared alice's recording")
	}
	if len(w.eng.callsTo("recordConstructLadder")) != 0 {
		t.Fatal("a refused comparison moved the ladder")
	}
}

// TestTheLearnHandlerComparesOnlyAfterAnUnchangedLift: the completion trigger
// lifts, and -- only when the lift left the version UNCHANGED -- compares the
// recording that fired it. A recording that CHANGED the procedure is part of
// the new version's corpus, not evidence about it. The exported LearnFromRun
// never compares: its callers compare themselves, and would otherwise count a
// recording twice.
func TestTheLearnHandlerComparesOnlyAfterAnUnchangedLift(t *testing.T) {
	recs := append(twoRecordings(), recording1("c.txt", testNow.Add(-10*time.Minute)))
	third := recs[2]

	w := shadowWorld(t, recs...)
	w.eng.answer("procedureConstructByName", func(recordedCall, string) ([]map[string]any, string) {
		w.lc.mu.Lock()
		defer w.lc.mu.Unlock()
		return []map[string]any{copyRow(t, w.lc.row)}, ""
	})
	w.i.SetCompiler(&passingGate{})
	nodes, err := w.i.handleLearnFromRun(triggerCtx(), map[string]any{"runId": third.runId, "ownerUserId": testOwner}, 0)
	if err != nil {
		t.Fatalf("handleLearnFromRun: %v", err)
	}
	var reply map[string]any
	if err := json.Unmarshal(nodes[0].Payload, &reply); err != nil {
		t.Fatal(err)
	}
	if reply["lift"] != string(LiftUnchanged) || reply["shadowCompared"] != float64(1) || reply["shadowMatched"] != float64(1) {
		t.Fatalf("reply = %v, want an unchanged lift and one matching comparison", reply)
	}
	if w.lc.get("shadowMatches") != float64(1) {
		t.Fatalf("shadowMatches = %v", w.lc.get("shadowMatches"))
	}

	// The exported LearnFromRun, same recording, same state: no comparison.
	before := len(w.d.recorded())
	res, err := w.i.LearnFromRun(personCtx(testOwner), third.runId)
	if err != nil || res.Lift != LiftUnchanged {
		t.Fatalf("LearnFromRun = %+v, %v", res, err)
	}
	if len(w.d.recorded()) != before {
		t.Fatal("the exported LearnFromRun compared the recording; its callers compare themselves")
	}

	// A CREATED lift compares nothing: there was no version to compare with.
	fresh := newFakeEngine()
	seedCorpus(t, fresh, twoRecordings()...)
	fresh.replyWhen("workRunForOwner", `"`+twoRecordings()[1].runId+`"`, twoRecordings()[1].runRow())
	i := newTestIntegration(fresh)
	i.SetCompiler(&passingGate{})
	nodes, err = i.handleLearnFromRun(triggerCtx(), map[string]any{"runId": twoRecordings()[1].runId, "ownerUserId": testOwner}, 0)
	if err != nil {
		t.Fatalf("handleLearnFromRun (created): %v", err)
	}
	reply = map[string]any{} // a fresh map: Unmarshal into the old one would MERGE, keeping its keys
	_ = json.Unmarshal(nodes[0].Payload, &reply)
	if reply["lift"] != string(LiftCreated) {
		t.Fatalf("reply = %v, want a created lift", reply)
	}
	if _, compared := reply["shadowCompared"]; compared || len(fresh.callsTo("procedureConstructsForGoalSignature")) != 0 {
		t.Fatalf("a created lift was compared: %v", reply)
	}
}

// TestAShadowComparisonOnANodeWithNoWorkbenchIsNotCompared: a run's
// completion event is broadcast, so the comparison runs on whichever replica
// claimed it -- and a planner, an mcp node or a bff without a remote
// workbench has no dispatcher to replay on. That is NOT a mismatch: nothing
// is written, the streak stands, and the next recording's comparison on a
// node that can make it counts as it would have. A streak must not depend on
// which replica heard the event.
func TestAShadowComparisonOnANodeWithNoWorkbenchIsNotCompared(t *testing.T) {
	rec := recording1("c.txt", testNow.Add(-10*time.Minute))
	w := shadowWorld(t, rec)
	w.lc.set("shadowMatches", float64(1))
	w.i.seams.mu.Lock()
	w.i.seams.dispatchers = nil
	w.i.seams.mu.Unlock()

	outs, err := w.i.ShadowCompare(personCtx(testOwner), rec.runId)
	if err != nil {
		t.Fatalf("ShadowCompare: %v", err)
	}
	if len(outs) != 1 || !outs[0].NotCompared || outs[0].Match || outs[0].Diverged || outs[0].Code != codeNoWorkbench {
		t.Fatalf("outcomes = %+v, want one comparison not made here", outs)
	}
	for _, name := range []string{"createWorkRun", "recordConstructLadder", "recordConstructReliability"} {
		if n := len(w.eng.callsTo(name)); n != 0 {
			t.Fatalf("a comparison not made here wrote %s %d time(s)", name, n)
		}
	}
	if w.lc.get("shadowMatches") != float64(1) {
		t.Fatalf("shadowMatches = %v: the streak must stand", w.lc.get("shadowMatches"))
	}
}

// TestAShadowWhoseFirstStepTheSandboxRefusesIsNotCompared: the sandbox saying
// no to the first step means nothing ran on this node, which says nothing
// about whether the procedure does what the app does.
func TestAShadowWhoseFirstStepTheSandboxRefusesIsNotCompared(t *testing.T) {
	rec := recording1("c.txt", testNow.Add(-10*time.Minute))
	w := shadowWorld(t, rec)
	w.lc.set("shadowMatches", float64(1))
	w.d.fail["step0"] = errors.New("sandbox refused the command")
	outs, err := w.i.ShadowCompare(personCtx(testOwner), rec.runId)
	if err != nil {
		t.Fatalf("ShadowCompare: %v", err)
	}
	if len(outs) != 1 || !outs[0].NotCompared || outs[0].Match {
		t.Fatalf("outcomes = %+v, want a comparison not made", outs)
	}
	if len(w.eng.callsTo("recordConstructLadder")) != 0 || w.lc.get("shadowMatches") != float64(1) {
		t.Fatalf("a refused first step moved the ladder: shadowMatches %v", w.lc.get("shadowMatches"))
	}
}

// TestTheDispatcherSeesTheOwnersForwardedAuthorityOnTheShadowPath: with the
// workbench on its own node, a step is FORWARDED over the mesh, and the
// forward fails closed without an assertion of whose work it is. A shadow
// comparison runs from the completion trigger with only the borrowed owner,
// so the replay binds the writer-scoped assertion the work spine gives
// background work -- for the OWNER -- before it probes or dispatches. A
// served replay runs inside the goal's work dispatch, which bound one
// already, and that one is kept, not replaced.
func TestTheDispatcherSeesTheOwnersForwardedAuthorityOnTheShadowPath(t *testing.T) {
	rec := recording1("c.txt", testNow.Add(-10*time.Minute))
	w := shadowWorld(t, rec)
	if _, err := w.i.ShadowCompare(personCtx(testOwner), rec.runId); err != nil {
		t.Fatalf("ShadowCompare: %v", err)
	}
	seen := append(append([]forwardedSeen(nil), w.p.authorities...), w.d.authorities...)
	if len(w.d.authorities) != 2 || len(w.p.authorities) != 1 {
		t.Fatalf("probes %d, dispatches %d; want one probe and both steps", len(w.p.authorities), len(w.d.authorities))
	}
	for n, a := range seen {
		if !a.present || !sameUser(a.subject, testOwner) || a.credentialClass != auth.ForwardedClassUser {
			t.Fatalf("seam call %d saw %+v, want the owner's writer-scoped user authority", n, a)
		}
	}

	served := newReplayWorld(t, "trusted")
	bound := auth.ContextWithForwardedAuthority(personCtx(replayOwner), auth.ForwardedAuthority{
		Subject: replayOwner, CredentialClass: auth.ForwardedClassPat,
	})
	if _, err := served.i.Replay(bound, ReplayRequest{OwnerUserId: replayOwner, ConstructId: served.constructId,
		Mode: ReplayTrusted, GoalRunId: goalRunId, Input: map[string]any{"file": goalFile}}); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	for n, a := range served.d.authorities {
		if !a.present || a.credentialClass != auth.ForwardedClassPat {
			t.Fatalf("served dispatch %d saw %+v, want the work dispatch's own authority kept", n, a)
		}
	}
}

// TestARecordingTheVersionWasLearnedFromIsNotEvidenceAboutIt: the recording
// that created (or re-lifted) a version is part of that version's corpus. The
// completion trigger never compares it -- the lift was not unchanged -- but a
// second procedureLearnFromRun on the same run finds the lift unchanged and
// would compare it, counting a recording toward the very procedure it taught.
// The version's own recordedFrom names it, and it is not compared.
func TestARecordingTheVersionWasLearnedFromIsNotEvidenceAboutIt(t *testing.T) {
	rec := recording1("c.txt", testNow.Add(-10*time.Minute))
	w := shadowWorld(t, rec)
	w.withProcedure(t, func(p map[string]any) {
		from, _ := p["recordedFrom"].(map[string]any)
		from["runIds"] = append(from["runIds"].([]any), rec.runId)
	})
	outs, err := w.i.ShadowCompare(personCtx(testOwner), rec.runId)
	if err != nil {
		t.Fatalf("ShadowCompare: %v", err)
	}
	if len(outs) != 0 || len(w.d.recorded()) != 0 || len(w.eng.callsTo("recordConstructLadder")) != 0 {
		t.Fatalf("a recording the version was learned from was compared with it: %+v", outs)
	}

	// The control: the same recording, not among the version's own, is.
	control := shadowWorld(t, rec)
	if outs, err := control.i.ShadowCompare(personCtx(testOwner), rec.runId); err != nil || len(outs) != 1 || !outs[0].Match {
		t.Fatalf("the control compared %+v, %v -- so the refusal above proves nothing", outs, err)
	}
}
