package memql

import (
	"context"
	"fmt"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/common"
)

// WorkModelCallBudget is an observation, not admission or a new allowance.
// The installed guard reconciles receipts and reads approved limits freshly.
func (e *MemQLEngine) WorkModelCallBudget(ctx context.Context) (int, int, error) {
	run, err := recoveryOwnedRun(ctx)
	if err != nil {
		return 0, 0, err
	}
	if e.modelSeam != nil {
		if reader, ok := e.modelSeam.ceilings.(interface {
			ModelCallBudget(context.Context, common.RunContext) (int, int, error)
		}); ok {
			return reader.ModelCallBudget(ctx, run)
		}
	}
	return 0, 0, nil
}

func recoveryOwnedRun(ctx context.Context) (common.RunContext, error) {
	run, ok := common.RunFromContext(ctx)
	actor, _ := auth.AccessFromContext(ctx)
	if !ok || !run.IsRun() || run.GoalId == "" || actor == nil || run.OwnerUserId == "" || BareShortId(actor.UserId) != BareShortId(run.OwnerUserId) {
		return run, fmt.Errorf("recovery requires owned work")
	}
	return run, nil
}
