package work

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/memql"
	workstate "github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

// ModelCallBudget observes the hard model-call cap and reconciled receipt union
// without admitting or charging a call. Soft workload windows are considered at
// admission; an early recovery review must not preempt productive continuation.
// A zero limit means no model-call ceiling; an unreadable budget is an error.
func (c *RunCeilings) ModelCallBudget(ctx context.Context, rc common.RunContext) (limit, used int, err error) {
	if c == nil || !rc.IsRun() || strings.TrimSpace(rc.GoalId) == "" {
		return 0, 0, nil
	}
	b := c.budgetFor(rc)
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := c.seed(ctx, rc, b); err != nil {
		return 0, 0, fmt.Errorf("reading recovery budget: %w", err)
	}
	if err := c.refreshReceipts(ctx, rc, b); err != nil {
		return 0, 0, fmt.Errorf("reconciling recovery usage: %w", err)
	}
	declared, err := c.goalCeilings(ctx, rc)
	if err != nil {
		return 0, 0, err
	}
	b.ceilings = enforceableHere(declared)
	b.unbounded = b.ceilings == (workstate.Ceilings{})
	goal, err := c.store.goalForOwner(ownerActor(memql.ContextWithFreshRead(ctx), rc.OwnerUserId), rc.GoalId)
	if err != nil || goal == nil {
		return 0, 0, fmt.Errorf("recovery goal unavailable: %v", err)
	}
	hard, err := ceilingsOf(goal)
	if err != nil {
		return 0, 0, err
	}
	limit = hard.MaxModelCalls
	if limit == 0 {
		limit = b.ceilings.MaxModelCalls
	}
	return limit, b.spent.ModelCalls, nil
}
