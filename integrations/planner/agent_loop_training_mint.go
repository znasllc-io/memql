// agent_loop_training_mint.go
//
// memql#852 Gap 2: mint the kind=trainSpecialist child Plan when the
// user APPROVES a spawnTrainingPlan decision.
//
// The specialist/training gate (#842) parks an unapproved
// spawnTrainingPlan for approval; on approval (metrics.specialistApproved)
// the gate lets the action through, and this handler turns the approved
// decision into an actual training run. The TrainSpecialistDispatcher
// (#644) claims + runs the minted Plan.
//
// The dispatcher hard-requires input.domainId, but the planner's
// spawnTrainingPlan decision carries only {specialistId, topic, mode}.
// Per the #852 design decision, the target domain for initial training is
// the specialist's PRIMARY ATTACHED knowledge domain (resolved from its
// skills -- the agent row stores skillIds as the capability source of
// truth, #158). When no domain can be resolved we escalate for feedback
// rather than minting a Plan the dispatcher would reject.
package planner

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

// mintApprovedTrainingPlan handles an approved spawnTrainingPlan decision.
// It resolves the training target domain, mints a kind=trainSpecialist
// child Plan (which the dispatcher then runs), and marks the parent Plan
// succeeded with a pointer to the dispatched training Plan so the loop
// doesn't re-emit the decision.
func (l *PlannerAgentLoop) mintApprovedTrainingPlan(ctx context.Context, planId string, d plannerDecision) error {
	plan, err := l.loadPlan(ctx, planId)
	if err != nil {
		return err
	}

	specialistId := d.SpecialistId
	if specialistId == "" {
		l.logger.Warn("planner agent loop: spawnTrainingPlan missing specialistId; escalating",
			"planId", planId)
		return l.escalateAwaitingFeedback(ctx, planId, "feedback_required",
			"Training was approved, but the planner did not name a specialist to train. Manual intervention required.")
	}

	domainId := l.resolveSpecialistPrimaryDomain(ctx, specialistId)
	if domainId == "" {
		l.logger.Warn("planner agent loop: no attached domain for specialist; escalating",
			"planId", planId, "specialistId", specialistId)
		return l.escalateAwaitingFeedback(ctx, planId, "feedback_required",
			"Training was approved, but the specialist has no attached knowledge domain to train into. Attach a domain (or create the specialist first) and retry.")
	}

	mode := d.Mode
	if mode == "" {
		mode = "initial"
	}
	topic := d.Topic
	if topic == "" {
		topic = getString(plan, "goal")
	}

	input := map[string]any{
		"domainId":     domainId,
		"specialistId": specialistId,
		"topic":        topic,
		"mode":         mode,
	}
	inputJSON, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("marshal trainSpecialist input: %w", err)
	}

	// Deterministic child id keyed on the approving Plan -- one training
	// run per approval (idempotent if the loop re-dispatches the same
	// approved decision). SystemNodeShortId stays deterministic (no
	// uniqueness token) so the idempotency holds, while guaranteeing a
	// colon-free bare id the engine accepts (issue #1712).
	trainPlanId := id.SystemNodeShortId("train", planId)
	goal := fmt.Sprintf("Train specialist on %q (user-approved)", topic)
	call := fmt.Sprintf(
		`createPlan({"planId": %q, "partitionId": %q, "parentPlanId": %q, "kind": "trainSpecialist", "goal": %q, "requestedBy": %q, "triggerSource": "user.approved", "input": %s})`,
		trainPlanId, getString(plan, "partitionId"), planId, goal,
		getString(plan, "requestedBy"), string(inputJSON),
	)
	if _, err := l.engine.Execute(systemActorContext(ctx), call); err != nil {
		return fmt.Errorf("mint trainSpecialist plan: %w", err)
	}
	l.logger.Info("planner agent loop: minted trainSpecialist plan on approval",
		"planId", planId, "trainPlanId", trainPlanId, "specialistId", specialistId,
		"domainId", domainId, "mode", mode)

	// The parent Plan's job -- decide + trigger training -- is complete;
	// the training itself runs as the dispatched child. Terminate the
	// parent so the loop doesn't re-emit spawnTrainingPlan.
	return l.markPlanSucceeded(ctx, planId, map[string]any{
		"trainingPlanId": trainPlanId,
		"specialistId":   specialistId,
		"domainId":       domainId,
		"mode":           mode,
	})
}

// resolveSpecialistPrimaryDomain returns the first knowledge-domain id
// attached to the specialist via its skills, or "" when none resolves.
// The agent row carries skillIds (the capability source of truth, #158);
// effective domains come from unioning those skills' bundles. We read the
// specialist's skillIds, then walk the active skill catalog and return the
// first domain in skillIds order.
func (l *PlannerAgentLoop) resolveSpecialistPrimaryDomain(ctx context.Context, specialistId string) string {
	agentRows := l.execRows(ctx, fmt.Sprintf(`agentById({agentId:%q})`, specialistId))
	if len(agentRows) == 0 {
		return ""
	}
	caps, _ := agentRows[0]["capabilities"].(map[string]any)
	if caps == nil {
		return ""
	}
	skillIds := stringSliceFromAny(caps["skillIds"])
	if len(skillIds) == 0 {
		return ""
	}

	domainsBySkill := make(map[string][]string)
	for _, s := range l.execRows(ctx, `activeSkillsFull({})`) {
		domainsBySkill[getString(s, "id")] = stringSliceFromAny(s["domainIds"])
	}
	for _, sid := range skillIds {
		for _, dom := range domainsBySkill[sid] {
			if dom != "" {
				return dom
			}
		}
	}
	return ""
}

// execRows runs a read query and materializes its rows, logging + swallowing
// errors (callers treat an empty result as "not resolvable").
func (l *PlannerAgentLoop) execRows(ctx context.Context, query string) []map[string]any {
	res, err := l.engine.Execute(systemActorContext(ctx), query)
	if err != nil {
		l.logger.Warn("planner agent loop: query failed", "query", query, "error", err)
		return nil
	}
	return memql.MaterializeRows(res)
}
