package work

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/memql"
	workstate "github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

// A feedback decision can commit before releasing its parked run fails. Recover
// only that exact durable decision: never extend the window or record it twice,
// and never replace its saved guidance with a retry's browser state.
func (i *Integration) recoverProgressDecision(ctx context.Context, approvalID, decision string, answer map[string]any) (map[string]any, error) {
	value, choice := answer["value"].(string)
	guidance, text := answer["feedback"].(string)
	guidance = strings.TrimSpace(guidance)
	if decision != "answered" || len(answer) != 1 ||
		!(choice && (value == "retry" || value == "abandon") || text && guidance != "" && utf8.RuneCountInString(guidance) <= maxApprovalGuidanceCharacters) {
		return nil, nil
	}
	if text {
		answer = map[string]any{"feedback": guidance}
	}
	rows, err := i.store().query(ctx, "query "+call("workApprovalForOwner", map[string]any{"approvalId": approvalID}))
	if err != nil || len(rows) != 1 {
		return nil, err
	}
	approval := rows[0]
	subject, accepted := rowMap(approval, "subject"), rowMap(approval, "answer")
	if rowString(approval, "kind") != workstate.ApprovalKindFeedback || rowString(approval, "decision") != "answered" ||
		workstate.ArtifactHash(accepted) != workstate.ArtifactHash(answer) || len(accepted) != 1 ||
		memql.BareShortId(rowString(approval, "ownerUserId")) != memql.BareShortId(callerUserId(ctx)) ||
		memql.BareShortId(rowString(approval, "decidedBy")) != memql.BareShortId(callerUserId(ctx)) {
		return nil, nil
	}
	if !workstate.IsProgressFeedback(subject) && !workstate.IsReasoningCheckpoint(subject) {
		return nil, nil
	}
	if workstate.ArtifactHash(subject) != rowString(approval, "artifactHash") {
		return nil, fmt.Errorf("work: the saved reasoning checkpoint changed")
	}
	result := map[string]any{"approvalId": approvalID, "runId": rowString(approval, "runId"), "decision": "answered", "runResumed": false}
	run, err := i.readActRun(ctx, rowString(approval, "runId"))
	if err != nil {
		result["resumeError"] = err.Error()
		return result, nil
	}
	if memql.BareShortId(rowString(run.row, "humanResumeId")) == memql.BareShortId(approvalID) {
		result["runResumed"] = true
		return result, nil
	}
	if rowString(run.row, "status") == runStatusFailed && (rowString(run.row, "errorCode") == "work_limit" || value == "abandon" && rowString(run.row, "errorCode") == "approval_rejected") {
		result["runStopped"] = true
		return result, nil
	}
	runDecision := "answered"
	var retry *failureRetry
	if value == "abandon" {
		runDecision = decisionAbandoned
	} else {
		if err := i.validateGuidanceRun(ctx, approval); err != nil {
			result["resumeError"] = err.Error()
			return result, nil
		}
		if err := i.validateProgressCheckpoint(ctx, approval); err != nil {
			result["resumeError"] = err.Error()
			return result, nil
		}
		if workstate.IsProgressFeedback(subject) {
			if err := i.continueFromProgressFeedback(ctx, approval); errors.Is(err, errProgressLimit) {
				runDecision = "progress_limit"
			} else if err != nil {
				result["resumeError"] = err.Error()
				return result, nil
			}
		}
		if rowString(subject, "resumeKind") == waitKindReplan {
			retry = &failureRetry{stepKey: rowString(approval, "stepKey"), symptom: workstate.SymptomPlan, reason: rowString(subject, "resumeReason")}
		}
		if guidance != "" {
			retry = guidanceRetry(approval, guidance, callerUserId(ctx))
		}
	}
	changed, err := i.resumeParkedRun(ownerActor(ctx, run.owner), run.id, approvalID, runDecision, retry, i.clock().UTC())
	result["runResumed"] = changed && runDecision == "answered"
	result["runStopped"] = changed && runDecision != "answered"
	if err != nil {
		result["resumeError"] = err.Error()
	}
	return result, nil
}

// Validate the latest saved checkpoint before consuming an answer or changing
// the work window. The execution replica repeats this check before replay.
func (i *Integration) validateProgressCheckpoint(ctx context.Context, approval map[string]any) error {
	subject := rowMap(approval, "subject")
	if rowString(subject, "resumeKind") == waitKindReplan {
		return nil
	}
	if !workstate.IsReasoningCheckpoint(subject) {
		return fmt.Errorf("the saved work checkpoint is unavailable")
	}
	run, err := i.readActRun(ctx, rowString(approval, "runId"))
	if err != nil {
		return err
	}
	checkpoints, err := i.store().query(ctx, "query "+call("workContinuationForOwnerRun", map[string]any{"runId": run.id, "stepKey": rowString(approval, "stepKey")}))
	if err == nil && len(checkpoints) != 1 {
		err = fmt.Errorf("the saved work checkpoint is unavailable")
	}
	if err == nil {
		rerun := rowMap(run.row, "rerun")
		var override *common.StepOverride
		if rowString(rerun, "stepKey") == rowString(approval, "stepKey") {
			override = common.StepOverrideFromRecord(rowMap(rerun, "override"))
		}
		err = memql.ValidateWorkContinuationReceipt(common.RunContext{
			RunId: run.id, GoalId: rowString(run.row, "goalId"), OwnerUserId: run.owner, StepKey: rowString(approval, "stepKey"),
			Override: override,
			Mode:     rowString(run.row, "mode"), RerunRequestId: rowString(rowMap(run.row, "rerun"), "requestId"),
		}, checkpoints[0], rowString(subject, "continuationId"))
	}
	return err
}
