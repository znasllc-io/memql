package procedure

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/common"
)

// serve_test.go -- a goal served by a learned procedure through the
// procedureReplay builtin (epic memql#5408, plan Task 5 step 5), and the
// procedureStep builtin that refuses, always.

// inGoalRun is the context the replayLearnedProcedure statement executes in:
// the goal's run on the context, the run's owner as the actor.
func inGoalRun(actor string) context.Context {
	ctx := auth.ContextWithUserActor(context.Background(), actor)
	return common.ContextWithRun(ctx, common.RunContext{
		RunId: goalRunId, GoalId: goalId, StepKey: "replayed", Mode: common.RunModeLive, OwnerUserId: replayOwner,
	})
}

func decodeServe(t *testing.T, w *replayWorld, ctx context.Context) (map[string]any, error) {
	t.Helper()
	nodes, err := w.i.handleProcedureReplay(ctx, map[string]any{"constructId": w.constructId}, 0)
	if err != nil {
		return nil, err
	}
	var reply map[string]any
	if err := json.Unmarshal(nodes[0].Payload, &reply); err != nil {
		t.Fatal(err)
	}
	return reply, nil
}

// TestProcedureStepAlwaysRefuses: the statement every rendered step of a
// learned procedure's SOURCE is. The source is the artifact a person reads and
// approves; a procedure is served through the ladder, never run as an
// automation -- inside a run or outside one.
func TestProcedureStepAlwaysRefuses(t *testing.T) {
	i := newTestIntegration(newFakeEngine())
	for _, ctx := range []context.Context{context.Background(), inGoalRun(replayOwner)} {
		_, err := i.handleProcedureStep(ctx, map[string]any{"step": 0, "tool": "exec", "args": map[string]any{"command": "rm -rf ."}}, 0)
		if err == nil || !strings.Contains(err.Error(), "procedureStep runs only inside replayLearnedProcedure") {
			t.Fatalf("procedureStep answered %v, want the one refusal", err)
		}
	}
}

// TestProcedureReplayServesTheGoalFromTheCurrentRung: the statement reads the
// goal's run, binds the goal's own input, re-decides the rung, and serves --
// answering that the procedure served it.
func TestProcedureReplayServesTheGoalFromTheCurrentRung(t *testing.T) {
	w := newReplayWorld(t, "trusted")
	reply, err := decodeServe(t, w, inGoalRun(replayOwner))
	if err != nil {
		t.Fatalf("procedureReplay: %v", err)
	}
	if reply["servedBy"] != "procedure" || reply["rung"] != "trusted" || reply["diverged"] != false {
		t.Fatalf("reply = %v", reply)
	}
	if got := w.d.recorded()[0].Args["command"]; got != "mkdir -p out && echo hello > "+goalFile {
		t.Fatalf("the goal's input bound the parameter as %q", got)
	}
	// The replay run is DERIVED from the goal run and the statement, so a
	// resumed goal run finds it.
	if w.work.run(str(reply, "replayRunId")) == nil {
		t.Fatalf("no replay run %v", reply["replayRunId"])
	}
}

// TestProcedureReplayOnACanaryThatDivergesIsServedByTheApp: canary serves
// with the app standing by, and on a divergence the app takes the goal --
// the statement answers the app served it, with the repaired run.
func TestProcedureReplayOnACanaryThatDivergesIsServedByTheApp(t *testing.T) {
	w := newReplayWorld(t, "canary")
	w.d.fail["step1"] = errors.New("the workbench went away")
	reply, err := decodeServe(t, w, inGoalRun(replayOwner))
	if err != nil {
		t.Fatalf("procedureReplay: %v", err)
	}
	if reply["servedBy"] != "app" || reply["diverged"] != true || reply["repairRunId"] != "v1:work:run:repair-1" {
		t.Fatalf("reply = %v, want the app serving with the repaired run", reply)
	}
	fb := w.f.recorded()
	if len(fb) != 1 || fb[0].StepId != journalStepId(goalRunId, "replayed") || fb[0].Statement != testStatement {
		t.Fatalf("hand-back = %+v", fb)
	}
}

// TestProcedureReplayRefusesOutsideAWorkRun: a learned procedure serves a
// goal's run, and this statement is that run's; with no run there is no
// goal, no owner and no input to bind.
func TestProcedureReplayRefusesOutsideAWorkRun(t *testing.T) {
	w := newReplayWorld(t, "trusted")
	if _, err := decodeServe(t, w, auth.ContextWithUserActor(context.Background(), replayOwner)); err == nil ||
		!strings.Contains(err.Error(), "not inside one") {
		t.Fatalf("err = %v, want the outside-a-run refusal", err)
	}
	if len(w.d.recorded()) != 0 || len(w.f.recorded()) != 0 {
		t.Fatal("a refused statement reached a seam")
	}
}

// TestProcedureReplayRefusesAnotherPersonsConstruct (Step 5): a goal is served
// only by its owner's OWN learned procedure. Bob's run naming alice's
// construct reads nothing under bob and is refused -- never served, never
// handed to an app; and mallory acting inside alice's run is refused before
// anything is read.
func TestProcedureReplayRefusesAnotherPersonsConstruct(t *testing.T) {
	w := newReplayWorld(t, "trusted")
	const bob = "v1:identity:user:bob"
	w.work.putRun(map[string]any{"id": "v1:work:run:bobs", "ownerUserId": bob, "goalId": "v1:work:goal:bobs", "status": "running"})
	bobsRun := common.ContextWithRun(auth.ContextWithUserActor(context.Background(), bob),
		common.RunContext{RunId: "v1:work:run:bobs", StepKey: "replayed", OwnerUserId: bob, Mode: common.RunModeLive})
	if _, err := decodeServe(t, w, bobsRun); err == nil || !strings.Contains(err.Error(), "not one of the run owner's learned procedures") {
		t.Fatalf("err = %v, want bob refused alice's construct", err)
	}

	if _, err := decodeServe(t, w, inGoalRun("v1:identity:user:mallory")); err == nil || !strings.Contains(err.Error(), "belongs to another person") {
		t.Fatalf("err = %v, want mallory refused inside alice's run", err)
	}
	if len(w.d.recorded()) != 0 || len(w.f.recorded()) != 0 {
		t.Fatalf("a refused statement reached a seam: %d dispatches, %d hand-backs", len(w.d.recorded()), len(w.f.recorded()))
	}
}

// TestProcedureReplayFailsTheGoalWhenNothingCouldServeIt: a divergence with no
// app fallback on the node leaves the goal unserved, and the statement FAILS
// naming why -- a goal reported done that nobody did is the one answer this
// must never give.
func TestProcedureReplayFailsTheGoalWhenNothingCouldServeIt(t *testing.T) {
	w := newReplayWorld(t, "trusted")
	w.i.seams.mu.Lock()
	w.i.seams.fallback = nil
	w.i.seams.mu.Unlock()
	w.d.fail["step0"] = errors.New("the workbench is unreachable")
	_, err := decodeServe(t, w, inGoalRun(replayOwner))
	if err == nil || !strings.Contains(err.Error(), codeFallbackUnavailable) {
		t.Fatalf("err = %v, want procedure_fallback_unavailable", err)
	}
}
