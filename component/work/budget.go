package work

// budget.go -- the run's ceilings (design record
// docs/superpowers/specs/2026-09-05-work-spine-design.md, section E
// "Govern"), moved here from component/planner/budget.go with the plan
// swapped for the run.
//
// THE THREE-COUNTER SPLIT IS THE POINT, and it is the same one the
// planner's Plan row carries, for the same reason (root CLAUDE.md, and
// memql#4362 / memql#4676):
//
//   - The DOLLAR ceilings -- tokenBudget and costCeiling -- EXCLUDE
//     subscription and local spend. MemQL was not billed for a call that
//     ran through an app the user already pays for, or on a machine they
//     own, so counting it would park a run over money nobody was charged;
//     and the more somebody leaned on what they already own, the sooner
//     their work would stop, which is backwards.
//   - The LOOP caps -- maxModelCalls, maxRetries, maxEvents, wallClockMs
//     -- INCLUDE every call. A runaway loop that happens to route through
//     a subscription is still a runaway loop, and a cap blind to those
//     calls is a hole the cheapest path walks straight through.
//
// ZERO IS UNSET. A goal that declares no ceilings gets the deployment's
// defaults applied by the caller, not a ceiling of nothing -- reading 0
// as "nothing allowed" would park every run that did not fill the form in.

import "fmt"

// Ceiling names, as they appear on a budget approval.
const (
	CeilingTokens     = "tokens"
	CeilingCost       = "cost"
	CeilingWallClock  = "wallClock"
	CeilingModelCalls = "modelCalls"
	CeilingRetries    = "retries"
	CeilingEvents     = "events"
)

// Ceilings is v1:work:goal.ceilings, inherited by every run.
type Ceilings struct {
	TokenBudget   int     `json:"tokenBudget,omitempty"`
	CostCeiling   float64 `json:"costCeiling,omitempty"`
	WallClockMs   int64   `json:"wallClockMs,omitempty"`
	MaxRetries    int     `json:"maxRetries,omitempty"`
	MaxModelCalls int     `json:"maxModelCalls,omitempty"`
	MaxEvents     int     `json:"maxEvents,omitempty"`
}

// Spent is v1:work:run.spent.
type Spent struct {
	Tokens             int     `json:"tokens,omitempty"`
	TokensSubscription int     `json:"tokensSubscription,omitempty"`
	TokensLocal        int     `json:"tokensLocal,omitempty"`
	Cost               float64 `json:"cost,omitempty"`
	ModelCalls         int     `json:"modelCalls,omitempty"`
	Retries            int     `json:"retries,omitempty"`
	Events             int     `json:"events,omitempty"`
	WallClockMs        int64   `json:"wallClockMs,omitempty"`
}

// CeilingBreach is what parks the run, and what a person reads on the
// budget approval. It names the numbers because "over budget" with no
// figures is not something anyone can decide about.
type CeilingBreach struct {
	// Ceiling is one of the Ceiling* constants.
	Ceiling string `json:"ceiling"`
	// Limit is the ceiling's value, rendered.
	Limit string `json:"limit"`
	// Actual is what the run has spent, rendered.
	Actual string `json:"actual"`
	// Reason is the breach in a sentence.
	Reason string `json:"reason"`
}

// CheckCeilings returns the first ceiling this run has reached, or nil.
// estimatedTokens is the cost of the call about to be made, so the check
// happens BEFORE the spend rather than after it.
func CheckCeilings(c Ceilings, s Spent, estimatedTokens int) *CeilingBreach {
	// Dollar ceilings: metered spend only.
	if c.TokenBudget > 0 && s.Tokens+estimatedTokens > c.TokenBudget {
		return &CeilingBreach{
			Ceiling: CeilingTokens,
			Limit:   fmt.Sprintf("%d tokens", c.TokenBudget),
			Actual:  fmt.Sprintf("%d spent + %d estimated", s.Tokens, estimatedTokens),
			Reason:  "the run reached its metered token budget; subscription and local spend are excluded because MemQL was not billed for them",
		}
	}
	if c.CostCeiling > 0 && s.Cost >= c.CostCeiling {
		return &CeilingBreach{
			Ceiling: CeilingCost,
			Limit:   fmt.Sprintf("$%.2f", c.CostCeiling),
			Actual:  fmt.Sprintf("$%.2f", s.Cost),
			Reason:  "the run reached its cost ceiling",
		}
	}
	// Loop caps: every call, whoever paid.
	if c.MaxModelCalls > 0 && s.ModelCalls >= c.MaxModelCalls {
		return &CeilingBreach{
			Ceiling: CeilingModelCalls,
			Limit:   fmt.Sprintf("%d calls", c.MaxModelCalls),
			Actual:  fmt.Sprintf("%d made", s.ModelCalls),
			Reason:  "the run reached its model-call cap; the cap counts every call, including ones billed to a subscription or run locally",
		}
	}
	if c.MaxRetries > 0 && s.Retries >= c.MaxRetries {
		return &CeilingBreach{
			Ceiling: CeilingRetries,
			Limit:   fmt.Sprintf("%d retries", c.MaxRetries),
			Actual:  fmt.Sprintf("%d used", s.Retries),
			Reason:  "the run exhausted its retry budget",
		}
	}
	if c.MaxEvents > 0 && s.Events >= c.MaxEvents {
		return &CeilingBreach{
			Ceiling: CeilingEvents,
			Limit:   fmt.Sprintf("%d events", c.MaxEvents),
			Actual:  fmt.Sprintf("%d emitted", s.Events),
			Reason:  "the run reached its event cap",
		}
	}
	if c.WallClockMs > 0 && s.WallClockMs >= c.WallClockMs {
		return &CeilingBreach{
			Ceiling: CeilingWallClock,
			Limit:   fmt.Sprintf("%dms", c.WallClockMs),
			Actual:  fmt.Sprintf("%dms", s.WallClockMs),
			Reason:  "the run reached its wall-clock ceiling",
		}
	}
	return nil
}
