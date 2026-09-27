package procedure

import (
	"context"
	"reflect"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
)

// resume_intent_test.go -- a step that was in flight when its node died is
// not sent again when it could have reached the world (review finding I5,
// epic memql#5408).
//
// A replay writes a step's intent, dispatches it, and writes its receipt. A
// node that dies between the dispatch and the receipt leaves an intent and no
// receipt, and a resume used to dispatch that step again under the same
// idempotency key -- but no production dispatcher deduplicates on the key, so
// a command on somebody's machine, or a push from the workbench, ran twice.
// Now such a step is not re-sent: the replay stops there and hands the goal
// back, naming the step as one that MAY HAVE RUN. A step that could only have
// touched the replay's own workbench workspace is re-sent as before.

// machineReplayWorld is a trusted, machine-local procedure with a machine
// dispatcher that says its effects reach the world.
func machineReplayWorld(t *testing.T) (*replayWorld, *fakeDispatcher) {
	t.Helper()
	w := newReplayWorld(t, "trusted")
	w.withProcedure(t, func(p map[string]any) {
		p["footprint"] = map[string]any{"files": true, "machine": true}
		p["target"] = "machine"
	})
	machine := newFakeDispatcher()
	machine.delivers = true
	w.i.SetDispatcher(work.TargetMachine, machine)
	w.i.SetProber(machineProber())
	return w, machine
}

// interruptedAfterStep0 writes a replay run that ran step 0 and was killed
// with step 1 in flight: step 0's receipt, step 1's intent and no receipt.
func interruptedAfterStep0(t *testing.T, w *replayWorld, req ReplayRequest, status string) string {
	t.Helper()
	runId, _ := (&replay{mode: req.Mode, req: req, c: &loaded{id: w.constructId, hash: w.hash}}).replayRunId()
	bare := memql.BareShortId(runId)
	w.work.putRun(map[string]any{"id": runId, "ownerUserId": replayOwner, "status": status, "triggeredBy": "procedure:trusted",
		"input": map[string]any{"constructId": w.constructId, "procedureHash": w.hash, "mode": "trusted"}})
	w.work.putStep(map[string]any{
		"id": "v1:work:step:" + bare + "-step0", "runId": runId, "key": "step0", "seq": float64(0),
		"ownerUserId": replayOwner, "status": "done", "idempotencyKey": work.IdempotencyKey(runId, "step0", 1),
		"result": map[string]any{"status": "done", "result": map[string]any{
			"observation": map[string]any{"isError": false, "exitCode": float64(0), "resultType": "string"},
			"tool":        "exec", "summary": "ran `mkdir -p out && echo hello > e.txt`", "sideEffect": true,
			"output": map[string]any{"stdout": "", "exitCode": float64(0)},
		}},
	})
	w.work.putStep(map[string]any{
		"id": "v1:work:step:" + bare + "-step1", "runId": runId, "key": "step1", "seq": float64(1),
		"ownerUserId": replayOwner, "status": "running", "idempotencyKey": work.IdempotencyKey(runId, "step1", 1),
		"call": map[string]any{"construct": "procedure", "name": w.name, "tool": "fs_write"},
	})
	return runId
}

func servedReq(w *replayWorld) ReplayRequest {
	return ReplayRequest{OwnerUserId: replayOwner, ConstructId: w.constructId, Mode: ReplayTrusted,
		GoalRunId: goalRunId, StepKey: "replayed", Input: map[string]any{"file": goalFile}}
}

// TestAStepInFlightOnAMachineIsNotSentAgain: the write to somebody's machine
// may have landed before the node died. The resume sends nothing, counts
// nothing, and tells the app that step may have run.
func TestAStepInFlightOnAMachineIsNotSentAgain(t *testing.T) {
	w, machine := machineReplayWorld(t)
	req := servedReq(w)
	runId := interruptedAfterStep0(t, w, req, "running")

	out, err := w.i.Replay(context.Background(), req)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(machine.recorded()) != 0 {
		t.Fatalf("dispatched %v: a step that may have landed on the machine was sent again", dispatchedKeys(machine.recorded()))
	}
	if out.Served || !out.Interrupted || out.Code != codeInterrupted || !out.FellBack {
		t.Fatalf("outcome = %+v, want the replay stopped as interrupted and the goal handed back", out)
	}
	fb := w.f.recorded()
	if len(fb) != 1 {
		t.Fatalf("the app was handed the goal %d times, want once", len(fb))
	}
	g := fb[0].Guidance
	if len(g.Completed) != 2 || !g.Completed[0].SideEffect || g.Completed[0].MayHaveRun || !g.Completed[1].MayHaveRun ||
		g.Completed[1].IdempotencyKey != work.IdempotencyKey(runId, "step1", 1) {
		t.Fatalf("guidance steps = %+v, want step 1 done and step 2 as one that MAY HAVE RUN", g.Completed)
	}
	if !containsAll(g.Prompt, "may have run", "- step 2 (fs_write): wrote ./out/report.txt", "do not repeat them") {
		t.Fatalf("the prompt does not name the step that may have run:\n%s", g.Prompt)
	}
	if w.lc.get("failures") != float64(0) || w.lc.get("ladder") != "trusted" {
		t.Fatalf("failures %v, rung %v: a node dying is not the procedure failing", w.lc.get("failures"), w.lc.get("ladder"))
	}
	if run := w.work.run(runId); run["status"] != "failed" || run["errorCode"] != codeInterrupted {
		t.Fatalf("the replay run = %v", run)
	}
}

// TestAWorkbenchStepThatSendsIsNotSentAgain: on the workbench a step touches
// only the replay's own workspace -- unless the command itself sends
// something out of it. A push in flight when the node died may have reached
// the remote.
func TestAWorkbenchStepThatSendsIsNotSentAgain(t *testing.T) {
	w := newReplayWorld(t, "trusted")
	withCommand(t, w, 1, "git push origin main")
	req := servedReq(w)
	runId := interruptedAfterStep0(t, w, req, "running")
	w.work.putStep(map[string]any{
		"id": "v1:work:step:" + memql.BareShortId(runId) + "-step1", "runId": runId, "key": "step1", "seq": float64(1),
		"ownerUserId": replayOwner, "status": "running", "idempotencyKey": work.IdempotencyKey(runId, "step1", 1),
		"call": map[string]any{"construct": "procedure", "name": w.name, "tool": "exec"},
	})
	out, err := w.i.Replay(context.Background(), req)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(w.d.recorded()) != 0 || !out.Interrupted {
		t.Fatalf("outcome = %+v, dispatched %v: a push in flight was sent again", out, dispatchedKeys(w.d.recorded()))
	}
}

// TestAWorkbenchOnlyStepInFlightIsSentAgain: the control. A write in the
// workbench's own workspace reached nothing the app shares, so the resume
// re-sends it under the same idempotency key, as it always did.
func TestAWorkbenchOnlyStepInFlightIsSentAgain(t *testing.T) {
	w := newReplayWorld(t, "trusted")
	req := servedReq(w)
	runId := interruptedAfterStep0(t, w, req, "running")
	out, err := w.i.Replay(context.Background(), req)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if !out.Served || out.Interrupted || !reflect.DeepEqual(dispatchedKeys(w.d.recorded()), []string{"step1"}) {
		t.Fatalf("outcome = %+v, dispatched %v: want the workbench write re-sent and the goal served", out, dispatchedKeys(w.d.recorded()))
	}
	if got := w.d.recorded()[0].IdempotencyKey; got != work.IdempotencyKey(runId, "step1", 1) {
		t.Fatalf("re-sent under %q, not the step's own key", got)
	}
}

// TestAnInterruptedHandBackNamesTheStepInFlight: the abandoned sweep closed
// the run before any resume -- it has no outcome, only rows. The hand-back
// lists what the receipts say ran AND the step that was in flight, as one
// that may have run; it used to list only the receipts.
func TestAnInterruptedHandBackNamesTheStepInFlight(t *testing.T) {
	w, machine := machineReplayWorld(t)
	req := servedReq(w)
	interruptedAfterStep0(t, w, req, "abandoned")
	out, err := w.i.Replay(context.Background(), req)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(machine.recorded()) != 0 || !out.FellBack {
		t.Fatalf("outcome = %+v with %d dispatches, want a hand-back and nothing sent", out, len(machine.recorded()))
	}
	g := w.f.recorded()[0].Guidance
	if len(g.Completed) != 2 || !g.Completed[1].MayHaveRun || g.Completed[1].Index != 1 {
		t.Fatalf("guidance steps = %+v, want the step in flight named as one that may have run", g.Completed)
	}
	if !containsAll(g.Prompt, "may have run", "- step 2 (fs_write)", "It stopped at step 2") {
		t.Fatalf("the prompt does not name the step in flight, or where the replay stopped:\n%s", g.Prompt)
	}
	if out.DivergedStep != 1 {
		t.Fatalf("stopped at index %d, want the step in flight (1): a step that may have run did not finish", out.DivergedStep)
	}
}

// TestAResumeTheRungNoLongerServesNamesWhereItStopped: resumed after the
// procedure was demoted, a replay goes no further and hands the goal back
// with what ran and what was in flight -- stopped at the step in flight.
func TestAResumeTheRungNoLongerServesNamesWhereItStopped(t *testing.T) {
	w, machine := machineReplayWorld(t)
	req := servedReq(w)
	interruptedAfterStep0(t, w, req, "running")
	w.lc.set("ladder", "shadow")
	out, err := w.i.Replay(context.Background(), req)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(machine.recorded()) != 0 || !out.FellBack || out.DivergedStep != 1 {
		t.Fatalf("outcome = %+v with %d dispatches, want a hand-back stopped at the step in flight", out, len(machine.recorded()))
	}
	g := w.f.recorded()[0].Guidance
	if len(g.Completed) != 2 || !g.Completed[1].MayHaveRun || !containsAll(g.Prompt, "It stopped at step 2") {
		t.Fatalf("guidance = %+v\n%s", g.Completed, g.Prompt)
	}
}
