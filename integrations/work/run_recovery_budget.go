package work

import (
	"context"
	"fmt"
	"strings"

	workstate "github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

// ModelCallBudget observes the same effective allowance and receipt union as
// Admit, without admitting or charging a call. Recovery can consult it before
// the hard limit is reached. A fresh goal read makes an explicit increase on
// another replica visible even while this replica is still below its old cap.
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
	return b.ceilings.MaxModelCalls, b.spent.ModelCalls, nil
}
