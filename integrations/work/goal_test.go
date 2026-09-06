package work

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	work "github.com/znasllc-io/memql/component/work"
)

// artifactHashOf is the pure hash, borrowed for fixtures so a test never
// hard-codes a digest. A hard-coded one would pass with the hash function
// broken.
func artifactHashOf(subject map[string]any) string { return work.ArtifactHash(subject) }

// TestCreateGoalOpensTheGoalAndItsFirstRun pins the shape of the accept.
func TestCreateGoalOpensTheGoalAndItsFirstRun(t *testing.T) {
	i, eng := newTestIntegration(t)
	ctx := callerContext("u-alice")

	nodes, err := i.handleCreateGoal(ctx, map[string]any{
		"statement":    "reconcile the September invoices",
		"input":        map[string]any{"month": "2026-09"},
		"accountIds":   []any{"v1:accounts:account:acme"},
		"ceilings":     map[string]any{"maxModelCalls": 12},
		"requestedVia": "nexus",
	}, 0)
	if err != nil {
		t.Fatalf("createGoal: %v", err)
	}

	goalCall := eng.callTo(t, "createWorkGoal")
	goalArgs := goalCall.Args(t)
	if got := goalArgs["statement"]; got != "reconcile the September invoices" {
		t.Errorf("statement reached the engine as %v", got)
	}
	if got := goalArgs["origin"]; got != "user" {
		t.Errorf("origin = %v, want user", got)
	}
	if got := goalArgs["requestedVia"]; got != "nexus" {
		t.Errorf("requestedVia = %v", got)
	}
	// ownerUserId must NOT be an argument: the concept marks it @serverSet and
	// the mutation stamps it from actor.userId. An argument here would be a
	// forgeable owner.
	if _, present := goalArgs["ownerUserId"]; present {
		t.Error("createWorkGoal was called with an ownerUserId argument; the field is @serverSet and must arrive through the actor")
	}
	if goalCall.Actor != "u-alice" {
		t.Errorf("the goal was written under actor %q, want the caller", goalCall.Actor)
	}

	runCall := eng.callTo(t, "createWorkRun")
	runArgs := runCall.Args(t)
	if got := runArgs["status"]; got != runStatusCompiling {
		t.Errorf("the first run opened at status %v, want %q -- a run opened after compile leaves compile's own model calls unjournaled", got, runStatusCompiling)
	}
	if got := runArgs["mode"]; got != modeLive {
		t.Errorf("mode = %v, want live", got)
	}
	if runArgs["goalId"] != goalArgs["goalId"] {
		t.Errorf("the run names goal %v but the goal was written as %v", runArgs["goalId"], goalArgs["goalId"])
	}
	if _, present := runArgs["ownerUserId"]; present {
		t.Error("createWorkRun was called with an ownerUserId argument; it is @serverSet")
	}
	if runCall.Actor != "u-alice" {
		t.Errorf("the run was written under actor %q, want the goal owner's borrowed authority", runCall.Actor)
	}

	reply := decodeReply(t, nodes)
	if reply["goalId"] != goalArgs["goalId"] || reply["runId"] != runArgs["runId"] {
		t.Errorf("the reply names {%v, %v} but the rows written were {%v, %v}",
			reply["goalId"], reply["runId"], goalArgs["goalId"], runArgs["runId"])
	}
	// No compiler is bound in this harness, so the reply must SAY so rather
	// than implying work started.
	if reply["compileDispatched"] != false {
		t.Errorf("compileDispatched = %v with no compile surface bound; a caller must be able to tell", reply["compileDispatched"])
	}
	if !strings.HasPrefix(reply["goalId"].(string), goalConcept+":") {
		t.Errorf("goal id %q is not canonical", reply["goalId"])
	}
	if !strings.HasPrefix(reply["runId"].(string), runConcept+":") {
		t.Errorf("run id %q is not canonical", reply["runId"])
	}
}

// TestCreateGoalRefusesAnUnknownRequestedVia: the enum is closed on the
// concept, and refusing at the door names the ARGUMENT rather than the concept.
func TestCreateGoalRefusesAnUnknownRequestedVia(t *testing.T) {
	i, eng := newTestIntegration(t)
	_, err := i.handleCreateGoal(callerContext("u1"), map[string]any{
		"statement": "x", "requestedVia": "telepathy",
	}, 0)
	if err == nil {
		t.Fatal("an unknown requestedVia was accepted")
	}
	if got := eng.summary(); got != "(none)" {
		t.Errorf("the goal was written before the argument was checked: %s", got)
	}
}

// TestCreateGoalDispatchesCompileWithTheBudgetScope.
//
// The budget scope is what makes the per-run and per-goal ceilings reachable
// by the LLM guard at the provider chokepoint. Compile is the first thing that
// can reach a model on a goal's behalf, so a scope applied later leaves
// exactly the runaway-compile calls uncounted.
func TestCreateGoalDispatchesCompileWithTheBudgetScope(t *testing.T) {
	i, _ := newTestIntegration(t)
	rec := &recordingCompiler{done: make(chan CompileRequest, 1)}
	i.SetCompiler(rec)

	nodes, err := i.handleCreateGoal(callerContext("u-alice"), map[string]any{"statement": "do it"}, 0)
	if err != nil {
		t.Fatalf("createGoal: %v", err)
	}
	reply := decodeReply(t, nodes)
	if reply["compileDispatched"] != true {
		t.Fatalf("compileDispatched = %v with a compiler bound", reply["compileDispatched"])
	}
	req := <-rec.done
	if req.GoalId != reply["goalId"] || req.RunId != reply["runId"] {
		t.Errorf("compile was handed {%s, %s}, the reply names {%v, %v}", req.GoalId, req.RunId, reply["goalId"], reply["runId"])
	}
	if req.OwnerUserId != "u-alice" {
		t.Errorf("compile was handed owner %q", req.OwnerUserId)
	}
	if rec.actor != "u-alice" {
		t.Errorf("compile ran under actor %q, want the goal owner's borrowed authority -- an owned read under any other actor answers zero rows", rec.actor)
	}
	// The scope assertion is in two halves, because the guard reads its
	// scopes off the context through an UNEXPORTED key: a test can observe
	// THAT a context carries scopes, and can assert WHICH ones separately
	// against the one function that names them.
	if !rec.hadBudgetScope {
		t.Error("the compile context carries no budget scope at all -- the per-run and per-goal ceilings are unreachable by the LLM guard, so a runaway compile is uncapped")
	}
	got := compileBudgetScopes(req)
	for _, want := range []string{"run:" + req.RunId, "goal:" + req.GoalId} {
		if !containsString(got, want) {
			t.Errorf("compileBudgetScopes = %v, missing %q", got, want)
		}
	}
	// And the detached context must SURVIVE the caller's, or compile dies the
	// instant the builtin returns.
	if rec.cancelled {
		t.Error("the compile context was already cancelled; compile outlives the call that dispatched it by design")
	}
}

// TestCancelGoalAsksOnlyLiveRuns.
//
// Cancellation is REQUESTED rather than done, and a terminal run has nothing
// to be asked. Writing cancelRequested onto a succeeded run would put a
// pending-looking flag on a finished record.
func TestCancelGoalAsksOnlyLiveRuns(t *testing.T) {
	i, eng := newTestIntegration(t)
	ctx := callerContext("u-alice")
	eng.reply("workGoalForOwner", map[string]any{"id": "v1:work:goal:g1", "ownerUserId": "u-alice"})
	eng.reply("workRunsForGoal",
		map[string]any{"id": "v1:work:run:r1", "status": runStatusRunning},
		map[string]any{"id": "v1:work:run:r2", "status": runStatusSucceeded},
		map[string]any{"id": "v1:work:run:r3", "status": runStatusWaiting},
		map[string]any{"id": "v1:work:run:r4", "status": runStatusAbandoned},
	)

	nodes, err := i.handleCancelGoal(ctx, map[string]any{"goalId": "v1:work:goal:g1", "reason": "no longer needed"}, 0)
	if err != nil {
		t.Fatalf("cancelGoal: %v", err)
	}

	updates := eng.callsTo("updateWorkRun")
	if len(updates) != 2 {
		t.Fatalf("asked %d runs to stop, want 2 (the running and the waiting one); calls: %s", len(updates), eng.summary())
	}
	asked := map[string]bool{}
	for _, u := range updates {
		args := u.Args(t)
		asked[args["runId"].(string)] = true
		if args["cancelRequested"] != true {
			t.Errorf("run %v was updated without cancelRequested", args["runId"])
		}
		if args["cancelledBy"] != "u-alice" {
			t.Errorf("cancelledBy = %v, want the caller", args["cancelledBy"])
		}
		// A cancel must never write a terminal status: the run notices at its
		// next step boundary, so a step already in flight finishes and is
		// journaled rather than abandoned mid-effect.
		if _, present := args["status"]; present {
			t.Errorf("cancelGoal wrote a status onto run %v; cancellation is requested, not done", args["runId"])
		}
	}
	if !asked["v1:work:run:r1"] || !asked["v1:work:run:r3"] {
		t.Errorf("the wrong runs were asked: %v", asked)
	}

	reply := decodeReply(t, nodes)
	if reply["runsAsked"] != float64(2) {
		t.Errorf("runsAsked = %v", reply["runsAsked"])
	}
	if reply["goalClosed"] != true {
		t.Errorf("goalClosed = %v, want true -- cancelGoal closes the goal row", reply["goalClosed"])
	}

	// THE ORDER IS THE GUARANTEE. The runs are asked to stop FIRST and the
	// goal closes after: a goal closed first is one reported finished while
	// its runs are still executing. Asserting the reply alone would pass with
	// the writes in either order.
	closeAt, lastRunAt := -1, -1
	for n, c := range eng.calls {
		switch {
		case strings.Contains(c.Query, "closeWorkGoal("):
			closeAt = n
			// The close is a @serverOnly write under the OWNER's borrowed
			// authority, not the caller's -- the same rule the run updates
			// above follow, and worth pinning on the goal too.
			if c.Origin != auth.OriginInternal {
				t.Errorf("closeWorkGoal ran on origin %v; @serverOnly is refused on anything but internal", c.Origin)
			}
		case strings.Contains(c.Query, "updateWorkRun("):
			lastRunAt = n
		}
	}
	if closeAt < 0 {
		t.Fatalf("the goal row was never closed: %+v", eng.calls)
	}
	if lastRunAt > closeAt {
		t.Errorf("the goal closed at call %d, before a run was asked at %d -- a goal closed first reports finished while its runs are still executing", closeAt, lastRunAt)
	}
}

// TestCancelGoalRefusesAGoalTheCallerCannotRead. The owned tier IS the
// authorization: a goal that does not come back is a goal that is not theirs.
func TestCancelGoalRefusesAGoalTheCallerCannotRead(t *testing.T) {
	i, eng := newTestIntegration(t)
	// workGoalForOwner answers nothing -- which is what the read gate does for
	// somebody else's goal: zero rows, no error.
	_, err := i.handleCancelGoal(callerContext("u-mallory"), map[string]any{"goalId": "v1:work:goal:not-mine"}, 0)
	if err == nil {
		t.Fatal("cancelGoal acted on a goal that did not come back from the owned read")
	}
	if len(eng.callsTo("updateWorkRun")) != 0 {
		t.Error("cancelGoal wrote to a run despite failing to read the goal")
	}
}

// ---------------------------------------------------------------------------

type recordingCompiler struct {
	done           chan CompileRequest
	actor          string
	hadBudgetScope bool
	cancelled      bool
}

func (c *recordingCompiler) Compile(ctx context.Context, req CompileRequest) {
	c.actor = callerUserId(ctx)
	c.hadBudgetScope = hasBudgetScope(ctx)
	select {
	case <-ctx.Done():
		c.cancelled = true
	default:
	}
	c.done <- req
}

// hasBudgetScope reports whether ctx carries any budget scope, using the one
// EXPORTED behaviour that reveals it: ContextWithBudgetScope with no ids
// returns ctx UNCHANGED when the context carries none (its merged list is
// empty and the function short-circuits), and a fresh context when it does.
func hasBudgetScope(ctx context.Context) bool {
	return memql.ContextWithBudgetScope(ctx) != ctx
}

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
