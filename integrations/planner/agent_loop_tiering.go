// agent_loop_tiering.go
//
// Model tiering for the Planner Agent loop (epic #836 / memql#838).
//
// The cost bomb was running the planner on Opus + extended thinking
// ($15/$75 per 1M PLUS thinking tokens) for EVERY decision. The fix is
// two-fold:
//
//  1. CHEAP BY DEFAULT. The plannerAgent prompt's @defaultProvider is a
//     cheap chat tier (streamClaudeSonnet, no extended thinking) and the
//     per-user Planner agent's providerConfig no longer pins
//     reasoningClaudeOpus. Routine routing / decompose / dispatch steps
//     run cheap.
//
//  2. ESCALATE ON NEED, NOT BY DEFAULT. Reasoning (reasoningClaudeOpus =
//     Opus + an 8192-token thinking budget) is reserved for genuinely
//     hard planning and is invoked ONLY when the cheap tier hasn't
//     converged after a few iterations in a cycle. selectPlannerProvider
//     is the pure, deterministic, unit-tested decision: below the
//     escalation iteration it returns ("", false) -> no override -> the
//     prompt's cheap default is used; at/after it returns the reasoning
//     provider + reasoning=true and the loop wraps the SI context with a
//     provider override for that call.
//
// Because a trivial/moderate plan converges in 1-2 iterations (and a
// trivial deliverable never even enters this loop -- see the #837
// triage), the escalation threshold means such plans make ZERO
// Opus+thinking calls. Only a plan that keeps churning past the
// threshold pays for reasoning, and even then it is bounded by the
// per-cycle iteration cap + the cumulative per-plan ceiling (#819).
package planner

import (
	"os"
	"strconv"
	"strings"
)

const (
	// defaultReasoningEscalationIter is the per-cycle iteration at which
	// the loop escalates from the cheap tier to the reasoning tier. With
	// the convergence guard parking on the 3rd identical decision and the
	// per-cycle cap at maxPlannerIterations, escalating at iter 3 gives
	// the reasoning model a shot at a genuinely-hard plan AFTER the cheap
	// tier has tried a few times -- without paying for reasoning on the
	// common 1-2 iteration path. Override with
	// MEMQL_PLANNER_REASONING_ESCALATION_ITER.
	defaultReasoningEscalationIter = 3

	// defaultPlannerReasoningProvider is the provider the loop escalates
	// TO. Opus + extended thinking. Override with
	// MEMQL_PLANNER_REASONING_PROVIDER (e.g. to disable escalation
	// entirely, point it at the cheap provider, or to a different
	// reasoning model). An empty / whitespace override is ignored.
	defaultPlannerReasoningProvider = "reasoningClaudeOpus"
)

// reasoningEscalationIter returns the iteration threshold, honoring the
// env override. A value <= 0 disables escalation (the loop stays on the
// cheap tier for every iteration, however long it runs).
func reasoningEscalationIter() int {
	if v := strings.TrimSpace(os.Getenv("MEMQL_PLANNER_REASONING_ESCALATION_ITER")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return defaultReasoningEscalationIter
}

// plannerReasoningProvider returns the provider name the loop escalates
// to, honoring the env override.
func plannerReasoningProvider() string {
	if v := strings.TrimSpace(os.Getenv("MEMQL_PLANNER_REASONING_PROVIDER")); v != "" {
		return v
	}
	return defaultPlannerReasoningProvider
}

// selectPlannerProvider is the PURE tier-selection decision for one
// plannerAgent call at the given per-cycle iteration. It returns:
//
//   - ("", false)            -> stay on the cheap tier (no provider
//                               override; the prompt's @defaultProvider
//                               applies). This is the path for iter 0..N-1.
//   - (reasoningProvider, true) -> escalate to the reasoning tier for
//                               this call (iter >= threshold).
//
// Escalation is disabled (always cheap) when the threshold is <= 0.
// Deterministic + side-effect-free so the tiering contract is
// unit-testable without an engine or an LLM.
func selectPlannerProvider(iter int) (providerOverride string, reasoning bool) {
	threshold := reasoningEscalationIter()
	if threshold <= 0 {
		return "", false
	}
	if iter < threshold {
		return "", false
	}
	return plannerReasoningProvider(), true
}
