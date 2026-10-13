package work

import (
	"context"
	"fmt"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

// HoldWorkRunLifecycle is a native journal seam, not a registered capability.
// Terminal cancellation bookkeeping shares the public Stop/rerun fence. The
// journal must re-read the exact owned execution under the returned hold.
func (i *Integration) HoldWorkRunLifecycle(ctx context.Context) (*memql.GateHold, error) {
	actor, err := requirePrincipal(ctx)
	if err != nil {
		return nil, err
	}
	run, ok := common.RunFromContext(ctx)
	if !ok || run.RunId == "" || run.GoalId == "" || run.OwnerUserId == "" ||
		memql.BareShortId(actor.UserId) != memql.BareShortId(run.OwnerUserId) {
		return nil, fmt.Errorf("work lifecycle requires the owning execution")
	}
	pool, err := i.gatePool()
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	wait := 5 * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		wait = min(wait, time.Until(deadline))
	}
	if wait <= 0 {
		return nil, context.DeadlineExceeded
	}
	// The wait is bounded, but the session survives the caller deadline until
	// the journal explicitly releases it after its dispatched writes return.
	return memql.AcquireGateHold(context.WithoutCancel(ctx), pool, wait, "work-approval-decision:goal-lifecycle-"+memql.BareShortId(run.GoalId))
}
