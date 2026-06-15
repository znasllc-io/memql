package memql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// planner_feedback_validation.go enforces the generic feedback-intake
// transition for v1:planner:plan rows at the engine pre-insert boundary
// (epic memql#1404 / child memql#1405). The append-only DSL cannot
// express "reject this resume unless the Plan is currently awaitingFeedback
// AND the actor owns it" on its own, so the rule lives in Go -- mirroring
// the harness step-transition guard (harness_step_validation.go) and the
// agent-kind actor-scope guard (agent_kind_actor_validation.go).
//
// It is scoped to the INTAKE write only: a write to v1:planner:plan that
// stamps a feedbackResponse carrying a respondedBy (the shape
// mutationAttachPlanFeedback produces). Every other plan write -- the
// planner agent loop's status transitions, the scope-elevation
// Allow/Deny path (which sets status=succeeded/cancelled, not running, and
// does not stamp respondedBy), the admission controller, etc. -- carries no
// such marker and passes untouched.
//
// The transition rule itself is a pure, table-testable function
// (feedbackIntakeAllowed) so the ownership + prior-status matrix can be
// unit-tested without a database, like stepTransitionAllowed.

const conceptPlannerPlan = "v1:planner:plan"

// planStatusAwaitingFeedback is the only prior status from which a generic
// feedback-intake resume is legal. Kept in one place so the guard and the
// concept enum (dsl/planner/concepts.memql:plan.status) stay in lockstep.
const planStatusAwaitingFeedback = "awaitingFeedback"

// isFeedbackIntakeWrite reports whether a v1:planner:plan payload is the
// product of mutationAttachPlanFeedback -- i.e. it stamps a feedbackResponse
// object carrying a non-empty respondedBy. That marker distinguishes the
// generic free-text intake resume from every other plan write (the scope-
// elevation Allow/Deny path sets feedbackResponse.response without going
// through this mutation and lands status=succeeded/cancelled, so gating on
// respondedBy + status=running keeps this guard from touching it).
func isFeedbackIntakeWrite(payload map[string]any) bool {
	fr, ok := payload["feedbackResponse"].(map[string]any)
	if !ok {
		return false
	}
	return strings.TrimSpace(stringFromAny(fr["respondedBy"])) != ""
}

// feedbackIntakeAllowed reports whether a feedback-intake resume is legal
// given the prior Plan status and whether the actor owns the Plan. Pure
// logic, split out so the gate is unit-testable without a database. Rules:
//
//   - the Plan MUST currently be in awaitingFeedback (you can only answer a
//     question that was asked; resuming a running / terminal / queued Plan
//     via the intake path is a no-op at best and a race at worst);
//   - the actor MUST own the Plan (a privileged caller -- cluster owner or a
//     system actor such as the cognition assistant-mediated path -- is also
//     allowed; ownerOrPrivileged folds those in).
//
// priorStatus == "" means there is no prior row (the Plan does not exist);
// the intake path can only ever resume an existing parked Plan, so that is
// rejected.
func feedbackIntakeAllowed(priorStatus string, ownerOrPrivileged bool) bool {
	if strings.TrimSpace(priorStatus) != planStatusAwaitingFeedback {
		return false
	}
	return ownerOrPrivileged
}

// validateFeedbackIntakeTransition runs the engine pre-insert guard for the
// generic feedback-intake resume (mutationAttachPlanFeedback). It is a no-op
// for any plan write that is not an intake write (no feedbackResponse with a
// respondedBy). For an intake write it loads the prior plan row and enforces
// feedbackIntakeAllowed.
//
// Called from executor.executeInsert; not part of the public API.
func (e *MemQLEngine) validateFeedbackIntakeTransition(ctx context.Context, payload map[string]any, planID string, actor string) error {
	if payload == nil || !isFeedbackIntakeWrite(payload) {
		return nil
	}

	planID = strings.TrimSpace(planID)
	prior, err := e.getLatestPlanPayload(ctx, planID)
	if err != nil {
		return err
	}
	if prior == nil {
		return fmt.Errorf(
			"v1:planner:plan: cannot attach feedback to a non-existent Plan %q", planID)
	}

	priorStatus := strings.TrimSpace(stringFromAny(prior["status"]))
	owner := strings.TrimSpace(stringFromAny(prior["requestedBy"]))

	if !feedbackIntakeAllowed(priorStatus, e.actorOwnsOrPrivileged(ctx, owner, actor)) {
		if priorStatus != planStatusAwaitingFeedback {
			return fmt.Errorf(
				"v1:planner:plan: feedback can only be attached to a Plan in %q (Plan %q is %q)",
				planStatusAwaitingFeedback, planID, priorStatus)
		}
		return fmt.Errorf(
			"v1:planner:plan: only the Plan owner (or a privileged caller) may attach feedback to Plan %q", planID)
	}
	return nil
}

// actorOwnsOrPrivileged reports whether the caller may attach feedback to a
// Plan owned by ownerUserId. True when:
//   - the caller is the owner (AccessContext.UserId == ownerUserId, or the
//     raw actor string matches -- covers the system:planner dispatch that
//     impersonates the owner, and dev mode where the access context is thin);
//   - the caller is a cluster owner (admin override);
//   - the caller is a system actor (role=="system" or actor begins with
//     "system:") -- the assistant-mediated chat path (#1406) runs the intake
//     mutation under the cognition system actor on the user's behalf.
//
// Fails closed: an empty owner on the prior row (should never happen --
// requestedBy is @required) is only writable by a privileged caller.
func (e *MemQLEngine) actorOwnsOrPrivileged(ctx context.Context, ownerUserId, actor string) bool {
	ownerUserId = strings.TrimSpace(ownerUserId)
	actor = strings.TrimSpace(actor)

	identity, _ := auth.UserIdentityFromContext(ctx)
	if isSystemActor(identity, actor) {
		return true
	}
	if ac, ok := auth.AccessFromContext(ctx); ok && ac != nil {
		if ac.IsClusterOwner() {
			return true
		}
		if uid := strings.TrimSpace(ac.UserId); uid != "" && uid == ownerUserId {
			return true
		}
	}
	if ownerUserId != "" && actor == ownerUserId {
		return true
	}
	return false
}

// getLatestPlanPayload loads the latest version of a v1:planner:plan row by
// id. Returns (nil, nil) when no row exists yet. Mirrors
// getLatestHarnessStepPayload -- the planner concept is not in the
// filesystem-concept const list, so the concept id is the literal string.
func (e *MemQLEngine) getLatestPlanPayload(ctx context.Context, planID string) (map[string]any, error) {
	if e == nil {
		return nil, fmt.Errorf("engine is nil")
	}
	if planID == "" {
		return nil, nil
	}
	db := e.database()
	if db == nil {
		return nil, fmt.Errorf("memory engine database not configured")
	}

	var node memorynodes.MemoryNode
	err := db.NewSelect().
		Model(&node).
		Where("id = ?", planID).
		Where("concept = ?", conceptPlannerPlan).
		OrderExpr(`"createdAt" DESC`).
		Limit(1).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("load plan %q: %w", planID, err)
	}

	payload := make(map[string]any)
	if jerr := json.Unmarshal(node.Payload, &payload); jerr != nil {
		return nil, fmt.Errorf("decode plan %q payload: %w", planID, jerr)
	}
	return payload, nil
}
