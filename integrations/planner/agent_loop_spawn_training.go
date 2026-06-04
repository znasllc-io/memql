// agent_loop_spawn_training.go
//
// memql#852 Gap 2: mint a kind=trainSpecialist child Plan when the user
// approves a spawnTrainingPlan decision.
//
// The gate (agent_loop_specialist_gate.go) parks an unapproved
// spawnTrainingPlan for approval (and publishes the approval card, Gap
// 1). Once metrics.specialistApproved is set, the decision falls through
// the gate to here. We resolve the training target knowledge domain and
// mint a trainSpecialist Plan that the TrainSpecialistDispatcher (#644)
// then claims + runs -- the same mint shape spawnRefreshPlan uses for
// the cron refresh path.
//
// Domain provisioning (the #852 Gap-2 architecture decision, resolved
// 2026-06-04 in favor of "reuse the specialist's attached domain"):
// initial training targets the new specialist's PRIMARY attached
// knowledge domain. The agent factory snapshots the skill-bundle domains
// onto capabilities.domains at creation, and the Trainer seeds an empty
// domain shell inline, so this trains into the exact domain the
// specialist reads at inference -- no dispatcher contract change, no
// orphan domains. If the specialist carries no attached domain we fall
// back to minting an empty domain for the Trainer to seed.
//
// Domain ids are bare slugs throughout the system (skill bundles, the
// seed catalog, the dispatcher's input.domainId), so the resolved /
// minted id is threaded verbatim into the trainSpecialist plan's
// input.domainId.
package planner

import (
	"context"
	"fmt"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

// dispatchApprovedTrainingPlan mints a trainSpecialist child Plan for an
// already-approved spawnTrainingPlan decision, then marks the
// originating Plan succeeded (the training itself runs asynchronously on
// the child Plan).
func (l *PlannerAgentLoop) dispatchApprovedTrainingPlan(ctx context.Context, planId string, d *plannerDecision) error {
	plan, err := l.loadPlan(ctx, planId)
	if err != nil {
		return fmt.Errorf("load plan %s: %w", planId, err)
	}

	// Specialist: prefer the decision's id; fall back to the Plan's
	// ownerAgentId (stamped by the createSpecialist iteration that ran
	// just before this training request).
	specialistId := d.SpecialistId
	if specialistId == "" {
		specialistId = getString(plan, "ownerAgentId")
	}
	if specialistId == "" {
		return l.escalateAwaitingFeedback(ctx, planId, "feedback_required",
			"Training was approved, but no target specialist could be identified (the decision carried no specialistId and the Plan has no ownerAgentId).")
	}

	mode := d.Mode
	if mode == "" {
		mode = "initial"
	}
	topic := d.Topic
	if topic == "" {
		topic = getString(plan, "goal")
	}
	requestedBy := getString(plan, "requestedBy")
	spaceId := getString(plan, "spaceId")

	// Resolve the training target domain (Option 1): the specialist's
	// primary attached knowledge domain.
	domainId := l.resolvePrimarySpecialistDomain(ctx, specialistId)
	if domainId == "" {
		// Fallback: the specialist has no attached domain -- mint an
		// empty one (bare-slug id, matching the catalog convention) for
		// the Trainer to seed.
		domainId, err = l.mintTrainingDomain(ctx, requestedBy, topic)
		if err != nil {
			return l.escalateAwaitingFeedback(ctx, planId, "feedback_required",
				fmt.Sprintf("Training was approved, but no target knowledge domain could be resolved for specialist %s or created: %v", specialistId, err))
		}
	}

	// Mint the trainSpecialist child Plan the dispatcher claims + runs.
	// Mirrors spawnRefreshPlan's shape (refresh_cron.go); triggerSource
	// "planner" distinguishes it from the cron's "system" refreshes.
	trainPlanId := fmt.Sprintf("train:%s", id.NewShortId())
	goal := fmt.Sprintf("Train specialist %s on %q (mode=%s)", specialistId, topic, mode)
	input := map[string]any{
		"domainId":     domainId,
		"mode":         mode,
		"specialistId": specialistId,
		"topic":        topic,
	}
	call := fmt.Sprintf(
		`mutationCreatePlan({"planId": %q, "spaceId": %q, "kind": "trainSpecialist", "goal": %q, "requestedBy": %q, "triggerSource": "planner", "input": %s})`,
		trainPlanId, spaceId, goal, requestedBy, mustJSONObject(input),
	)
	if _, err = l.engine.Execute(systemActorContext(ctx), call); err != nil {
		return l.markPlanFailed(ctx, planId,
			fmt.Sprintf("spawnTrainingPlan: failed to mint trainSpecialist plan: %v", err))
	}
	l.logger.Info("planner agent loop: minted trainSpecialist plan on approval",
		"planId", planId, "trainPlanId", trainPlanId, "specialistId", specialistId,
		"domainId", domainId, "mode", mode, "topic", topic)

	// The originating Plan's deliverable (arrange training) is done; the
	// training runs asynchronously on the child Plan. Mark the parent
	// succeeded so the loop terminates cleanly rather than re-invoking.
	return l.markPlanSucceeded(ctx, planId, map[string]any{
		"response":     fmt.Sprintf("Approved. Spawned training plan %s for specialist %s (domain %s, mode %s).", trainPlanId, specialistId, domainId, mode),
		"trainPlanId":  trainPlanId,
		"specialistId": specialistId,
		"domainId":     domainId,
		"mode":         mode,
	})
}

// resolvePrimarySpecialistDomain returns the first knowledge domain
// attached to the specialist (capabilities.domains[0]), or "" when the
// agent can't be loaded or carries no domains.
func (l *PlannerAgentLoop) resolvePrimarySpecialistDomain(ctx context.Context, specialistId string) string {
	res, err := l.engine.Execute(systemActorContext(ctx),
		fmt.Sprintf(`queryAgentById({agentId:%q})`, specialistId))
	if err != nil {
		l.logger.Warn("planner spawnTrainingPlan: queryAgentById failed",
			"specialistId", specialistId, "error", err)
		return ""
	}
	rows := memql.MaterializeRows(res)
	if len(rows) == 0 {
		return ""
	}
	domains := agentDomains(rows[0])
	if len(domains) == 0 {
		return ""
	}
	return domains[0]
}

// agentDomains pulls capabilities.domains off an agent row, tolerating
// the capabilities sub-object living either at the flattened top level
// or nested under payload (depending on the row shape MaterializeRows
// yields).
func agentDomains(row map[string]any) []string {
	caps, _ := row["capabilities"].(map[string]any)
	if caps == nil {
		if p, ok := row["payload"].(map[string]any); ok {
			caps, _ = p["capabilities"].(map[string]any)
		}
	}
	if caps == nil {
		return nil
	}
	return planStringSlice(caps["domains"])
}

// mintTrainingDomain creates an empty, private knowledge domain owned by
// the requesting user (bare-slug id, matching the catalog convention)
// for the Trainer to seed when a specialist has no attached domain.
// Returns the bare-slug id to thread into input.domainId.
func (l *PlannerAgentLoop) mintTrainingDomain(ctx context.Context, ownerUserId, topic string) (string, error) {
	slug := id.Slugify(topic)
	if slug == "" {
		slug = "specialist-training"
	}
	domainId := fmt.Sprintf("%s-%s", slug, id.NewShortId())
	name := topic
	if name == "" {
		name = "Specialist training"
	}
	call := fmt.Sprintf(
		`mutationCreateKnowledgeDomain({domainId:%q, name:%q, scope:"private", ownerId:%q, source:"llmSeeded"})`,
		domainId, name, ownerUserId,
	)
	if _, err := l.engine.Execute(systemActorContext(ctx), call); err != nil {
		return "", err
	}
	l.logger.Info("planner spawnTrainingPlan: minted fallback training domain",
		"domainId", domainId, "ownerUserId", ownerUserId, "topic", topic)
	return domainId, nil
}

// planStringSlice converts an any holding a []string / []any-of-strings
// into []string, dropping non-string / empty elements. (Local to the
// planner package; the training integration has its own toStringSlice.)
func planStringSlice(v any) []string {
	switch s := v.(type) {
	case []string:
		return s
	case []any:
		out := make([]string, 0, len(s))
		for _, e := range s {
			if str, ok := e.(string); ok && str != "" {
				out = append(out, str)
			}
		}
		return out
	default:
		return nil
	}
}
