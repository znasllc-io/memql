package procedure

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/work"
)

var errBoom = errors.New("the app could not take the goal over")

// unavailable_test.go -- a target that could not run the procedure is not the
// procedure failing (review finding I1, epic memql#5408).
//
// The default policy demotes a trusted procedure after two failed replays and
// after ONE whose preconditions proved insufficient. So a laptop asleep
// through two goals, a workbench rollout, or one dropped stream would demote
// a procedure that did nothing wrong -- unless the ladder counts only what
// says something about the procedure. Each case here is one a node or a
// machine caused; the goal still goes to the app, the replay run says why
// under a code of its own, and the ladder does not move.

// TestAProbeThatCouldNotRunIsNotCounted: the prober could not reach the
// target (the machine is asleep, no workbench peer answered). Nothing was
// measured, so nothing about the procedure's preconditions is known.
func TestAProbeThatCouldNotRunIsNotCounted(t *testing.T) {
	w := newReplayWorld(t, "trusted")
	w.p.err = errors.New("no_workbench_peer: no workbench answered")
	out := w.serve(t, ReplayTrusted, map[string]any{"file": goalFile})
	if !out.StartRefused || out.Code != codeTargetUnavailable || !strings.Contains(out.Diagnosis, "no_workbench_peer") {
		t.Fatalf("outcome = %+v, want a refused start naming the unavailable target", out)
	}
	if len(w.f.recorded()) != 1 || len(w.d.recorded()) != 0 {
		t.Fatalf("hand-backs %d, dispatches %d: the goal goes to the app, and nothing runs", len(w.f.recorded()), len(w.d.recorded()))
	}
	if w.lc.get("failures") != float64(0) || w.lc.get("ladder") != "trusted" {
		t.Fatalf("failures %v, rung %v: a target that could not be probed is not the procedure failing", w.lc.get("failures"), w.lc.get("ladder"))
	}
	if run := w.work.run(out.ReplayRunId); run["errorCode"] != codeTargetUnavailable {
		t.Fatalf("the replay run = %v, want it to say the target was unavailable", run)
	}
}

// TestAFirstStepTheTargetRefusedIsNotCounted: a Go error from the dispatcher
// before any step completed -- the machine offline, the stream not there --
// is the target, not the procedure. Two in a row leave a trusted procedure
// trusted.
func TestAFirstStepTheTargetRefusedIsNotCounted(t *testing.T) {
	w := newReplayWorld(t, "trusted")
	w.d.fail["step0"] = errors.New("worker_unreachable: the machine is asleep")
	for n := 0; n < 2; n++ {
		out := w.serve(t, ReplayTrusted, map[string]any{"file": goalFile})
		if !out.StartRefused || out.Code != codeTargetUnavailable {
			t.Fatalf("replay %d = %+v, want a refused start as target_unavailable", n, out)
		}
	}
	if len(w.f.recorded()) != 2 {
		t.Fatalf("the app was handed %d goals, want both", len(w.f.recorded()))
	}
	if w.lc.get("failures") != float64(0) || w.lc.get("ladder") != "trusted" {
		t.Fatalf("failures %v, rung %v: two goals on an unreachable machine demoted the procedure", w.lc.get("failures"), w.lc.get("ladder"))
	}
}

// TestAStepTheTargetCouldNotFinishIsNotCountedAndMayHaveRun: the stream
// dropped while the second step was in flight. The dispatcher says so
// (Unavailable), which says nothing about the procedure -- and whether the
// step ran is unknown, so the app is told it MAY HAVE, rather than that it
// did not.
func TestAStepTheTargetCouldNotFinishIsNotCountedAndMayHaveRun(t *testing.T) {
	w := newReplayWorld(t, "trusted")
	w.d.alter["step1"] = func(r *DispatchResult) {
		yes := true
		*r = DispatchResult{Observation: work.StepObservation{IsError: &yes}, Unavailable: true,
			Output: map[string]any{"errorCode": "worker_disconnected"}}
	}
	out := w.serve(t, ReplayTrusted, map[string]any{"file": goalFile})
	if out.Served || !out.TargetUnavailable || out.Code != codeTargetUnavailable || out.Insufficient {
		t.Fatalf("outcome = %+v, want the replay stopped as target_unavailable", out)
	}
	if w.lc.get("failures") != float64(0) || w.lc.get("insufficient") != float64(0) || w.lc.get("ladder") != "trusted" {
		t.Fatalf("failures %v, insufficient %v, rung %v: a dropped stream was counted against the procedure",
			w.lc.get("failures"), w.lc.get("insufficient"), w.lc.get("ladder"))
	}
	fb := w.f.recorded()
	if len(fb) != 1 {
		t.Fatalf("the app was handed the goal %d times, want once", len(fb))
	}
	g := fb[0].Guidance
	if len(g.Completed) != 2 || g.Completed[1].Index != 1 || !g.Completed[1].MayHaveRun || g.Completed[0].MayHaveRun {
		t.Fatalf("guidance steps = %+v, want step 1 done and step 2 as one that MAY HAVE RUN", g.Completed)
	}
	if !containsAll(g.Prompt, "may have run", "- step 2 (fs_write)", "It stopped at step 2") {
		t.Fatalf("the prompt does not say the step may have run:\n%s", g.Prompt)
	}
	if run := w.work.run(out.ReplayRunId); run["status"] != "failed" || run["errorCode"] != codeTargetUnavailable {
		t.Fatalf("the replay run = %v", run)
	}
}

// TestATimedOutStepIsAnOrdinaryFailureNeverInsufficient: a command that ran
// out of time reported no exit code -- it did not finish, so what it would
// have answered is unknown. That is a failed replay (two demote), never the
// preconditions proving insufficient (one demotes): only a step that RAN TO
// AN ANSWER unlike the recordings' says that.
func TestATimedOutStepIsAnOrdinaryFailureNeverInsufficient(t *testing.T) {
	w := newReplayWorld(t, "trusted")
	w.d.alter["step0"] = func(r *DispatchResult) {
		yes := true
		*r = DispatchResult{Observation: work.StepObservation{IsError: &yes}, Output: map[string]any{"errorCode": "timeout"}}
	}
	out := w.serve(t, ReplayTrusted, map[string]any{"file": goalFile})
	if !out.Diverged || out.Insufficient || out.TargetUnavailable {
		t.Fatalf("outcome = %+v, want an ordinary divergence", out)
	}
	if w.lc.get("failures") != float64(1) || w.lc.get("insufficient") != float64(0) || w.lc.get("ladder") != "trusted" {
		t.Fatalf("failures %v, insufficient %v, rung %v: want one ordinary failure", w.lc.get("failures"), w.lc.get("insufficient"), w.lc.get("ladder"))
	}

	// The control: the command ran to an exit code no recording saw. That IS
	// the preconditions proving insufficient, and one demotes.
	control := newReplayWorld(t, "trusted")
	control.d.alter["step0"] = func(r *DispatchResult) {
		yes, one := true, 1
		r.Observation = work.StepObservation{IsError: &yes, ExitCode: &one, ResultType: "string"}
	}
	if c := control.serve(t, ReplayTrusted, map[string]any{"file": goalFile}); !c.Insufficient || control.lc.get("ladder") != "shadow" {
		t.Fatalf("the control = %+v, rung %v -- a completed step that differed must still count as insufficient", c, control.lc.get("ladder"))
	}
}

// TestAShadowWhoseTargetCouldNotFinishAStepIsNotCompared: in shadow the same
// stream drop leaves the streak exactly where it was.
func TestAShadowWhoseTargetCouldNotFinishAStepIsNotCompared(t *testing.T) {
	w := newReplayWorld(t, "shadow")
	w.lc.set("shadowMatches", float64(2))
	w.d.alter["step1"] = func(r *DispatchResult) {
		yes := true
		*r = DispatchResult{Observation: work.StepObservation{IsError: &yes}, Unavailable: true}
	}
	out := w.shadowOf(t, "v1:work:run:rec-c", "c.txt")
	if !out.NotCompared || out.Match || out.Code != codeTargetUnavailable {
		t.Fatalf("outcome = %+v, want the comparison not made", out)
	}
	if w.lc.get("shadowMatches") != float64(2) || len(w.eng.callsTo("recordConstructLadder")) != 0 {
		t.Fatalf("shadowMatches %v: a dropped stream reset the streak", w.lc.get("shadowMatches"))
	}
}

// TestAStoppedReplayThatIsExecutedAgainSaysWhereItStopped: the goal's run is
// resumed after the replay stopped on an unavailable target and the app's
// hand-back failed, so the statement runs again and hands the goal over from
// the run's row. It must name the step the replay stopped at -- the stored
// stoppedAt -- not a position counted from a list that includes the step that
// MAY HAVE RUN.
func TestAStoppedReplayThatIsExecutedAgainSaysWhereItStopped(t *testing.T) {
	w := newReplayWorld(t, "trusted")
	w.f.err = errBoom
	w.d.alter["step1"] = func(r *DispatchResult) {
		yes := true
		*r = DispatchResult{Observation: work.StepObservation{IsError: &yes}, Unavailable: true}
	}
	req := servedReq(w)
	if first, err := w.i.Replay(context.Background(), req); err != nil || first.FellBack {
		t.Fatalf("first = %+v, %v; want the hand-back to fail", first, err)
	}
	w.f.err = nil
	again, err := w.i.Replay(context.Background(), req)
	if err != nil || !again.AlreadyDone || !again.FellBack || again.DivergedStep != 1 {
		t.Fatalf("again = %+v, %v; want the goal handed over once, stopped at step index 1", again, err)
	}
	if prompt := w.f.recorded()[1].Guidance.Prompt; !strings.Contains(prompt, "It stopped at step 2 because") {
		t.Fatalf("the hand-back names the wrong step:\n%s", prompt)
	}
}
