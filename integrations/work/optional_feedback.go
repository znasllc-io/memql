package work

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/memql"
	workstate "github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

// A transport retry acknowledges the same human receipt. It never updates the
// answer or starts work again, including after completion or cancellation.
func (i *Integration) recoverOptionalFeedbackAnswer(ctx context.Context, id, decision string, answer map[string]any) (map[string]any, error) {
	if decision != "answered" {
		return nil, nil
	}
	rows, err := i.store().query(ctx, "query "+call("workApprovalForOwner", map[string]any{"approvalId": id}))
	if err != nil || len(rows) != 1 {
		return nil, err
	}
	a := rows[0]
	if _, _, ok := workstate.OptionalFeedback(a); !ok || rowString(a, "decision") != "answered" {
		return nil, nil
	}
	want, err := json.Marshal(answer)
	if err != nil {
		return nil, err
	}
	saved, err := json.Marshal(a["answer"])
	if err != nil {
		return nil, err
	}
	if string(want) != string(saved) {
		return nil, fmt.Errorf("work: this question already has a different answer")
	}
	resumed, err := i.resumeParkedRun(ctx, rowString(a, "runId"), id, "answered", nil, i.clock())
	if err != nil {
		return nil, err
	}
	return map[string]any{"approvalId": id, "runId": rowString(a, "runId"), "decision": "answered", "runResumed": resumed}, nil
}

func (i *Integration) optionalFeedbackSubject(ctx context.Context, owner, runID string, subject map[string]any) error {
	rc, ok := common.RunFromContext(ctx)
	if !ok || rc.StepKey == "" || memql.BareShortId(rc.OwnerUserId) != memql.BareShortId(owner) || memql.BareShortId(rc.RunId) != memql.BareShortId(runID) {
		return fmt.Errorf("optional feedback requires the current owned execution")
	}
	run, err := i.store().runForOwner(memql.ContextWithFreshRead(ownerActor(ctx, owner)), runID)
	if err != nil {
		return err
	}
	if run == nil || rowString(run, "templateFingerprint") == "" || rowString(rowMap(run, "rerun"), "requestId") != rc.RerunRequestId {
		return fmt.Errorf("optional feedback execution changed")
	}
	value, err := workflowhost.RunPhase(ctx, rc.Spine, workstate.SpineContract, "workFeedbackPolicy", map[string]any{"optional": true}, workflowhost.Options{})
	if err != nil {
		return err
	}
	policy, ok := value.(map[string]any)
	seconds := rowInt(policy, "waitSeconds")
	if !ok || seconds < 1 || seconds > 300 {
		return fmt.Errorf("optional feedback policy needs a wait of 1 to 300 seconds")
	}
	override, err := common.StepOverrideFingerprint(rc.Override)
	if err != nil {
		return err
	}
	subject["feedbackContinuation"] = map[string]any{"waitSeconds": seconds, "rerunRequestId": rc.RerunRequestId, "templateFingerprint": rowString(run, "templateFingerprint"), "overrideKey": override}
	return nil
}

// Uses the same decision gate as a human answer. Neither a stale sweep nor a
// second replica can release a newer wait or consume another dispatch identity.
func (i *Integration) continueOptionalFeedback(ctx context.Context, runID string, now time.Time) (bool, error) {
	ctx = memql.ContextWithFreshRead(ctx)
	run, err := i.store().runForOwner(ctx, runID)
	if err != nil || run == nil {
		return false, err
	}
	wait := rowMap(run, "waitingOn")
	approvalID := rowString(wait, "subject")
	if rowString(run, "status") != runStatusWaiting || rowString(wait, "kind") != "approval" || rowString(wait, "approvalKind") != "feedback" || approvalID == "" {
		return false, nil
	}
	if i.decisionGate == nil {
		return false, fmt.Errorf("optional feedback coordination is unavailable")
	}
	release, err := i.decisionGate(ctx, approvalID)
	if err != nil {
		return false, err
	}
	defer release()
	rows, err := i.store().query(ctx, "query "+call("workApprovalForOwner", map[string]any{"approvalId": approvalID}))
	if err != nil || len(rows) != 1 {
		return false, err
	}
	a := rows[0]
	contract, due, ok := workstate.OptionalFeedback(a)
	if !ok || now.Before(due) || rowString(a, "decision") != "" || memql.BareShortId(rowString(a, "runId")) != memql.BareShortId(runID) {
		return false, nil
	}
	run, err = i.store().runForOwner(ctx, runID)
	if err != nil || run == nil {
		return false, err
	}
	if run["cancelRequested"] == true || rowString(run, "status") != runStatusWaiting || memql.BareShortId(rowString(rowMap(run, "waitingOn"), "subject")) != memql.BareShortId(approvalID) ||
		rowString(run, "templateFingerprint") != rowString(contract, "templateFingerprint") || rowString(rowMap(run, "rerun"), "requestId") != rowString(contract, "rerunRequestId") {
		return false, nil
	}
	checkpoints, err := i.store().query(ctx, "query "+call("workContinuationForOwnerRun", map[string]any{"runId": runID, "stepKey": rowString(a, "stepKey")}))
	if err != nil {
		return false, err
	}
	if len(checkpoints) != 1 || rowString(rowMap(checkpoints[0], "data"), "rerunRequestId") != rowString(contract, "rerunRequestId") || rowString(rowMap(checkpoints[0], "data"), "overrideKey") != rowString(contract, "overrideKey") {
		return false, fmt.Errorf("optional feedback has no matching saved checkpoint")
	}
	if rowString(a, "continuedAt") == "" {
		err = i.store().writeInternal(ctx, "mutation "+call("continueWorkFeedback", map[string]any{"approvalId": approvalID, "continuedAt": rfc(now), "versionTime": rfc(workRowVersionAfter(a["createdAt"], now))}))
		if err != nil {
			return false, err
		}
	} else if !workstate.OptionalFeedbackContinued(a, now) {
		return false, fmt.Errorf("optional feedback continuation receipt is invalid")
	}
	return i.resumeParkedRun(ctx, runID, approvalID, "optional_timeout", nil, now)
}
