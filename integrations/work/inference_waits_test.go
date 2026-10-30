package work

import (
	"context"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
)

func TestInferenceClaimUnifiesCanonicalAndBareRunIDs(t *testing.T) {
	bare, lease := runClaimKey(DispatchRequest{RunId: "r", Recovery: true, InferenceWait: "wait"})
	canonical, otherLease := runClaimKey(DispatchRequest{RunId: "v1:work:run:r", Recovery: true, InferenceWait: "wait"})
	if bare != "v1:work:run:r#inference:wait" || bare != canonical || lease != runClaimTTL || lease != otherLease {
		t.Fatalf("availability claims differ: %q/%v and %q/%v", bare, lease, canonical, otherLease)
	}
}

func inferenceWaitRow(at time.Time) map[string]any {
	run := retryWaitRow(at)
	run["waitingOn"] = map[string]any{"kind": "approval", "approvalKind": "inferenceUnavailable", "subject": "approval-1", "since": rfc(at.Add(-5 * time.Minute)), "resumeAt": rfc(at)}
	return run
}

func TestInferenceStartupRestoresOnlyAutomaticOwnedWaitTimers(t *testing.T) {
	run := inferenceWaitRow(testNow.Add(-time.Minute))
	i, _, d, timers := retryAgent(t, &pkClaims{}, run)
	human := inferenceWaitRow(testNow.Add(-time.Minute))
	rowMap(human, "waitingOn")["approvalKind"] = "sideEffect"
	cancelled := inferenceWaitRow(testNow.Add(-time.Minute))
	cancelled["cancelRequested"] = true
	i.rowsInFlight = (&stubRowSource{rows: []map[string]any{run, human, cancelled}}).read
	if _, err := i.RestoreInferenceWaits(actorCtx("ordinary-user")); err == nil {
		t.Fatal("startup cross-owner scan accepted ordinary caller")
	}
	ctx := auth.ContextWithAccess(context.Background(), auth.MaintenanceActor("sweepWaitingWorkRuns"))
	count, err := i.RestoreInferenceWaits(ctx)
	if err != nil || count != 1 {
		t.Fatalf("restored %d: %v", count, err)
	}
	i.RestoreInferenceWaits(ctx)
	if delays := timers.fire(); len(delays) != 1 {
		t.Fatalf("duplicate startup armed %v", delays)
	}
	if got := d.settled(t, 1); len(got) != 1 {
		t.Fatalf("dispatches %v", got)
	}
}

func TestInferenceWaitEventArmsOnceAndRechecksDueTime(t *testing.T) {
	run := inferenceWaitRow(testNow.Add(20 * time.Second))
	i, _, d, timers := retryAgent(t, &pkClaims{}, run)
	i.HandleRunEvent(remedyEvent(run))
	i.HandleRunEvent(remedyEvent(run))
	if delays := timers.fire(); len(delays) != 1 || delays[0] != 20*time.Second {
		t.Fatalf("timers %v", delays)
	}
	if got := d.settled(t, 0); len(got) != 0 {
		t.Fatalf("early timer dispatched %v", got)
	}
	i.SetNow(func() time.Time { return testNow.Add(21 * time.Second) })
	i.HandleRunEvent(remedyEvent(run))
	timers.fire()
	if got := d.settled(t, 1); len(got) != 1 || got[0].InferenceWait == "" || !got[0].Recovery || got[0].RerunRequestId != "" {
		t.Fatalf("availability recovery = %+v", got)
	}
}

func TestInferenceWaitTimerRejectsChangedOrStoppedWork(t *testing.T) {
	for _, change := range []string{"approval", "since", "resumeAt", "future", "human", "cancelled", "closed", "driver", "owner", "missing-time"} {
		t.Run(change, func(t *testing.T) {
			run := inferenceWaitRow(testNow.Add(-time.Minute))
			i, eng, d, timers := retryAgent(t, &pkClaims{}, run)
			i.HandleRunEvent(remedyEvent(run))
			fresh := inferenceWaitRow(testNow.Add(-time.Minute))
			wait := rowMap(fresh, "waitingOn")
			switch change {
			case "approval":
				wait["subject"] = "new-approval"
			case "since":
				wait["since"] = rfc(testNow.Add(-7 * time.Minute))
			case "resumeAt":
				wait["resumeAt"] = rfc(testNow.Add(-2 * time.Minute))
			case "future":
				wait["resumeAt"] = rfc(testNow.Add(time.Minute))
			case "human":
				wait["approvalKind"] = "sideEffect"
			case "cancelled":
				fresh["cancelRequested"] = true
			case "closed":
				fresh["status"] = "succeeded"
			case "driver":
				fresh["triggeredBy"] = "procedure:shadow"
			case "owner":
				fresh = nil // an owner-scoped read cannot see another owner's run
			case "missing-time":
				delete(wait, "resumeAt")
			}
			eng.reply("workRunForOwner", fresh)
			timers.fire()
			if got := d.settled(t, 0); len(got) != 0 {
				t.Fatalf("stale timer dispatched %v", got)
			}
		})
	}
}

func TestInferenceWaitMissedBeforeAgentRestartIsReannounced(t *testing.T) {
	run := inferenceWaitRow(testNow.Add(-2 * time.Minute))
	leader, eng := newTestIntegration(t)
	eng.reply("workRunForOwner", run)
	res := sweepRows(context.Background(), leader, []map[string]any{run}, testNow, time.Minute)
	nudge := argsOf(t, eng, "updateWorkRun")
	if res.Redispatched != 1 || len(nudge) != 2 || nudge["heartbeatAt"] != rfc(testNow) {
		t.Fatalf("sweep %+v wrote %v", res, nudge)
	}
	announced := merged(run, nudge)
	claims := heldByTheFailingExecution(t)
	a, _, da, ta := retryAgent(t, claims, announced)
	b, _, db, tb := retryAgent(t, claims, announced)
	for _, peer := range []*Integration{a, b} {
		peer.HandleRunEvent(remedyEvent(announced))
	}
	ta.fire()
	tb.fire()
	if n := len(da.settled(t, 0)) + len(db.settled(t, 0)); n != 1 {
		t.Fatalf("dispatched %d times", n)
	}
}

func TestInferenceDispatchFenceBindsExactWait(t *testing.T) {
	wait := rowMap(inferenceWaitRow(testNow.Add(-time.Minute)), "waitingOn")
	req := DispatchRequest{GoalId: "goal", Status: "waiting", Recovery: true, InferenceWait: inferenceWaitIdentity(wait)}
	if !req.CanDispatchStoredRun("goal", "waiting", wait, testNow) {
		t.Fatal("current due wait refused")
	}
	if req.CanDispatchStoredRun("other-goal", "waiting", wait, testNow) {
		t.Fatal("claimed wait changed goals")
	}
	wait["subject"] = "another-approval"
	if req.CanDispatchStoredRun("goal", "waiting", wait, testNow) {
		t.Fatal("claimed old wait admitted a newer approval")
	}
	_, ttl := runClaimKey(req)
	if ttl != runClaimTTL {
		t.Fatal("recovery must have a leased claim")
	}
}
