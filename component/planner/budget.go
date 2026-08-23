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

	// SpentSubscription is Plan.tokenSpentSubscription: tokens spent
	// through an app the USER already pays for (memql#4362), or
	// through a run whose billing could not be determined.
	//
	// It is tracked SEPARATELY from Spent rather than added to it,
	// because the two caps want opposite answers about it:
	//
	//   - the DOLLAR ceiling must exclude it. MemQL was not billed
	//     for these tokens, so counting them would park a plan over
	//     money nobody was charged -- and the more the user leans on
	//     the subscription they already pay for, the sooner their
	//     plans would stop, which is exactly backwards.
	//
	//   - the LOOP caps must include it. A runaway decompose loop that
	//     happened to route through a subscription is still a runaway
	//     loop, and a cap that could not see those calls would be a
	//     hole the cheapest path walks straight through.
	//
	// So this field is subtracted from the ceiling check here and
	// counted by CallsMade, which the planner loop reads.
	SpentSubscription int

	// CallsMade is the Plan's cumulative LLM/executor call count,
	// every billing kind included. The loop cap reads this.
	CallsMade int
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
	// The ceiling is a DOLLAR ceiling, so it counts only what MemQL
	// paid for. state.SpentSubscription is deliberately absent from
	// this arithmetic (memql#4362) -- see the field's comment for why
	// including it would park plans over money nobody was charged.
	available := budget - state.Spent - state.AllocatedToCh
	if estimatedCallTokens > available {
		return fmt.Errorf("planner: tokenBudgetExceeded for plan %s -- %d estimated, %d available",
			planId, estimatedCallTokens, available)
	}
	return nil
}

// SplitSpend routes a completed executor's token spend to the right
// counter (memql#4362). Returns the amounts to add to Plan.tokenSpent
// and Plan.tokenSpentSubscription respectively; exactly one is
// non-zero.
//
// An executor that reports no billing is treated as METERED, the
// conservative direction: unattributed spend counts against the
// ceiling rather than vanishing into the covered bucket, where it
// would be invisible to the one control that stops runaway cost.
func SplitSpend(result ExecutorResult) (metered int, subscription int) {
	if result.CountsAgainstDollarCeiling() {
		return result.TokensSpent, 0
	}
	return 0, result.TokensSpent
}
