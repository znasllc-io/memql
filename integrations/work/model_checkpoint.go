package work

import (
	"context"
	"fmt"

	"github.com/znasllc-io/memql/component/memql"
	workstate "github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

// PauseWorkForModelCall preserves the same child and effects. This is not an
// approval to replay an automation or to increase any model/run allowance.
func (i *Integration) PauseWorkForModelCall(ctx context.Context, failure, checkpoint, question string) (string, error) {
	run, ok := common.RunFromContext(ctx)
	capacity := workstate.IsContextCapacityFailure(failure)
	if !ok || run.GoalId == "" || checkpoint == "" || (!workstate.IsModelCallCeiling(failure) && !capacity) {
		return "", fmt.Errorf("model checkpoint must belong to this execution")
	}
	options := []map[string]any{{"label": "Retry", "value": "retry"}, {"label": "Abandon", "value": "abandon"}}
	subject := map[string]any{"providerFailure": "model_call_ceiling", "resumeKind": "checkpoint", "continuationId": checkpoint,
		"question": question, "kind": "choice", "options": options, "errorMessage": failure}
	if capacity {
		subject["providerFailure"] = "context_capacity"
	}
	if run.Continuation != nil {
		subject["afterApprovalId"] = memql.BareShortId(run.Continuation.ApprovalID)
	}
	return i.parkHumanDecision(ctx, run.OwnerUserId, run.RunId, "feedback", subject, question, options, i.clock().Add(DefaultApprovalTTL))
}
