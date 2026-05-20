package planner

import (
	"context"
	"fmt"
)

// TokenBudget is the pre-call budget check the agent's tool-call
// wrapper invokes before each LLM/tool call to enforce Q6's
// hard-stop semantics.
//
// Per Q6: pre-call check rejects when
//
//	parentPlan.tokenSpent + estimatedCallCost > parentPlan.tokenBudget
//
// UNLESS parentPlan.tokenCapDisabled = true (in which case the call
// proceeds and observability records the overage).
//
// Soft warnings (75% / 90% spent) are emitted by a separate
// automation that scans Plan rows; this helper only handles the
// hard-stop pre-call gate.
type TokenBudget interface {
	// CheckCall returns nil if the prospective call fits within the
	// Plan's remaining budget (or the cap is disabled). Returns a
	// non-nil error -- the caller maps it to a Task failure.
	CheckCall(ctx context.Context, planId string, estimatedCallTokens int) error
}

// PlanLookup is the narrow interface a TokenBudget needs from the
// engine. Decoupled so the planner package doesn't import the full
// MemQL engine surface.
type PlanLookup interface {
	GetPlanTokenState(ctx context.Context, planId string) (TokenState, error)
}

// TokenState is the snapshot a TokenBudget reads to make its check.
type TokenState struct {
	Budget        int  // Plan.tokenBudget (0 = use default)
	Spent         int  // Plan.tokenSpent
	AllocatedToCh int  // Plan.tokenAllocatedToChildren
	CapDisabled   bool // Plan.tokenCapDisabled
}

// EngineTokenBudget is the default implementation. Reads the Plan's
// token state via the injected lookup, applies the Q6 hard-stop
// rule.
type EngineTokenBudget struct {
	Lookup        PlanLookup
	DefaultBudget int // workspace default; used when Plan.tokenBudget is zero
}

// NewEngineTokenBudget creates a TokenBudget backed by the given
// PlanLookup. defaultBudget = 0 disables the workspace-default
// fallback; in that case Plans with tokenBudget=0 get unlimited
// budget (effectively bypassing the check).
func NewEngineTokenBudget(lookup PlanLookup, defaultBudget int) *EngineTokenBudget {
	return &EngineTokenBudget{Lookup: lookup, DefaultBudget: defaultBudget}
}

// CheckCall implements TokenBudget. Per Q6:
//   - tokenCapDisabled -> pass through (observability still records).
//   - tokenSpent + estimated > budget (after subtracting child
//     allocations) -> reject with a typed error the caller turns
//     into a Task failure with reason 'tokenBudgetExceeded'.
func (b *EngineTokenBudget) CheckCall(ctx context.Context, planId string, estimatedCallTokens int) error {
	if b == nil || b.Lookup == nil {
		return nil // no enforcement when not configured
	}
	state, err := b.Lookup.GetPlanTokenState(ctx, planId)
	if err != nil {
		// Lookup failure is fail-open -- don't block work because
		// of a transient query error.
		return nil
	}
	if state.CapDisabled {
		return nil
	}
	budget := state.Budget
	if budget == 0 {
		budget = b.DefaultBudget
	}
	if budget == 0 {
		// No effective budget configured -> no enforcement.
		return nil
	}
	available := budget - state.Spent - state.AllocatedToCh
	if estimatedCallTokens > available {
		return fmt.Errorf("planner: tokenBudgetExceeded for plan %s -- %d estimated, %d available",
			planId, estimatedCallTokens, available)
	}
	return nil
}
