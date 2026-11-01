package work

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

// Feedback on an unaccepted result continues the SAME run through the native
// replanner. Unlike rerunStep, it neither selects an old effect to repeat nor
// reopens an accepted goal. The lifecycle gate covers the fresh result/version
// check and the write, so acceptance and continuation cannot both win.
func (i *Integration) handleContinueRun(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ac, err := requirePrincipal(ctx)
	if err != nil {
		return nil, err
	}
	if rc, running := common.RunFromContext(ctx); ac.Synthetic || ac.Unranked || (running && rc.IsRun()) {
		return nil, fmt.Errorf("work: continuing a finished result requires the person's feedback")
	}
	runID, requestID := argString(args, "runId"), argString(args, "requestId")
	feedback, version := strings.TrimSpace(argString(args, "feedback")), argString(args, "runVersion")
	expected, err := time.Parse(time.RFC3339Nano, version)
	if err != nil || runID == "" || requestID == "" || len(requestID) > 200 || feedback == "" || utf8.RuneCountInString(feedback) > 4000 {
		return nil, fmt.Errorf("work: continuation needs the exact result version, a request id and 1 to 4000 characters of feedback")
	}
	version = rfc(expected)
	run, err := i.readActRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	goalID := rowString(run.row, "goalId")
	release, err := i.decisionGate(ctx, "goal-lifecycle-"+memql.BareShortId(goalID), "workload:"+memql.BareShortId(run.id), "provider-admission:"+memql.BareShortId(run.id))
	if err != nil {
		return nil, err
	}
	defer release()
	ctx = memql.ContextWithFreshRead(ctx)
	run, err = i.readActRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	goal, err := i.store().goalForOwner(ctx, goalID)
	if err != nil {
		return nil, err
	}
	if goal == nil || rowString(goal, "status") != "open" || len(rowMap(goal, "acceptance")) != 0 ||
		memql.BareShortId(rowString(goal, "ownerUserId")) != memql.BareShortId(ac.UserId) ||
		memql.BareShortId(run.owner) != memql.BareShortId(ac.UserId) || rowString(run.row, "goalId") != goalID ||
		run.row["cancelRequested"] == true || rowString(run.row, "parentRunId") != "" {
		return nil, fmt.Errorf("work: only the owner's open, unaccepted goal can continue from this result")
	}
	saved, accepted, _, err := i.readResultFeedback(ctx, run.id, requestID)
	if err != nil {
		return nil, err
	}
	if saved != nil && (rowString(saved, "runVersion") != version || rowString(saved, "feedback") != feedback || rowString(saved, "requestedBy") != ac.UserId) {
		return nil, fmt.Errorf("work: this continuation request already names different feedback")
	}
	if accepted {
		return i.resultNode(map[string]any{"runId": run.id, "continuationRequested": true, "feedbackReceiptState": "sealed"}), nil
	}
	receipt := rowMap(run.row, "feedbackContinuation")
	if rowString(receipt, "requestId") == requestID {
		if rowString(receipt, "runVersion") != version || rowString(receipt, "feedback") != feedback || rowString(receipt, "requestedBy") != ac.UserId {
			return nil, fmt.Errorf("work: this continuation request already names different feedback")
		}
		return i.resultNode(i.resultFeedbackAcknowledgement(ctx, run.id, receipt)), nil
	}
	at, valid := rowTime(run.row, "createdAt")
	status := rowString(run.row, "status")
	if !valid || !at.Equal(expected) || (status != runStatusSucceeded && status != runStatusFailed) {
		return nil, fmt.Errorf("work: the result changed; review its current state before continuing")
	}
	if err := run.requireExecutable(); err != nil {
		return nil, err
	}
	if mode := rowString(run.row, "mode"); mode != "" && mode != common.RunModeLive {
		return nil, fmt.Errorf("work: continue the live goal result, not a replay or branch")
	}
	conflicts, err := i.store().query(ctx, "query "+call("workContinuationConflictsForOwnerGoal", map[string]any{"goalId": goalID, "runVersion": version}))
	if err != nil {
		return nil, err
	}
	if len(conflicts) != 0 {
		return nil, fmt.Errorf("work: newer or active work exists; review the current goal result")
	}
	// Reading the sealed template and journal must succeed before dispatch.
	// The target anchors the recovery phase; it is NOT a request to replay it.
	if _, err := i.store().replanTemplate(ctx, run.row); err != nil {
		return nil, err
	}
	if _, err := i.readActVersions(ctx, &run); err != nil {
		return nil, err
	}
	if len(run.order) == 0 {
		return nil, fmt.Errorf("work: this result has no recorded plan to continue")
	}
	now := i.clock().UTC()
	classification, err := i.finishedFeedbackWindow(ctx, goal, run.row, requestID, now)
	if err != nil {
		return nil, err
	}
	if err := i.sealResultFeedback(ctx, run.id, receipt); err != nil {
		return nil, err
	}
	if saved == nil {
		saved = map[string]any{"schemaVersion": 1, "id": memql.WorkResultFeedbackID(run.id, requestID), "requestId": requestID, "runVersion": version, "feedback": feedback, "requestedBy": ac.UserId, "requestedAt": rfc(now), "stepKey": run.order[len(run.order)-1]}
		if err := i.writeResultFeedback(ctx, run.id, saved, false); err != nil {
			return nil, err
		}
	}
	fields := remedyReopenFields(now)
	fields["classification"] = classification
	fields["versionTime"] = rfc(workRowVersionAfter(run.row["createdAt"], now))
	fields["status"] = runStatusWaiting
	fields["waitingOn"] = map[string]any{"kind": waitKindReplan, "subject": run.order[len(run.order)-1], "since": rfc(now),
		"reason":            "The owner supplied feedback on the finished, unaccepted result. Preserve all completed work and its evidence; plan only the remaining work. No permission, budget increase or acceptance was granted.\n" + feedback,
		"feedbackRequestId": requestID}
	fields["feedbackContinuation"] = saved
	fields["stepOrder"] = run.checkpointOrder()
	if err := i.store().updateRun(ownerActor(ctx, run.owner), run.id, fields); err != nil {
		return nil, err
	}
	return i.resultNode(i.resultFeedbackAcknowledgement(ctx, run.id, saved)), nil
}
