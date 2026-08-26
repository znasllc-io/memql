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

	// SpentLocal is Plan.tokenSpentLocal: tokens spent on a model
	// running on one of the USER'S OWN MACHINES (epic memql#4676).
	//
	// It mirrors SpentSubscription exactly, including why it is a
	// separate field rather than an addition to Spent -- the dollar
	// ceiling must exclude it and the loop caps must include it -- and
	// the local case makes the first half sharper: the whole point of
	// running a model on hardware the user already owns is that it
	// costs nothing, so charging it to a dollar budget would mean the
	// more someone used their own machine, the sooner their plans
	// stopped.
	//
	// ABSENT IS NOT ZERO. A runtime that reports no usage leaves this
	// untouched rather than adding 0, because "the model ran and used
	// nothing" and "the model ran and nobody counted" are different
	// facts, and only one of them is ever true.
	SpentLocal int

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
	// paid for. state.SpentSubscription and state.SpentLocal are
	// deliberately absent from this arithmetic (memql#4362, memql#4681)
	// -- see the fields' comments for why including them would park
	// plans over money nobody was charged.
	available := budget - state.Spent - state.AllocatedToCh
	if estimatedCallTokens > available {
		return fmt.Errorf("planner: tokenBudgetExceeded for plan %s -- %d estimated, %d available",
			planId, estimatedCallTokens, available)
	}
	return nil
}

// Spend is where one executor's tokens land: exactly one of the three
// counters is non-zero.
type Spend struct {
	// Metered is Plan.tokenSpent -- MemQL's own vendor spend, the only
	// one the dollar ceiling reads.
	Metered int
	// Subscription is Plan.tokenSpentSubscription (memql#4362).
	Subscription int
	// Local is Plan.tokenSpentLocal (memql#4681).
	Local int
}

// SplitSpend routes a completed executor's token spend to the right
// counter (memql#4362, memql#4681).
//
// An executor that reports no billing is treated as METERED, the
// conservative direction: unattributed spend counts against the
// ceiling rather than vanishing into a covered bucket, where it would
// be invisible to the one control that stops runaway cost.
//
// `unknown` lands in Subscription rather than getting a counter of its
// own, and that is not sloppiness: the counters exist to answer "what
// did this cost us", and the honest answer for unknown is "we could
// not tell, so we are not charging you for it" -- the same treatment
// subscription gets. What must NOT happen is unknown being recorded as
// local, because that would claim the work ran on the user's hardware.
func SplitSpend(result ExecutorResult) Spend {
	switch result.EffectiveBilling() {
	case BillingLocal:
		return Spend{Local: result.TokensSpent}
	case BillingMetered:
		return Spend{Metered: result.TokensSpent}
	default:
		return Spend{Subscription: result.TokensSpent}
	}
}
