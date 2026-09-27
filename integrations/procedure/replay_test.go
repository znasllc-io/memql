package procedure

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
	proc "github.com/znasllc-io/memql/component/procedure"
	"github.com/znasllc-io/memql/component/work"
)

// replay_test.go -- the replay runner (epic memql#5408, plan Task 5 step 2).
// Every test drives the production sequence over a construct the REAL lift
// wrote (newReplayWorld), through faked seams that record what they were
// asked, against a store that moves the way the rows do.

// TestATrustedProcedureServesTheGoalWithNoModelAndNoApp is the ladder's point:
// a trusted procedure answers its goal by replaying its template on the
// workbench, reaching no model and handing nothing to the app -- and the
// record says so: a replay run that is never a recording, a step row per
// template step under its idempotency key, the ladder and the reliability.
func TestATrustedProcedureServesTheGoalWithNoModelAndNoApp(t *testing.T) {
	w := newReplayWorld(t, "trusted")
	out := w.serve(t, ReplayTrusted, map[string]any{"file": goalFile})

	if !out.Served || out.Diverged || out.StartRefused || out.FellBack {
		t.Fatalf("outcome = %+v, want served", out)
	}
	if out.ModelCalls != 0 || len(w.f.recorded()) != 0 {
		t.Fatalf("a trusted replay reached a model (%d) or the app (%d)", out.ModelCalls, len(w.f.recorded()))
	}
	calls := w.d.recorded()
	if !reflect.DeepEqual(dispatchedKeys(calls), []string{"step0", "step1"}) {
		t.Fatalf("dispatched %v, want every step once, in order", dispatchedKeys(calls))
	}
	for n, c := range calls {
		if c.Target != work.TargetWorkbench || c.Sandbox || c.RunId != out.ReplayRunId || c.OwnerUserId != replayOwner {
			t.Errorf("dispatch %d = %+v, want the workbench, for real, in the replay run, as the owner", n, c)
		}
		if want := work.IdempotencyKey(out.ReplayRunId, c.StepKey, 1); c.IdempotencyKey != want {
			t.Errorf("dispatch %d idempotency key %q, want %q", n, c.IdempotencyKey, want)
		}
	}
	if got := calls[0].Args["command"]; got != "mkdir -p out && echo "+"hello > "+goalFile {
		t.Errorf("the command bound the goal's input as %q", got)
	}
	if got := calls[1].Args["file_path"]; got != relativeReportPath {
		t.Errorf("the write names %q, want the workspace-relative %q", got, relativeReportPath)
	}

	run := w.work.run(out.ReplayRunId)
	if run == nil {
		t.Fatalf("no replay run %s was written", out.ReplayRunId)
	}
	if run["status"] != "succeeded" || run["triggeredBy"] != "procedure:trusted" || run["parentRunId"] != goalRunId ||
		run["templateConstructId"] != w.constructId {
		t.Fatalf("replay run = %v", run)
	}
	if _, hasGoal := run["goalId"]; hasGoal {
		t.Fatal("a replay run must carry no goalId: integrations/work would dispatch it as a template and run it twice")
	}
	if o, _ := run["outcome"].(map[string]any); o["servedBy"] != "procedure" {
		t.Fatalf("outcome = %v, want servedBy procedure", run["outcome"])
	}
	steps := w.work.stepsOf(out.ReplayRunId)
	for _, key := range []string{"step0", "step1"} {
		s := steps[key]
		if s["status"] != "done" || s["stepType"] != "procedureStep" || s["idempotencyKey"] != work.IdempotencyKey(out.ReplayRunId, key, 1) {
			t.Errorf("step %s = %v", key, s)
		}
		if c, _ := s["call"].(map[string]any); c["construct"] != "procedure" || c["name"] != w.name {
			t.Errorf("step %s call = %v", key, s["call"])
		}
	}
	if out.Transition.To != work.RungTrusted || w.lc.get("lastReplayAt") != testNow.Format(timeLayout) {
		t.Errorf("ladder = %+v, lastReplayAt %v", out.Transition, w.lc.get("lastReplayAt"))
	}
	if w.lc.get("reliability") != 0.2 || w.lc.get("reinforceCount") != float64(1) {
		t.Errorf("reliability %v / count %v, want the first reinforcement", w.lc.get("reliability"), w.lc.get("reinforceCount"))
	}
	for _, c := range w.eng.writes() {
		if !c.Internal || c.Actor != replayOwner {
			t.Errorf("%s written internal=%v as %q, want the stamp and the owner", c.Name(), c.Internal, c.Actor)
		}
	}
}

// TestAProcedureDemotedSinceCompileFallsBackAtExecution (Review Focus 3):
// compile chose a trusted procedure; by the time the goal's run executes it
// was demoted, re-lifted or retired. The rung is re-read and the goal goes to
// the app -- nothing dispatched, no replay run opened, the ladder untouched:
// the rung it already moved to is the whole of the story.
func TestAProcedureDemotedSinceCompileFallsBackAtExecution(t *testing.T) {
	for _, rung := range []string{"shadow", "candidate", "retired"} {
		t.Run(rung, func(t *testing.T) {
			w := newReplayWorld(t, rung)
			out := w.serve(t, ReplayTrusted, map[string]any{"file": goalFile})
			if !out.StartRefused || out.Served || out.Code != codeNotServable {
				t.Fatalf("outcome = %+v, want a refusal naming the rung", out)
			}
			if len(w.d.recorded()) != 0 {
				t.Fatalf("a %s procedure dispatched %v", rung, dispatchedKeys(w.d.recorded()))
			}
			if n := len(w.f.recorded()); n != 1 || !out.FellBack {
				t.Fatalf("the app was handed the goal %d time(s), want once", n)
			}
			if len(w.eng.callsTo("createWorkRun")) != 0 || len(w.eng.callsTo("recordConstructLadder")) != 0 {
				t.Fatalf("a refused start wrote %s", w.eng.summary())
			}
		})
	}
}

// TestAMachineLocalFootprintSentToTheWorkbenchIsRefusedBeforeTheFirstStep
// (D4): the stored target says workbench and the footprint names a machine's
// own files. A machine-local procedure that failed at step three on the
// workbench has already run steps one and two somewhere they mean nothing, so
// it is refused BEFORE the first -- no Dispatch at all -- and the goal goes to
// the app.
func TestAMachineLocalFootprintSentToTheWorkbenchIsRefusedBeforeTheFirstStep(t *testing.T) {
	w := newReplayWorld(t, "trusted")
	w.withProcedure(t, func(p map[string]any) {
		p["footprint"] = map[string]any{"files": true, "machine": true}
		p["target"] = "workbench"
	})
	machine := newFakeDispatcher()
	w.i.SetDispatcher(work.TargetMachine, machine)

	out := w.serve(t, ReplayTrusted, map[string]any{"file": goalFile})
	if !out.StartRefused || out.Code != codeStartRefused || !strings.Contains(out.Diagnosis, "workbench") {
		t.Fatalf("outcome = %+v, want a refusal naming the workbench", out)
	}
	if n := len(w.d.recorded()) + len(machine.recorded()); n != 0 {
		t.Fatalf("%d step(s) were dispatched before the refusal", n)
	}
	if len(w.f.recorded()) != 1 {
		t.Fatalf("the app was handed the goal %d time(s), want once", len(w.f.recorded()))
	}
	if w.lc.get("failures") != float64(1) {
		t.Errorf("failures = %v: a trusted procedure that cannot start failed the goal it was chosen for", w.lc.get("failures"))
	}
	if run := w.work.run(out.ReplayRunId); run == nil || run["status"] != "failed" {
		t.Fatalf("the refusal was not recorded on a replay run: %v", run)
	}
}

// TestAMachineProcedureOnANodeWithNoMachineDispatcherIsANamedRefusal: the NODE
// lacks a seam, which says nothing about the procedure -- the refusal names
// it and the ladder does not count it.
func TestAMachineProcedureOnANodeWithNoMachineDispatcherIsANamedRefusal(t *testing.T) {
	w := newReplayWorld(t, "trusted")
	w.withProcedure(t, func(p map[string]any) {
		p["footprint"] = map[string]any{"files": true, "machine": true}
		p["target"] = "machine"
	})
	out := w.serve(t, ReplayTrusted, map[string]any{"file": goalFile})
	if !out.StartRefused || out.Code != codeNoMachineDispatcher || !out.FellBack {
		t.Fatalf("outcome = %+v, want no_machine_dispatcher and a hand-back", out)
	}
	if len(w.eng.callsTo("recordConstructLadder")) != 0 {
		t.Fatal("a missing seam moved the ladder")
	}
}

// TestTrustedReplayWithAnUnboundParameterFallsBackBeforeTheFirstStep (Review
// Focus 2): a goal whose input cannot supply a free parameter -- absent,
// empty, or a value with no literal spelling -- is served by the app. A replay
// NEVER runs with a hole bound to nothing: it would do something nobody asked
// for.
func TestTrustedReplayWithAnUnboundParameterFallsBackBeforeTheFirstStep(t *testing.T) {
	for name, input := range map[string]map[string]any{
		"absent":    {},
		"empty":     {"file": ""},
		"an object": {"file": map[string]any{"name": "e.txt"}},
		"nil":       {"file": nil},
	} {
		t.Run(name, func(t *testing.T) {
			w := newReplayWorld(t, "trusted")
			out := w.serve(t, ReplayTrusted, input)
			if !out.StartRefused || out.Served || !strings.Contains(out.Diagnosis, "s0.command.7") {
				t.Fatalf("outcome = %+v, want a refusal naming the parameter", out)
			}
			if len(w.d.recorded()) != 0 {
				t.Fatalf("a replay with an unbound parameter dispatched %v", dispatchedKeys(w.d.recorded()))
			}
			fb := w.f.recorded()
			if len(fb) != 1 || !strings.Contains(fb[0].Guidance.Prompt, "did not start, so nothing has been done") {
				t.Fatalf("hand-backs = %+v, want one that says nothing ran", fb)
			}
			if w.lc.get("failures") != float64(1) {
				t.Errorf("failures = %v, want the refused start counted", w.lc.get("failures"))
			}
		})
	}
}

// TestATrustedProcedureWhoseFingerprintMismatchesFallsBackToTheAppAndRecordsTheMismatch
// is #5411's acceptance (D16): the target reports a tool version the
// recordings never ran. The app serves the goal -- handed it exactly once --
// the replay run's outcome NAMES the mismatch, and the ladder counts one more
// failure.
func TestATrustedProcedureWhoseFingerprintMismatchesFallsBackToTheAppAndRecordsTheMismatch(t *testing.T) {
	w := newReplayWorld(t, "trusted")
	w.p.observed.Tools["mkdir"] = "9.3"

	out := w.serve(t, ReplayTrusted, map[string]any{"file": goalFile})
	if !out.StartRefused || out.Served {
		t.Fatalf("outcome = %+v, want the start refused", out)
	}
	if n := len(w.f.recorded()); n != 1 {
		t.Fatalf("the app was handed the goal %d times, want once", n)
	}
	if len(w.d.recorded()) != 0 {
		t.Fatalf("a refused start dispatched %v", dispatchedKeys(w.d.recorded()))
	}
	const mismatch = "tools.mkdir: recorded 9.4, found 9.3"
	run := w.work.run(out.ReplayRunId)
	o, _ := run["outcome"].(map[string]any)
	if list, _ := o["mismatches"].([]any); len(list) != 1 || list[0] != mismatch {
		t.Fatalf("the replay run's outcome = %v, want it to name %q", o, mismatch)
	}
	if o["repairRunId"] != "v1:work:run:repair-1" {
		t.Errorf("the repaired run is not on the outcome: %v", o["repairRunId"])
	}
	if w.lc.get("failures") != float64(1) || w.lc.get("ladder") != "trusted" {
		t.Errorf("ladder = %v with %v failures, want trusted with one more failure", w.lc.get("ladder"), w.lc.get("failures"))
	}
}

// TestNoProberMeansALearnedPreconditionIsUnmeasured: an ABSENT measurement is
// never a match. A node that cannot probe does not start a procedure that
// learned anything about its environment.
func TestNoProberMeansALearnedPreconditionIsUnmeasured(t *testing.T) {
	w := newReplayWorld(t, "trusted")
	w.i.seams.mu.Lock()
	w.i.seams.prober = nil
	w.i.seams.mu.Unlock()
	out := w.serve(t, ReplayTrusted, map[string]any{"file": goalFile})
	if !out.StartRefused || len(out.Preconditions.Unmeasured) == 0 || !strings.Contains(out.Diagnosis, "no prober") || out.Code != codeNoProber {
		t.Fatalf("outcome = %+v, want the start refused as unmeasured", out)
	}
	// The NODE lacks a seam, which says nothing about the procedure.
	if w.lc.get("failures") != float64(0) {
		t.Fatalf("failures = %v: a node with no prober counted against the procedure", w.lc.get("failures"))
	}
}

// TestAStepWithAReceiptIsNeverDispatchedAgain (Review Focus 5): the goal's run
// died mid-replay and is resumed; its replay statement executes again and
// finds the replay run it already started, because that run's id is derived
// from the goal run and the statement. The step whose receipt exists is NOT
// dispatched again; the one left unfinished is, under the SAME idempotency
// key. And once the replay has finished, executing it again runs, counts and
// hands over nothing.
func TestAStepWithAReceiptIsNeverDispatchedAgain(t *testing.T) {
	w := newReplayWorld(t, "trusted")
	req := ReplayRequest{OwnerUserId: replayOwner, ConstructId: w.constructId, Mode: ReplayTrusted,
		GoalRunId: goalRunId, StepKey: "replayed", Input: map[string]any{"file": goalFile}}
	runId, derived := (&replay{mode: ReplayTrusted, req: req, c: &loaded{id: w.constructId, hash: w.hash}}).replayRunId()
	if !derived {
		t.Fatal("a replay serving a goal's statement must derive its run id")
	}
	bare := memql.BareShortId(runId)
	w.work.putRun(map[string]any{"id": runId, "ownerUserId": replayOwner, "status": "running", "triggeredBy": "procedure:trusted"})
	w.work.putStep(map[string]any{
		"id": "v1:work:step:" + bare + "-step0", "runId": runId, "key": "step0", "seq": float64(0),
		"ownerUserId": replayOwner, "status": "done", "idempotencyKey": work.IdempotencyKey(runId, "step0", 1),
		"result": map[string]any{"status": "done", "result": map[string]any{
			"observation": map[string]any{"isError": false, "exitCode": float64(0), "resultType": "string"},
			"tool":        "exec", "summary": "ran `mkdir -p out && echo hello > e.txt`", "sideEffect": true,
		}},
	})
	w.work.putStep(map[string]any{
		"id": "v1:work:step:" + bare + "-step1", "runId": runId, "key": "step1", "seq": float64(1),
		"ownerUserId": replayOwner, "status": "running", "idempotencyKey": work.IdempotencyKey(runId, "step1", 1),
	})

	out, err := w.i.Replay(context.Background(), req)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if out.ReplayRunId != runId || !out.Served {
		t.Fatalf("outcome = %+v, want the resumed run served", out)
	}
	calls := w.d.recorded()
	if !reflect.DeepEqual(dispatchedKeys(calls), []string{"step1"}) {
		t.Fatalf("dispatched %v: step0 has a receipt and must never run again", dispatchedKeys(calls))
	}
	if calls[0].IdempotencyKey != work.IdempotencyKey(runId, "step1", 1) {
		t.Errorf("the unfinished step was re-dispatched under %q, not its own key", calls[0].IdempotencyKey)
	}
	if len(out.Completed) != 2 || out.Completed[0].IdempotencyKey != work.IdempotencyKey(runId, "step0", 1) {
		t.Errorf("completed = %+v, want both steps with their keys", out.Completed)
	}

	// Finished: executing the statement again changes nothing at all.
	writes := len(w.eng.writes())
	again, err := w.i.Replay(context.Background(), req)
	if err != nil {
		t.Fatalf("Replay again: %v", err)
	}
	if !again.AlreadyDone || !again.Served || len(w.d.recorded()) != 1 || len(w.eng.writes()) != writes {
		t.Fatalf("a finished replay ran again: %+v, dispatches %d, writes %d -> %d",
			again, len(w.d.recorded()), writes, len(w.eng.writes()))
	}
}

// TestADivergenceHandsTheAppThePartialTraceAndNeverRedoesACompletedStep (D16,
// Review Focus 5): a machine-local procedure's command RAN on the person's
// machine -- the dispatcher says its effect was delivered -- and then the
// report's write came back with bytes no recording wrote. The replay STOPS
// there; the app is told, before it starts, which steps already ran with
// their idempotency keys, which of them it must never repeat, and where and
// why the replay stopped: repair, not resample. The procedure diverged
// although every precondition held, so they proved insufficient and it is
// demoted at once; and executing the statement again hands nothing over
// twice.
func TestADivergenceHandsTheAppThePartialTraceAndNeverRedoesACompletedStep(t *testing.T) {
	w := newReplayWorld(t, "trusted")
	w.withProcedure(t, func(p map[string]any) {
		p["footprint"] = map[string]any{"files": true, "machine": true}
		p["target"] = "machine"
	})
	machine := newFakeDispatcher()
	machine.delivers = true
	machine.alter["step1"] = func(r *DispatchResult) {
		r.Observation.Contents[0].Digest = "sha256:" + strings.Repeat("0", 64)
	}
	w.i.SetDispatcher(work.TargetMachine, machine)
	w.i.SetProber(machineProber())

	req := ReplayRequest{OwnerUserId: replayOwner, ConstructId: w.constructId, Mode: ReplayTrusted,
		GoalRunId: goalRunId, StepKey: "replayed", Input: map[string]any{"file": goalFile}}
	out, err := w.i.Replay(context.Background(), req)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if !out.Diverged || out.DivergedStep != 1 || out.Served || !out.Insufficient {
		t.Fatalf("outcome = %+v, want a divergence at step 1 with the preconditions held", out)
	}
	key0 := work.IdempotencyKey(out.ReplayRunId, "step0", 1)
	if len(out.Completed) != 1 || out.Completed[0].IdempotencyKey != key0 || !out.Completed[0].SideEffect {
		t.Fatalf("completed = %+v, want step 0 with its key and its delivered effect", out.Completed)
	}
	fb := w.f.recorded()
	if len(fb) != 1 {
		t.Fatalf("the app was handed the goal %d times, want once", len(fb))
	}
	g := fb[0].Guidance
	if !reflect.DeepEqual(g.Completed, out.Completed) || g.Procedure != w.name {
		t.Fatalf("guidance = %+v", g)
	}
	// Steps are numbered from 1 in everything a person or the app reads, as
	// MemQL OS lists them; the stored indices stay 0-based.
	if !containsAll(g.Prompt, testStatement, "A learned procedure already ran these steps -- do not repeat them:",
		"- step 1 (exec)", key0, "It stopped at step 2 because:", "Step 2 (fs_write", "out/report.txt") {
		t.Fatalf("the prompt does not hand over the partial trace:\n%s", g.Prompt)
	}
	if out.DivergedStep != 1 || out.Completed[0].Index != 0 {
		t.Fatalf("stored indices %d / %d, want the 0-based positions", out.DivergedStep, out.Completed[0].Index)
	}
	if fb[0].StepId != journalStepId(goalRunId, "replayed") || fb[0].RunId != goalRunId || fb[0].GoalId != goalId || fb[0].App != "claude-code" {
		t.Fatalf("hand-back = %+v", fb[0])
	}
	run := w.work.run(out.ReplayRunId)
	o, _ := run["outcome"].(map[string]any)
	if run["status"] != "failed" || run["errorCode"] != codeDiverged || o["repairRunId"] != "v1:work:run:repair-1" || o["servedBy"] != "app" {
		t.Fatalf("replay run = %v", run)
	}
	if w.lc.get("ladder") != "shadow" || !out.Transition.Demoted {
		t.Errorf("ladder = %v: one precondition that proved insufficient demotes a trusted procedure", w.lc.get("ladder"))
	}
	if keys := dispatchedKeys(machine.recorded()); !reflect.DeepEqual(keys, []string{"step0", "step1"}) {
		t.Fatalf("dispatched %v", keys)
	}

	again, err := w.i.Replay(context.Background(), req)
	if err != nil {
		t.Fatalf("Replay again: %v", err)
	}
	if !again.AlreadyDone || !again.FellBack || len(w.f.recorded()) != 1 || len(machine.recorded()) != 2 {
		t.Fatalf("executing the statement again redid work: %+v, hand-backs %d, dispatches %d",
			again, len(w.f.recorded()), len(machine.recorded()))
	}
}

// TestAWorkbenchStepIsNotOneTheAppMaySkip: on the workbench nothing a step did
// reached the app -- the replay runs in a workspace of its own -- so a
// divergence there lists the steps that ran as ones whose effects did not
// reach it, and tells the app to skip nothing it would then never do.
func TestAWorkbenchStepIsNotOneTheAppMaySkip(t *testing.T) {
	w := newReplayWorld(t, "trusted")
	w.d.alter["step1"] = func(r *DispatchResult) {
		r.Observation.Contents[0].Digest = "sha256:" + strings.Repeat("0", 64)
	}
	out := w.serve(t, ReplayTrusted, map[string]any{"file": goalFile})
	if !out.Diverged || len(out.Completed) != 1 || out.Completed[0].SideEffect {
		t.Fatalf("outcome = %+v, want step 0 completed and delivered nowhere", out)
	}
	prompt := w.f.recorded()[0].Guidance.Prompt
	if strings.Contains(prompt, "do not repeat them") || !containsAll(prompt, "in its own workspace, which you do not share", out.Completed[0].IdempotencyKey) {
		t.Fatalf("a workbench step was handed over as one to skip:\n%s", prompt)
	}
}

// TestARefusedStepIsAFailedReplayNotAnInsufficientPrecondition: a Go error from
// a dispatcher means the step did NOT run -- a gate, a scope, a spelling the
// executor does not support, the machine not there. After a step ran it is a
// divergence that delivered nothing: one failed replay -- two of which demote
// -- rather than the one insufficiency that demotes at once. Before any step
// ran it is the TARGET refusing, which says nothing about the procedure: a
// refused start the ladder does not count (review finding I1).
func TestARefusedStepIsAFailedReplayNotAnInsufficientPrecondition(t *testing.T) {
	w := newReplayWorld(t, "trusted")
	w.d.fail["step1"] = errors.New("denied_by_scope")
	out := w.serve(t, ReplayTrusted, map[string]any{"file": goalFile})
	if !out.Diverged || out.Insufficient || out.DivergedStep != 1 || !out.FellBack || len(out.Completed) != 1 {
		t.Fatalf("a step refused after another ran = %+v, want a divergence that delivered nothing", out)
	}
	if w.lc.get("ladder") != "trusted" || w.lc.get("failures") != float64(1) {
		t.Fatalf("ladder %v failures %v, want still trusted with one failure", w.lc.get("ladder"), w.lc.get("failures"))
	}

	w.d.fail = map[string]error{"step0": errors.New("command_not_allowed")}
	second := w.serve(t, ReplayTrusted, map[string]any{"file": goalFile})
	if !second.StartRefused || second.Diverged || second.Code != codeTargetUnavailable {
		t.Fatalf("a first step refused before it ran = %+v, want a refused start the ladder does not count", second)
	}
	if !strings.Contains(w.f.recorded()[1].Guidance.Prompt, "did not start, so nothing has been done") {
		t.Fatalf("the app was not told nothing ran:\n%s", w.f.recorded()[1].Guidance.Prompt)
	}
	if second.Transition.From != "" || w.lc.get("ladder") != "trusted" || w.lc.get("failures") != float64(1) {
		t.Fatalf("a target's refusal moved the ladder: %+v, ladder %v, failures %v", second.Transition, w.lc.get("ladder"), w.lc.get("failures"))
	}

	w.d.fail = map[string]error{"step1": errors.New("denied_by_scope")}
	third := w.serve(t, ReplayTrusted, map[string]any{"file": goalFile})
	if !third.Transition.Demoted || w.lc.get("ladder") != "shadow" {
		t.Fatalf("two failed replays did not demote: %+v, ladder %v", third.Transition, w.lc.get("ladder"))
	}
}

// TestADivergenceWithNoAppFallbackFailsTheReplayRunNamingWhy: nothing served
// the goal, and the outcome says so -- never a silent success.
func TestADivergenceWithNoAppFallbackFailsTheReplayRunNamingWhy(t *testing.T) {
	w := newReplayWorld(t, "trusted")
	w.i.seams.mu.Lock()
	w.i.seams.fallback = nil
	w.i.seams.mu.Unlock()
	w.d.fail["step1"] = errors.New("boom")
	out := w.serve(t, ReplayTrusted, map[string]any{"file": goalFile})
	if out.FellBack || out.Served || out.Code != codeFallbackUnavailable {
		t.Fatalf("outcome = %+v, want procedure_fallback_unavailable", out)
	}
	run := w.work.run(out.ReplayRunId)
	if run["errorCode"] != codeFallbackUnavailable || !strings.Contains(str(run, "errorMessage"), "boom") {
		t.Fatalf("replay run = %v, want it failed as unavailable with the reason", run)
	}
}

// TestACleanCanaryReplayClimbsOnItsOwnEvidence (D3): after the one approval,
// clean canary replays make a procedure trusted with nobody asked again.
func TestACleanCanaryReplayClimbsOnItsOwnEvidence(t *testing.T) {
	w := newReplayWorld(t, "canary")
	w.policy(work.LadderPolicy{ShadowMatches: 2, DistinctBindings: 2, CanaryMatches: 1, FailuresToDemote: 2, InsufficientToDemote: 1, RetireAfterDays: 30})
	out := w.serve(t, ReplayCanary, map[string]any{"file": goalFile})
	if !out.Served || out.Transition.From != work.RungCanary || out.Transition.To != work.RungTrusted {
		t.Fatalf("outcome = %+v, transition %+v", out, out.Transition)
	}
	if run := w.work.run(out.ReplayRunId); run["triggeredBy"] != "procedure:canary" {
		t.Errorf("a canary replay ran as %v", run["triggeredBy"])
	}
}

// TestAShadowMatchingMTimesAcrossKBindingsRaisesExactlyOnePromotion is #5410's
// acceptance through the runner (D3, D15): policy m=2, k=2. Two matches on
// two bindings raise ONE procedurePromotion approval -- its artifact hash the
// construct's version, its run the shadow replay that met the threshold --
// and its id rides the same ladder write, so a third match proposes nothing.
// The negative control: two matches on ONE binding propose nothing.
func TestAShadowMatchingMTimesAcrossKBindingsRaisesExactlyOnePromotion(t *testing.T) {
	policy := work.LadderPolicy{ShadowMatches: 2, DistinctBindings: 2, CanaryMatches: 5, FailuresToDemote: 2, InsufficientToDemote: 1, RetireAfterDays: 30}

	control := newReplayWorld(t, "shadow")
	control.policy(policy)
	control.shadowOf(t, "v1:work:run:rec-c", "c.txt")
	if out := control.shadowOf(t, "v1:work:run:rec-c2", "c.txt"); !out.Match || out.ApprovalId != "" {
		t.Fatalf("control: %+v -- two matches on one binding must not propose", out)
	}
	if n := len(control.eng.callsTo("createWorkApproval")); n != 0 {
		t.Fatalf("control raised %d approvals", n)
	}

	w := newReplayWorld(t, "shadow")
	w.policy(policy)
	first := w.shadowOf(t, "v1:work:run:rec-c", "c.txt")
	if !first.Match || first.ApprovalId != "" || w.lc.get("shadowMatches") != float64(1) {
		t.Fatalf("first comparison = %+v, shadowMatches %v", first, w.lc.get("shadowMatches"))
	}
	for _, c := range w.d.recorded() {
		if !c.Sandbox || c.Target != work.TargetWorkbench {
			t.Fatalf("a shadow step ran %+v, want the workbench sandbox", c)
		}
	}
	second := w.shadowOf(t, "v1:work:run:rec-d", "d.txt")
	if !second.Match || second.ApprovalId == "" || !second.Transition.Propose {
		t.Fatalf("second comparison = %+v, want the promotion proposed", second)
	}
	approvals := w.eng.callsTo("createWorkApproval")
	if len(approvals) != 1 {
		t.Fatalf("%d approvals raised, want exactly one", len(approvals))
	}
	a := parseCallArgs(t, approvals[0].Query)
	if a["kind"] != work.ApprovalKindProcedurePromotion || a["artifactHash"] != w.hash || a["runId"] != second.ReplayRunId ||
		a["approvalId"] != second.ApprovalId {
		t.Fatalf("approval = %v", a)
	}
	subject, _ := a["subject"].(map[string]any)
	if subject["constructId"] != w.constructId || subject["procedureHash"] != w.hash || subject["shadowMatches"] != float64(2) {
		t.Fatalf("subject = %v", subject)
	}
	if !approvals[0].Internal || approvals[0].Actor != replayOwner {
		t.Fatalf("the approval was raised internal=%v as %q, want the stamp and the owner", approvals[0].Internal, approvals[0].Actor)
	}
	if w.lc.get("promotionApprovalId") != second.ApprovalId {
		t.Fatalf("the construct points at %v, want the raised approval", w.lc.get("promotionApprovalId"))
	}

	third := w.shadowOf(t, "v1:work:run:rec-e", "e.txt")
	if !third.Match || third.ApprovalId != "" || len(w.eng.callsTo("createWorkApproval")) != 1 {
		t.Fatalf("a third match raised a second approval: %+v", third)
	}
}

// TestAShadowReplayIsNeverCountedTwice: the replay run of a comparison is
// derived from the recording and the construct, so a comparison that is run
// again for the same recording finds its run and counts nothing.
func TestAShadowReplayIsNeverCountedTwice(t *testing.T) {
	w := newReplayWorld(t, "shadow")
	w.shadowOf(t, "v1:work:run:rec-c", "c.txt")
	again := w.shadowOf(t, "v1:work:run:rec-c", "c.txt")
	if !again.AlreadyDone || w.lc.get("shadowMatches") != float64(1) || len(w.eng.callsTo("recordConstructLadder")) != 1 {
		t.Fatalf("the same recording was counted twice: %+v, shadowMatches %v", again, w.lc.get("shadowMatches"))
	}
}

// TestARecordingIsComparedOncePerConstructWhateverTheVersion: a recording is
// evidence about a procedure ONCE. Compared against one version, then asked
// about again after the procedure was re-lifted (procedureLearnFromRun called
// a second time, the proving driver comparing again), it finds the comparison
// it already made -- its replay run is derived from the recording and the
// construct, not the version -- answers what that one answered, and moves
// nothing: the recording must not count a second time, against a version it
// may itself have been learned into.
func TestARecordingIsComparedOncePerConstructWhateverTheVersion(t *testing.T) {
	w := newReplayWorld(t, "shadow")
	first := w.shadowOf(t, "v1:work:run:rec-c", "c.txt")
	if !first.Match || w.lc.get("shadowMatches") != float64(1) {
		t.Fatalf("first comparison = %+v, shadowMatches %v", first, w.lc.get("shadowMatches"))
	}
	// A re-lift: a new version on the entry rung, its streak empty.
	reliftTo(w, "shadow", "sha256:the-relifted-version")
	dispatches, ladderWrites := len(w.d.recorded()), len(w.eng.callsTo("recordConstructLadder"))

	again := w.shadowOf(t, "v1:work:run:rec-c", "c.txt")
	if !again.AlreadyDone || again.ReplayRunId != first.ReplayRunId || !again.Match {
		t.Fatalf("the same recording compared again = %+v, want the first comparison's answer", again)
	}
	if len(w.d.recorded()) != dispatches || len(w.eng.callsTo("recordConstructLadder")) != ladderWrites {
		t.Fatal("the same recording was replayed or counted a second time")
	}
	if w.lc.get("shadowMatches") != float64(0) {
		t.Fatalf("shadowMatches = %v: the new version's streak counted a recording already counted", w.lc.get("shadowMatches"))
	}
}

// TestAComparisonOfAReplacedVersionIsClosedNotResumed: a comparison started
// against one version and was interrupted; by the time it is asked for again
// the procedure has been re-lifted. Its receipts are the OLD version's steps,
// so resuming it would compare the new version's template against them. It is
// closed, saying a newer version replaced the one it ran, and counts nothing.
func TestAComparisonOfAReplacedVersionIsClosedNotResumed(t *testing.T) {
	w := newReplayWorld(t, "shadow")
	req := ReplayRequest{OwnerUserId: replayOwner, ConstructId: w.constructId, Mode: ReplayShadow, GoalRunId: "v1:work:run:rec-c"}
	runId, derived := (&replay{mode: ReplayShadow, req: req, c: &loaded{id: w.constructId, hash: w.hash}}).replayRunId()
	if !derived {
		t.Fatal("a shadow comparison of a recording must derive its run id")
	}
	w.work.putRun(map[string]any{
		"id": runId, "ownerUserId": replayOwner, "status": "running", "triggeredBy": "procedure:shadow",
		"input": map[string]any{"constructId": w.constructId, "procedureHash": "sha256:the-version-it-started-on", "mode": "shadow"},
	})
	out := w.shadowOf(t, "v1:work:run:rec-c", "c.txt")
	if !out.NotCompared || out.Match || !out.VersionReplaced {
		t.Fatalf("outcome = %+v, want the stale comparison closed as not compared", out)
	}
	if len(w.d.recorded()) != 0 || len(w.eng.callsTo("recordConstructLadder")) != 0 {
		t.Fatalf("a comparison of a replaced version dispatched %d step(s) or moved the ladder", len(w.d.recorded()))
	}
	run := w.work.run(runId)
	if o, _ := run["outcome"].(map[string]any); run["status"] != "failed" || o["versionReplaced"] != true {
		t.Fatalf("the stale comparison's run = %v, want it closed saying the version was replaced", run)
	}
}

// TestAMachineLocalProcedureIsComparedDryInShadow: a shadow never touches a
// person's machine. A machine-local procedure's comparison dispatches nothing
// and holds the calls it would make to the app's own.
func TestAMachineLocalProcedureIsComparedDryInShadow(t *testing.T) {
	w := newReplayWorld(t, "shadow")
	w.withProcedure(t, func(p map[string]any) {
		p["footprint"] = map[string]any{"files": true, "machine": true}
		p["target"] = "machine"
	})
	out := w.shadowOf(t, "v1:work:run:rec-c", "c.txt")
	if !out.Match || len(w.d.recorded()) != 0 {
		t.Fatalf("outcome = %+v with %d dispatches, want a dry match", out, len(w.d.recorded()))
	}
	// And a call that is not the app's is a mismatch, dry as well.
	w2 := newReplayWorld(t, "shadow")
	w2.withProcedure(t, func(p map[string]any) {
		p["footprint"] = map[string]any{"files": true, "machine": true}
		p["target"] = "machine"
	})
	out2, err := w2.i.Replay(context.Background(), ReplayRequest{
		OwnerUserId: replayOwner, ConstructId: w2.constructId, Mode: ReplayShadow, GoalRunId: "v1:work:run:rec-x",
		Bindings: map[string]string{"s0.command.7": "c.txt"}, AppActions: appObservations(),
		AppArgs: []map[string]any{{"command": "mkdir -p out && echo hello > other.txt"}, {"file_path": relativeReportPath, "content": "hello\n"}},
	})
	if err != nil || out2.Match || out2.DivergedStep != 0 {
		t.Fatalf("a dry comparison accepted a call the app never made: %+v, %v", out2, err)
	}
}

// TestACallThatDoesNotReadBackAsItsStepIsRefusedBeforeDispatch (D16's
// content-addressed input check): the call about to be dispatched must read
// back as its template step, bound to exactly the values the replay bound.
// Here the report's file name is a parameter inside a PATH. A goal input
// carrying a separator -- "sub/report.txt" would write one directory deeper
// than any recording did, a different call wearing the right values -- is
// refused as it is written out, since a path-segment parameter takes one
// segment (component/procedure.Materialize); and a value that is written out
// but does not read back as itself -- a number parameter given "1.0", which
// goes out as 1 -- is refused by the input check. Either way the write never
// reaches the dispatcher; the command before it ran, and is what the app is
// told about. The control: a plain name serves.
func TestACallThatDoesNotReadBackAsItsStepIsRefusedBeforeDispatch(t *testing.T) {
	withNameHole := func(w *replayWorld) {
		w.withDecoded(t, func(p *Procedure) {
			fp, ok := p.Steps[1].Args.At([]string{"file_path"})
			if !ok || len(fp.Kids) != 3 {
				t.Fatalf("the fixture's write path is %+v, want ./out/<name>", fp)
			}
			fp.Kids[2] = proc.HoleNode("s1.file_path.2", "string")
			p.Holes = append(p.Holes, proc.Hole{Id: "s1.file_path.2", StepIndex: 1, Path: []string{"file_path", "2"}, Type: "string", Class: proc.HoleFree})
			p.FreeParameters = append(p.FreeParameters, "s1.file_path.2")
			p.InputMap["s1.file_path.2"] = "name"
		})
	}
	withCountHole := func(w *replayWorld) {
		w.withDecoded(t, func(p *Procedure) {
			args := p.Steps[1].Args
			for n, k := range args.Keys {
				if k == "content" {
					args.Kids[n] = proc.HoleNode("s1.content", "number")
				}
			}
			p.Holes = append(p.Holes, proc.Hole{Id: "s1.content", StepIndex: 1, Path: []string{"content"}, Type: "number", Class: proc.HoleFree})
			p.FreeParameters = append(p.FreeParameters, "s1.content")
			p.InputMap["s1.content"] = "count"
		})
	}

	for _, c := range []struct {
		name  string
		shape func(*replayWorld)
		input map[string]any
		says  []string
	}{
		{"a separator in a path segment", withNameHole, map[string]any{"file": goalFile, "name": "sub/report.txt"},
			[]string{"could not be written out", "s1.file_path.2", "not one segment"}},
		{"a value that does not read back as itself", withCountHole, map[string]any{"file": goalFile, "count": "1.0"},
			[]string{"would not have made the call", "s1.content"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newReplayWorld(t, "trusted")
			c.shape(w)
			out := w.serve(t, ReplayTrusted, c.input)
			if !out.Diverged || out.DivergedStep != 1 || out.Insufficient || !containsAll(out.Diagnosis, c.says...) {
				t.Fatalf("outcome = %+v, want the write refused before it ran, as a plain failure saying %q", out, c.says)
			}
			if keys := dispatchedKeys(w.d.recorded()); !reflect.DeepEqual(keys, []string{"step0"}) {
				t.Fatalf("dispatched %v: the write must never reach the dispatcher", keys)
			}
			if len(out.Completed) != 1 || out.Completed[0].Index != 0 {
				t.Fatalf("completed = %+v, want the command that ran", out.Completed)
			}
		})
	}

	control := newReplayWorld(t, "trusted")
	withNameHole(control)
	if ok := control.serve(t, ReplayTrusted, map[string]any{"file": goalFile, "name": "report.txt"}); !ok.Served {
		t.Fatalf("the control did not serve, so the refusal above proves nothing: %+v", ok)
	}
}

// TestAStoredPreconditionThisReplicaCannotCheckRefusesTheReplay (E5): the
// stored initiation set is read through DecodePreconditions, and a predicate
// a newer writer learned -- one this replica has no check for -- refuses the
// replay rather than being skipped. Skipped, the replay would start on
// evidence the recordings never gave; refused, nothing is dispatched.
func TestAStoredPreconditionThisReplicaCannotCheckRefusesTheReplay(t *testing.T) {
	w := newReplayWorld(t, "trusted")
	prec, _ := w.lc.get("preconditions").(map[string]any)
	prec = copyRow(t, prec)
	prec["kernel"] = map[string]any{"min": "6.1"}
	w.lc.set("preconditions", prec)

	_, err := w.i.Replay(context.Background(), ReplayRequest{
		OwnerUserId: replayOwner, ConstructId: w.constructId, Mode: ReplayTrusted, GoalRunId: goalRunId,
		Input: map[string]any{"file": goalFile},
	})
	if err == nil || !strings.Contains(err.Error(), "kernel") {
		t.Fatalf("Replay = %v, want a refusal naming the predicate it cannot check", err)
	}
	if len(w.d.recorded()) != 0 {
		t.Fatalf("a replay with an uncheckable precondition dispatched %v", dispatchedKeys(w.d.recorded()))
	}
	if _, err := DecodePreconditions(map[string]any{"tools": map[string]any{"mkdir": "9.4"}}); err != nil {
		t.Fatalf("the control: a known predicate must still decode: %v", err)
	}
}

// TestAGoalValueNoRecordingWasShapedLikeRefusesTheStart (B5): the fixture's
// parameter is a file name every recording spelled plainly. A goal that gives
// it an option, an absolute path, a home directory or a parent directory
// asks for a call no recording made: the start is refused naming the
// parameter and the value, nothing is dispatched, the goal goes to the app
// once, and the ladder counts it -- the procedure was chosen for this goal and
// could not serve it.
func TestAGoalValueNoRecordingWasShapedLikeRefusesTheStart(t *testing.T) {
	for _, v := range []string{"-rf", "/etc/passwd", "~/x", ".."} {
		t.Run(v, func(t *testing.T) {
			w := newReplayWorld(t, "trusted")
			out := w.serve(t, ReplayTrusted, map[string]any{"file": v})
			if !out.StartRefused || out.Served || out.Code != codeStartRefused || !containsAll(out.Diagnosis, "s0.command.7", strconv.Quote(v)) {
				t.Fatalf("outcome = %+v, want a refused start naming the parameter and the value", out)
			}
			if len(w.d.recorded()) != 0 {
				t.Fatalf("a refused value dispatched %v", dispatchedKeys(w.d.recorded()))
			}
			if len(w.f.recorded()) != 1 {
				t.Fatalf("the app was handed the goal %d time(s), want once", len(w.f.recorded()))
			}
			if w.lc.get("failures") != float64(1) {
				t.Errorf("failures = %v, want the refused start counted", w.lc.get("failures"))
			}
		})
	}
}

// TestAGoalValueThatIsShellTextReplaysAsOneArgument (B5): a value that holds
// shell code, an operator or a glob is not refused -- nothing about it is an
// option or a path no recording showed -- and it reaches the command as
// exactly one argument, never as code: the dispatched command quotes it.
func TestAGoalValueThatIsShellTextReplaysAsOneArgument(t *testing.T) {
	for v, quoted := range map[string]string{
		"x;rm${IFS}-rf${IFS}$HOME": `'x;rm${IFS}-rf${IFS}$HOME'`,
		"$(id)":                    `'$(id)'`,
		"`id`":                     "'`id`'",
		"R&D.txt":                  `'R&D.txt'`,
		"*.txt":                    `'*.txt'`,
		"a|b":                      `'a|b'`,
	} {
		t.Run(v, func(t *testing.T) {
			w := newReplayWorld(t, "trusted")
			out := w.serve(t, ReplayTrusted, map[string]any{"file": v})
			if !out.Served {
				t.Fatalf("outcome = %+v, want served", out)
			}
			calls := w.d.recorded()
			if got, want := calls[0].Args["command"], "mkdir -p out && echo hello > "+quoted; got != want {
				t.Fatalf("dispatched %q, want %q", got, want)
			}
		})
	}
}

// TestAShadowRecordingWithAValueNoRecordingWasShapedLikeIsAMismatch (B5): in
// shadow the app's own action bound the parameter, and a value shaped like
// nothing the procedure was learned from makes the recording something other
// than an instance of it -- a MISMATCH, recorded like an unfit recording's,
// with nothing replayed and the streak started again.
func TestAShadowRecordingWithAValueNoRecordingWasShapedLikeIsAMismatch(t *testing.T) {
	w := newReplayWorld(t, "shadow")
	w.lc.set("shadowMatches", float64(1))
	out := w.shadowOf(t, "v1:work:run:rec-x", "-rf")
	if out.Match || !out.Diverged || out.StartRefused || out.Code != codeMismatch || !containsAll(out.Diagnosis, "s0.command.7", `"-rf"`) {
		t.Fatalf("outcome = %+v, want a mismatch naming the parameter and the value", out)
	}
	if out.DivergedStep != 0 {
		t.Errorf("DivergedStep = %d, want the parameter's step", out.DivergedStep)
	}
	if len(w.d.recorded()) != 0 {
		t.Fatalf("a mismatched recording dispatched %v", dispatchedKeys(w.d.recorded()))
	}
	if w.lc.get("shadowMatches") != float64(0) {
		t.Fatalf("shadowMatches = %v: a mismatch starts the streak again", w.lc.get("shadowMatches"))
	}
	if run := w.work.run(out.ReplayRunId); run == nil || run["errorCode"] != codeMismatch {
		t.Fatalf("the mismatch was not recorded: %v", run)
	}
}
