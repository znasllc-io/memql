package work

import (
	"context"
	"fmt"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

// PauseWorkForBudget owns one decision for the goal run, even when the active
// agent turn is several synchronous automation invocations below it.
func (i *Integration) PauseWorkForBudget(ctx context.Context, ceiling *memql.RunCeilingError, checkpoint string) (string, error) {
	run, ok := common.RunFromContext(ctx)
	if !ok || run.GoalId == "" || ceiling == nil || checkpoint == "" ||
		memql.BareShortId(ceiling.RunId) != memql.BareShortId(run.RunId) || ceiling.StepKey != run.StepKey {
		return "", fmt.Errorf("budget checkpoint must belong to this execution")
	}
	b := ceiling.Breach
	req := work.BudgetApproval(run.RunId, run.StepKey, work.CeilingBreach{Ceiling: b.Ceiling, Limit: b.Limit, Actual: b.Actual, Reason: b.Reason}, i.clock(), DefaultApprovalTTL)
	req.Subject["resumeKind"], req.Subject["continuationId"] = "checkpoint", checkpoint
	return i.parkHumanDecisionWithBudget(ctx, run.OwnerUserId, run.RunId, req.Kind, req.Subject,
		"This run reached its "+b.Ceiling+" ceiling. Raise it and carry on, or stop the run?", nil, req.ExpiresAt, ceiling)
}
