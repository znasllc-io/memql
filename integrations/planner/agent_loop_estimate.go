// agent_loop_estimate.go
//
// Up-front token-cost estimation + a user-approval threshold gate for a
// Plan before it spends a single planner LLM call (epic #836 / #839).
//
// The Plan concept already carries `estimate` / `tokenBudget`; this
// wires a REAL up-front estimate and an approval gate on top:
//
//   - Before the decompose loop runs, estimate the Plan's token cost
//     (expected planner calls x per-call size + per-phase execution).
//   - Stamp the estimate as the Plan's tokenBudget when one isn't
//     already set (so the #819 cumulative cap has a real, plan-specific
//     ceiling instead of only the generous global fallback).
//   - If the estimate exceeds a configurable threshold, PARK the Plan as
//     awaitingFeedback(budget_approval_required) -- NO planner LLM call
//     is made until the user approves. Below the threshold, auto-run.
//
// "Approved" is recognized via metrics.budgetApproved (set by the
// frontend Approve action on resume) or tokenCapDisabled (an explicit
// "no budget limits" escape hatch), so a re-entry after approval doesn't
// re-park. The estimate + gate are pure functions so the contract is
// unit-testable without an engine.
package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"
)

const (
	// defaultPlanApprovalTokenThreshold is the up-front estimate (in
	// tokens) at or below which a Plan auto-runs; above it the Plan parks
	// for user approval. Chosen generous enough that ordinary plans
	// auto-run -- a trivial deliverable never enters this loop at all
	// (the #837 triage shortcuts it), and a normal moderate plan
	// estimates well under this -- but low enough that a big multi-phase
	// program is surfaced for an explicit go-ahead before it spends.
	// Override with MEMQL_PLANNER_APPROVAL_TOKEN_THRESHOLD; 0 disables the
	// gate (every plan auto-runs).
	defaultPlanApprovalTokenThreshold = 250_000

	// expectedPlannerCallsBase is the assumed number of plannerAgent
	// calls for a Plan with no phase outline yet (decompose + a couple of
	// dispatch/createSpecialist + completion). Phase-bearing plans scale
	// from their phase count instead.
	expectedPlannerCallsBase = 4

	// perPhaseExecTokenEstimate approximates the execution-side token
	// cost contributed by one phase (the agent turns that carry it out),
	// folded into the up-front estimate so a many-phase program reads as
	// more expensive than a one-phase one.
	perPhaseExecTokenEstimate = 40_000
)

// planApprovalTokenThreshold returns the approval threshold, honoring the
// env override. <= 0 means the gate is disabled (auto-run everything).
func planApprovalTokenThreshold() int {
	if v := os.Getenv("MEMQL_PLANNER_APPROVAL_TOKEN_THRESHOLD"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return defaultPlanApprovalTokenThreshold
}

// estimatePlanCostTokens computes a deterministic up-front token estimate
// for a Plan from the fields available at first entry: the goal length
// and the phase outline (when the planner has already stamped one). It is
// intentionally a heuristic -- InvokeAI returns no usage stats and the
// exact path isn't known up front -- but it is monotone: a longer goal or
// more phases always estimates higher, which is what the approval
// threshold keys on.
//
//	estimate = expectedCalls * (systemPromptOverhead + goalTokens)
//	           + phaseCount * perPhaseExecTokenEstimate
//
// expectedCalls scales with the phase count when phases are present
// (decompose already happened), else the no-outline base.
func estimatePlanCostTokens(plan map[string]any) int {
	goal := getString(plan, "goal")
	goalTokens := len(goal) / plannerCharsPerTokenEstimate

	phaseCount := 0
	if phases, ok := plan["phases"].([]any); ok {
		phaseCount = len(phases)
	}

	expectedCalls := expectedPlannerCallsBase
	if phaseCount > 0 {
		// decompose is done; expect roughly one planner decision per
		// phase plus a completion turn, never fewer than the base.
		expectedCalls = phaseCount + 1
		if expectedCalls < expectedPlannerCallsBase {
			expectedCalls = expectedPlannerCallsBase
		}
	}

	perCall := plannerSystemPromptTokenEstimate + goalTokens
	return expectedCalls*perCall + phaseCount*perPhaseExecTokenEstimate
}

// planAlreadyApproved reports whether the Plan has been explicitly
// approved to spend past the threshold: either the frontend set
// metrics.budgetApproved=true on the Approve action, or the Plan carries
// the tokenCapDisabled escape hatch.
func planAlreadyApproved(plan map[string]any) bool {
	if asBool(plan["tokenCapDisabled"]) {
		return true
	}
	if metrics, ok := plan["metrics"].(map[string]any); ok {
		if asBool(metrics["budgetApproved"]) {
			return true
		}
	}
	return false
}

// planApprovalResult is the verdict from evaluatePlanApprovalGate.
type planApprovalResult struct {
	EstimateTokens int
	Blocked        bool
	Message        string
}

// evaluatePlanApprovalGate is the PURE approval decision. The Plan is
// blocked (parked for approval) only when the gate is enabled
// (threshold > 0), the Plan is NOT already approved, and the estimate
// strictly exceeds the threshold. Deterministic + side-effect-free.
func evaluatePlanApprovalGate(plan map[string]any, estimate, threshold int) planApprovalResult {
	res := planApprovalResult{EstimateTokens: estimate}
	if threshold <= 0 {
		return res // gate disabled -> auto-run
	}
	if planAlreadyApproved(plan) {
		return res // user already said go
	}
	if estimate > threshold {
		res.Blocked = true
		res.Message = fmt.Sprintf(
			"This plan's estimated cost (~%d tokens) exceeds the auto-run threshold (%d). It is parked awaiting your approval and has made NO model calls. Approve to run it, or decline to cancel.",
			estimate, threshold)
	}
	return res
}

// gatePlanApprovalIfNeeded runs the up-front estimate + approval gate for
// a Plan before the decompose loop. It stamps the estimate as the Plan's
// tokenBudget (when unset) and tokenSpent-relative metrics, and -- if the
// estimate exceeds the threshold and the Plan isn't already approved --
// parks the Plan to awaitingFeedback(budget_approval_required), returning
// handled=true so the caller makes NO planner LLM call.
//
// Like the #837 triage, a load failure never strands the Plan: it returns
// (false, nil) and the caller proceeds to the (separately-capped) loop.
func (l *PlannerAgentLoop) gatePlanApprovalIfNeeded(ctx context.Context, planId string) (handled bool, err error) {
	threshold := planApprovalTokenThreshold()
	if threshold <= 0 {
		return false, nil // gate disabled
	}
	plan, lerr := l.loadPlan(ctx, planId)
	if lerr != nil {
		return false, nil
	}
	estimate := estimatePlanCostTokens(plan)
	gate := evaluatePlanApprovalGate(plan, estimate, threshold)

	// Stamp the estimate as the per-plan token budget when the Plan
	// doesn't already carry one, so the #819 cumulative cap binds on a
	// real plan-specific number. Don't clobber an explicit budget.
	if asInt(plan["tokenBudget"]) == 0 {
		l.stampPlanTokenBudget(ctx, planId, estimate)
	}

	if !gate.Blocked {
		return false, nil
	}
	l.logger.Info("planner approval gate: estimate over threshold; parking for approval (no LLM call)",
		"planId", planId, "estimateTokens", estimate, "threshold", threshold)
	if perr := l.parkForBudgetApproval(ctx, planId, gate.Message, estimate); perr != nil {
		// Parking failed -- do NOT fall through to spend; report the
		// error so the safety net marks the plan rather than running it.
		return true, perr
	}
	return true, nil
}

// buildStampTokenBudgetQuery serializes the tokenBudget stamp mutation
// from a map via json.Marshal rather than hand-concatenating the object
// literal.
//
// A bare `tokenBudget:%d` field (unquoted key, bare numeric value, no
// space) is mis-lexed by the engine DSL: the lexer folds the colon into
// the identifier, so `{tokenBudget:300000}` tokenizes as a single
// identifier `tokenBudget:300000` with no value -- the parser then fails
// with `expected ':' or '=' after object key, got "}"` (#1108).
// json.Marshal emits a quoted key (`"tokenBudget":300000`); the closing
// quote terminates the identifier scan, so the object is always
// well-formed regardless of the budget value.
func buildStampTokenBudgetQuery(planId string, budget int) (string, error) {
	argsJSON, err := json.Marshal(map[string]any{
		"planId":      planId,
		"status":      "planning",
		"tokenBudget": budget,
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`mutationUpdatePlanStatus(%s)`, string(argsJSON)), nil
}

// stampPlanTokenBudget sets Plan.tokenBudget (best-effort).
func (l *PlannerAgentLoop) stampPlanTokenBudget(ctx context.Context, planId string, budget int) {
	q, err := buildStampTokenBudgetQuery(planId, budget)
	if err != nil {
		l.logger.Warn("planner approval gate: stamp tokenBudget marshal failed", "planId", planId, "error", err)
		return
	}
	if _, err := l.engine.Execute(systemActorContext(ctx), q); err != nil {
		l.logger.Warn("planner approval gate: stamp tokenBudget failed", "planId", planId, "error", err)
	}
}

// parkForBudgetApproval transitions the Plan to
// awaitingFeedback(budget_approval_required) with the estimate carried in
// the feedbackRequest so the canvas card can render the number + an
// Approve/Decline control.
func (l *PlannerAgentLoop) parkForBudgetApproval(ctx context.Context, planId, message string, estimate int) error {
	fbReq := map[string]any{
		"question":       message,
		"kind":           "choice",
		"estimateTokens": estimate,
		"options": []map[string]any{
			{"label": "Approve & run", "value": "approve"},
			{"label": "Decline", "value": "decline"},
		},
		"askedAt": time.Now().UTC().Format(time.RFC3339),
	}
	fbReqJSON, err := json.Marshal(fbReq)
	if err != nil {
		fbReqJSON = []byte(`{}`)
	}
	q := fmt.Sprintf(
		`mutationUpdatePlanStatus({planId:%q, status:"awaitingFeedback", feedbackReason:"budget_approval_required", feedbackRequest:%s})`,
		planId, string(fbReqJSON),
	)
	_, err = l.engine.Execute(systemActorContext(ctx), q)
	return err
}
