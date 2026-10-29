package work

import (
	"context"
	"errors"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// automationContext is what a tree-loaded automation's dispatch looks like to a
// handler: internal origin, and the executor's own system actor. Built here
// rather than borrowed from recorder_test.go because the gate this file is
// about is the ORIGIN, and a helper that also carried a role would let a test
// pass for the wrong reason.
func automationContext() context.Context {
	return auth.ContextWithInternalOrigin(context.Background())
}

// killswitch_test.go -- memql#5066.
//
// The acceptance the issue names is two properties, and the second one is the
// hard one:
//
//	Flipping computerUseEnabled to false stops in-flight computer-use work,
//	with a test that FAILS against the pre-dispatch gate alone.
//	The selection is no broader than "runs actually using the worker".
//
// The first is easy to satisfy accidentally -- "cancel every run of that
// user" passes it -- so the file below is mostly about the second. Note what a
// query-matching fake CANNOT prove here: it has no row-authz gates, so
// "the write ran as the owner" is asserted as the ACTOR ON THE CALL rather
// than as a permission outcome. recorder_test.go's header states that
// standard; the enforcement itself is the concept's tier and
// TestRowAuthzEnforcementLandGate.

func runningRun(id string) map[string]any {
	return map[string]any{"id": id, "status": "running", "ownerUserId": "u-alice"}
}

func invocationRow(runId string) map[string]any {
	return map[string]any{"id": "v1:worker:invocation:i-" + runId, "runId": runId, "ownerUserId": "u-alice"}
}

// runningReads is what the kill switch asked its running-runs read: one entry
// per call, the owner it named and the actor the call carried.
type runningReads []struct{ owner, actor string }

// answerRunning makes the kill switch's running-runs read -- the recovery
// projection in production (runningRunsForOwner) -- answer rows, and records
// every call made of it.
func answerRunning(i *Integration, rows ...map[string]any) *runningReads {
	reads := &runningReads{}
	i.runningRuns = func(ctx context.Context, owner string) ([]map[string]any, error) {
		actor := ""
		if ac, ok := auth.AccessFromContext(ctx); ok && ac != nil {
			actor = ac.UserId
		}
		*reads = append(*reads, struct{ owner, actor string }{owner, actor})
		return rows, nil
	}
	return reads
}

// THE SELECTION, which is the whole design. Two runs are in flight; one
// dispatched to a machine and one never did. Only the first may be asked to
// stop.
//
// This is the assertion that fails against "cancel every in-flight run of that
// user" -- the broad reading the issue explicitly rules out -- and against the
// pre-dispatch gate alone, which asks nothing to stop at all.
func TestKillSwitchAsksOnlyTheRunsThatUsedAWorker(t *testing.T) {
	i, eng := newTestIntegration(t)

	reads := answerRunning(i, runningRun("v1:work:run:r-used"), runningRun("v1:work:run:r-never"))
	eng.replyWhen("invocationsForRun", "r-used", invocationRow("v1:work:run:r-used"))
	eng.replyWhen("invocationsForRun", "r-never") // no rows: this run never touched a machine

	nodes, err := i.handleCancelComputerUseRuns(automationContext(),
		map[string]any{"ownerUserId": "u-alice"}, 0)
	if err != nil {
		t.Fatalf("cancelComputerUseRuns: %v", err)
	}

	updates := eng.callsTo("updateWorkRun")
	if len(updates) != 1 {
		t.Fatalf("asked %d runs to stop, want exactly 1 -- the run that never dispatched to a machine\n"+
			"is not computer-use work, and cancelling it is broader than the control this replaces", len(updates))
	}
	args := updates[0].Args(t)
	if args["runId"] != "v1:work:run:r-used" {
		t.Errorf("stopped %v, want the run with a worker invocation behind it", args["runId"])
	}
	if args["cancelRequested"] != true {
		t.Errorf("the run was updated without cancelRequested: %v", args)
	}
	if args["cancelledBy"] != killSwitchActor {
		t.Errorf("cancelledBy = %v, want the kill-switch sentinel -- nobody clicked stop on this run,\n"+
			"and attributing it to the owner would misreport what they did", args["cancelledBy"])
	}
	if updates[0].Actor != "u-alice" {
		t.Errorf("the cancel was written under actor %q, want the affected owner's borrowed authority:\n"+
			"v1:work:run's write guard ignores the clusterOwner arm, so a write as anyone else is refused", updates[0].Actor)
	}

	if len(*reads) != 1 || (*reads)[0].owner != "u-alice" || (*reads)[0].actor != "u-alice" {
		t.Errorf("the running runs were read as %+v, want one read of u-alice's runs under u-alice's borrowed\n"+
			"authority: the selection is caller-scoped, and under a cluster owner it would answer for everyone", *reads)
	}

	reply := decodeReply(t, nodes)
	if reply["runsChecked"] != float64(2) && reply["runsChecked"] != 2 {
		t.Errorf("runsChecked = %v, want 2", reply["runsChecked"])
	}
	if reply["runsAsked"] != float64(1) && reply["runsAsked"] != 1 {
		t.Errorf("runsAsked = %v, want 1", reply["runsAsked"])
	}
}

// ...and the same selection from the other side: a person with in-flight work
// that has never touched a machine loses nothing when they turn the switch off.
func TestKillSwitchLeavesRunsThatNeverTouchedAMachineAlone(t *testing.T) {
	i, eng := newTestIntegration(t)
	answerRunning(i, runningRun("v1:work:run:r-a"), runningRun("v1:work:run:r-b"))
	// invocationsForRun is unlisted, so it answers no rows for either.

	if _, err := i.handleCancelComputerUseRuns(automationContext(),
		map[string]any{"ownerUserId": "u-alice"}, 0); err != nil {
		t.Fatalf("cancelComputerUseRuns: %v", err)
	}
	if got := len(eng.callsTo("updateWorkRun")); got != 0 {
		t.Fatalf("asked %d runs to stop; none of them used a worker", got)
	}
}

// A run whose invocation history cannot be READ is left running.
//
// The opposite default is the tempting one -- "cannot tell, so stop it" -- and
// it turns a database hiccup into every in-flight run of that user being
// cancelled, which is the broad behaviour the selection exists to avoid. The
// enforced control is not open meanwhile: the pre-dispatch gate is still
// refusing their next call, and it fails CLOSED on its own lookup error.
func TestKillSwitchLeavesARunAloneWhenItCannotTellWhetherAWorkerWasUsed(t *testing.T) {
	i, eng := newTestIntegration(t)
	answerRunning(i, runningRun("v1:work:run:r-unknown"))
	eng.refuse("invocationsForRun", errors.New("read timeout"))

	nodes, err := i.handleCancelComputerUseRuns(automationContext(),
		map[string]any{"ownerUserId": "u-alice"}, 0)
	if err != nil {
		t.Fatalf("a single unreadable run must not fail the whole sweep: %v", err)
	}
	if got := len(eng.callsTo("updateWorkRun")); got != 0 {
		t.Fatalf("stopped %d runs on a read error; 'cannot tell' must not resolve to 'stop everything'", got)
	}
	if reply := decodeReply(t, nodes); reply["runsAsked"] != float64(0) && reply["runsAsked"] != 0 {
		t.Errorf("runsAsked = %v, want 0", reply["runsAsked"])
	}
}

// One run that will not take the flag must not stop the rest being asked.
func TestKillSwitchKeepsAskingAfterOneRunRefusesTheFlag(t *testing.T) {
	i, eng := newTestIntegration(t)
	answerRunning(i, runningRun("v1:work:run:r-1"), runningRun("v1:work:run:r-2"))
	eng.reply("invocationsForRun", invocationRow("v1:work:run:r-1"))
	eng.refuse("updateWorkRun", errors.New("write conflict"))

	nodes, err := i.handleCancelComputerUseRuns(automationContext(),
		map[string]any{"ownerUserId": "u-alice"}, 0)
	if err != nil {
		t.Fatalf("one failed write must not fail the sweep: %v", err)
	}
	if got := len(eng.callsTo("updateWorkRun")); got != 2 {
		t.Fatalf("only %d runs were asked; a refusal on the first must not skip the second", got)
	}
	// ...and the reply must not claim it stopped work it did not stop.
	if reply := decodeReply(t, nodes); reply["runsAsked"] != float64(0) && reply["runsAsked"] != 0 {
		t.Errorf("runsAsked = %v after every write was refused; the count must be what LANDED", reply["runsAsked"])
	}
}

// THE GATE, and it is the one memql#2888 exists for. The argument names WHOSE
// runs to stop, so the ARGUMENT is the authorization decision -- and the attack
// that comment records is a caller supplying the trigger payload, not a caller
// holding a role. A call that did not arrive on a tree-loaded automation's
// internal-origin dispatch is refused.
func TestCancelComputerUseRunsRefusesACallerSuppliedPayload(t *testing.T) {
	i, eng := newTestIntegration(t)
	reads := answerRunning(i, runningRun("v1:work:run:r-1"))
	eng.reply("invocationsForRun", invocationRow("v1:work:run:r-1"))

	if _, err := i.handleCancelComputerUseRuns(callerContext("u-mallory"),
		map[string]any{"ownerUserId": "u-alice"}, 0); err == nil {
		t.Fatal("a writer cancelled another person's runs; a call without internal origin must be refused")
	}
	if got := len(eng.recorded()) + len(*reads); got != 0 {
		t.Errorf("the refusal happened after %d reads; a refusal after a read is a read that happened", got)
	}
}

// ...and a CLUSTER OWNER is refused too, on the same call, which is the part
// worth pinning. A role floor was the first shape tried here and it does not
// stop the attack: run_automation lets a caller choose the payload, and the
// payload is the decision. Origin is what distinguishes the engine's own
// dispatch from a caller's.
func TestCancelComputerUseRunsIsNotOpenedByARole(t *testing.T) {
	i, eng := newTestIntegration(t)
	reads := answerRunning(i, runningRun("v1:work:run:r-1"))
	eng.reply("invocationsForRun", invocationRow("v1:work:run:r-1"))

	if _, err := i.handleCancelComputerUseRuns(clusterOwnerContext("u-operator"),
		map[string]any{"ownerUserId": "u-alice"}, 0); err == nil {
		t.Fatal("a cluster owner reached the builtin directly; the gate is the origin, not the role")
	}
	if got := len(eng.recorded()) + len(*reads); got != 0 {
		t.Errorf("the refusal happened after %d reads", got)
	}
}

// A blank owner is refused rather than treated as "everyone".
func TestCancelComputerUseRunsRefusesABlankOwner(t *testing.T) {
	i, eng := newTestIntegration(t)
	reads := answerRunning(i)
	if _, err := i.handleCancelComputerUseRuns(automationContext(),
		map[string]any{"ownerUserId": "  "}, 0); err == nil {
		t.Fatal("a blank ownerUserId was accepted; borrowed authority for nobody is the cluster owner's own actor")
	}
	if got := len(eng.recorded()) + len(*reads); got != 0 {
		t.Errorf("the refusal happened after %d reads", got)
	}
}
