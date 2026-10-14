package work

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/memql"
	workstate "github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

// A budget decision can commit before releasing its parked run fails. Recover
// only that exact durable decision: never raise the limit or record it twice,
// and never replace its saved guidance with a retry's browser state.
func (i *Integration) recoverBudgetDecision(ctx context.Context, approvalID, decision string, answer map[string]any) (map[string]any, error) {
	requested, valid := budgetNumber(answer["newLimit"])
	if decision != "approved" || len(answer) != 1 || !valid || requested <= 0 {
		return nil, nil
	}
	rows, err := i.store().query(ctx, "query "+call("workApprovalForOwner", map[string]any{"approvalId": approvalID}))
	if err != nil || len(rows) != 1 {
		return nil, err
	}
	approval := rows[0]
	subject, accepted := rowMap(approval, "subject"), rowMap(approval, "answer")
	approved, valid := budgetNumber(accepted["newLimit"])
	if rowString(approval, "kind") != workstate.ApprovalKindBudget || rowString(approval, "decision") != "approved" ||
		!valid || requested != approved || memql.BareShortId(rowString(approval, "ownerUserId")) != memql.BareShortId(callerUserId(ctx)) ||
		memql.BareShortId(rowString(approval, "decidedBy")) != memql.BareShortId(callerUserId(ctx)) {
		return nil, nil
	}
	if !workstate.ApprovalAcceptsGuidance(workstate.ApprovalKindBudget, subject, approval["options"]) ||
		workstate.ArtifactHash(subject) != rowString(approval, "artifactHash") {
		return nil, fmt.Errorf("work: the approved budget checkpoint changed")
	}
	for key, value := range accepted {
		if key == "newLimit" {
			continue
		}
		text, ok := value.(string)
		if key != "feedback" || !ok || text == "" || text != strings.TrimSpace(text) || utf8.RuneCountInString(text) > maxApprovalGuidanceCharacters {
			return nil, fmt.Errorf("work: this budget request has invalid recorded guidance")
		}
	}
	result := map[string]any{"approvalId": approvalID, "runId": rowString(approval, "runId"), "decision": "approved", "runResumed": false}
	run, err := i.readActRun(ctx, rowString(approval, "runId"))
	if err != nil {
		result["resumeError"] = err.Error()
		return result, nil
	}
	if memql.BareShortId(rowString(run.row, "humanResumeId")) == memql.BareShortId(approvalID) {
		result["runResumed"] = true
		return result, nil
	}
	if err := i.validateGuidanceRun(ctx, approval); err != nil {
		result["resumeError"] = err.Error()
		return result, nil
	}
	checkpoints, err := i.store().query(ctx, "query "+call("workContinuationForOwnerRun", map[string]any{"runId": run.id, "stepKey": rowString(approval, "stepKey")}))
	if err == nil && len(checkpoints) != 1 {
		err = fmt.Errorf("the saved work checkpoint is unavailable")
	}
	if err == nil {
		err = memql.ValidateWorkContinuationReceipt(common.RunContext{
			RunId: run.id, GoalId: rowString(run.row, "goalId"), OwnerUserId: run.owner, StepKey: rowString(approval, "stepKey"),
			Mode: rowString(run.row, "mode"), RerunRequestId: rowString(rowMap(run.row, "rerun"), "requestId"),
		}, checkpoints[0], rowString(subject, "continuationId"))
	}
	if err != nil {
		result["resumeError"] = err.Error()
		return result, nil
	}
	resumed, err := i.resumeParkedRun(ownerActor(ctx, run.owner), run.id, approvalID, "approved", nil, i.clock().UTC())
	result["runResumed"] = resumed
	if err != nil {
		result["resumeError"] = err.Error()
	}
	return result, nil
}
