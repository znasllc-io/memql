package work

import (
	"context"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
)

// inferenceWaitIdentity binds a wakeup to the exact automatic availability
// wait. An approval without a readable retry time (notably a cost ceiling)
// stays a human decision. Neither this identity nor its timer grants paid
// fallback, changes routing or resets the run's accumulated usage.
func inferenceWaitIdentity(waiting map[string]any) string {
	if rowString(waiting, "kind") != "approval" || rowString(waiting, "approvalKind") != work.ApprovalKindInferenceUnavailable || rowString(waiting, "subject") == "" {
		return ""
	}
	if _, ok := parseTime(rowString(waiting, "since")); !ok {
		return ""
	}
	if _, ok := parseTime(rowString(waiting, "resumeAt")); !ok {
		return ""
	}
	return work.ArtifactHash(map[string]any{"since": waiting["since"], "subject": waiting["subject"], "resumeAt": waiting["resumeAt"]})
}

// armInferenceWait reaches agents directly from the parked row's event. The
// general cron leader may be an identity/BFF node with no dispatcher; relying
// only on that leader's redispatch left healthy subscriptions parked forever.
// Timers are local hints, deduplicated here and arbitrated by the durable run
// claim when they fire. The sweep reannounces a missed event after restart.
func (i *Integration) armInferenceWait(runID, owner string, waiting map[string]any) {
	identity := inferenceWaitIdentity(waiting)
	if identity == "" {
		return
	}
	key := "inference:" + runID + "#" + identity
	if _, loaded := i.armedRetries.LoadOrStore(key, struct{}{}); loaded {
		return
	}
	at, _ := parseTime(rowString(waiting, "resumeAt"))
	delay := at.Sub(i.clock())
	if delay < 0 {
		delay = 0
	}
	i.after(delay, func() {
		defer i.armedRetries.Delete(key)
		i.serveInferenceWait(context.Background(), runID, owner, identity)
	})
}

// serveInferenceWait re-reads under the run owner's restored authority before
// claiming. The executor repeats the exact-wait check after arbitration, so a
// delayed timer cannot serve a later wait, a stopped run, or a human approval.
// Recovery resumes the saved journal/request rather than creating a new goal
// or replaying its completed prefix. A failed claimant leaves a leased claim;
// the sweep can offer the same wait again after that lease expires.
func (i *Integration) serveInferenceWait(ctx context.Context, runID, owner, identity string) bool {
	if i.dispatcherRef() == nil || identity == "" {
		return false
	}
	ctx, err := i.runOwnerActor(ctx, runID, owner)
	if err != nil {
		return false
	}
	run, err := i.store().runForOwner(ownerActor(memql.ContextWithFreshRead(ctx), owner), runID)
	if err != nil || run == nil || argBool(run, "cancelRequested") || IsDriverOwnedRun(rowString(run, "triggeredBy")) {
		return false
	}
	name := rowString(run, "automationName")
	if name == "" || name == compilingAutomationName {
		return false
	}
	req := DispatchRequest{RunId: runID, OwnerUserId: owner, GoalId: rowString(run, "goalId"), AutomationName: name,
		Status: runStatusWaiting, TriggeredBy: rowString(run, "triggeredBy"), Recovery: true, InferenceWait: identity}
	if !req.CanDispatchStoredRun(req.GoalId, rowString(run, "status"), rowMap(run, "waitingOn"), i.clock()) {
		return false
	}
	return i.dispatchRun(ctx, req)
}

// serveInferenceRecovery is the scheduled backstop. A non-agent leader emits
// only a fresh heartbeat event for an unchanged overdue wait; it never marks
// the run running or answers the paid-provider question on the owner's behalf.
func (i *Integration) serveInferenceRecovery(ctx context.Context, run map[string]any, runID, owner string) bool {
	waiting := rowMap(run, "waitingOn")
	identity := inferenceWaitIdentity(waiting)
	if identity == "" {
		return false
	}
	if i.dispatcherRef() != nil {
		return i.serveInferenceWait(ctx, runID, owner, identity)
	}
	at, _ := parseTime(rowString(waiting, "resumeAt"))
	return i.nudgeWait(ctx, run, runID, owner, at, time.Minute)
}
