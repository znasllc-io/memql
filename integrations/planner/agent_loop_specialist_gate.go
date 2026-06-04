// agent_loop_specialist_gate.go
//
// Gate on specialist creation + training (epic #836 / memql#842).
//
// Creating or extending a specialist, and especially spawning a training
// plan, is expensive: the agent factory runs a structured-output
// analysis, and a trainSpecialist plan runs an Opus trainer turn + web
// fetches + embeddings. None of that should happen for a one-off
// deliverable -- "create a list of 10 birds" must complete with an
// EXISTING agent and zero training. Specialist creation / training is
// reserved for durable, REUSED capabilities and must be gated.
//
// The gate is a pure decision keyed on the Plan + the planner's emitted
// action:
//
//   - spawnTrainingPlan  -> ALWAYS gated. Training never happens
//                           automatically; it requires explicit user
//                           approval (metrics.specialistApproved) or the
//                           tokenCapDisabled escape hatch.
//   - createSpecialist /  -> gated for ONE-OFF plan kinds (produceArtifact,
//     extendSpecialist       adHocAction) -- those must finish with an
//                            existing agent. For other (genuine
//                            multi-step) plans it's allowed, since a real
//                            program may legitimately need a specialist.
//
// When gated and not approved, the loop parks the Plan to
// awaitingFeedback(specialist_approval_required) instead of running the
// factory / trainer -- so a trivial deliverable triggers no creation and
// no training.
package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// oneOffPlanKinds are Plan kinds that are inherently single-deliverable /
// non-durable: they must complete with an existing agent and NEVER
// auto-create or train a specialist.
var oneOffPlanKinds = map[string]bool{
	produceArtifactPlanKind: true,
	"adHocAction":           true,
}

// specialistGateEnabled gates the whole guard. Defaults on; an operator
// can disable it (MEMQL_PLANNER_SPECIALIST_GATE_ENABLED=0) to restore the
// old always-allow behavior.
func specialistGateEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("MEMQL_PLANNER_SPECIALIST_GATE_ENABLED"))) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// planSpecialistApproved reports whether the user has explicitly approved
// specialist creation / training for this Plan (the frontend Approve
// action sets metrics.specialistApproved), or the tokenCapDisabled
// escape hatch is set.
func planSpecialistApproved(plan map[string]any) bool {
	if asBool(plan["tokenCapDisabled"]) {
		return true
	}
	if metrics, ok := plan["metrics"].(map[string]any); ok {
		if asBool(metrics["specialistApproved"]) {
			return true
		}
	}
	return false
}

// specialistGateResult is the verdict from evaluateSpecialistGate.
type specialistGateResult struct {
	Blocked bool
	Message string
}

// evaluateSpecialistGate is the PURE decision for whether a given
// specialist/training action is allowed for this Plan. Deterministic +
// side-effect-free so the gate contract is unit-testable.
//
//	action in {createSpecialist, extendSpecialist, spawnTrainingPlan}.
//
// Returns Blocked=true (park for approval) when:
//   - the gate is enabled, AND the Plan isn't already approved, AND
//   - action == spawnTrainingPlan (always gated -- training is never
//     automatic), OR
//   - action is create/extendSpecialist AND the Plan is a one-off kind.
func evaluateSpecialistGate(plan map[string]any, action string, gateEnabled bool) specialistGateResult {
	if !gateEnabled {
		return specialistGateResult{}
	}
	if planSpecialistApproved(plan) {
		return specialistGateResult{}
	}

	kind := getString(plan, "kind")
	switch action {
	case "spawnTrainingPlan":
		return specialistGateResult{
			Blocked: true,
			Message: "This plan asked to train a specialist (an expensive operation: a reasoning trainer turn plus web fetches and embeddings). Training is reserved for durable, reused capabilities and is never run automatically. Approve to create + train the specialist, or decline to continue with an existing agent.",
		}
	case "createSpecialist", "extendSpecialist":
		if oneOffPlanKinds[kind] {
			return specialistGateResult{
				Blocked: true,
				Message: fmt.Sprintf(
					"This is a one-off deliverable (%s); it should complete with an existing agent rather than creating a new specialist. Approve if you want a durable specialist created for reuse, or decline to finish with an existing agent.",
					kind),
			}
		}
		return specialistGateResult{} // genuine multi-step plan -> allowed
	default:
		return specialistGateResult{} // unknown action -> not our gate
	}
}

// gateSpecialistAction runs the gate for a specialist/training action and,
// when blocked, parks the Plan to awaitingFeedback(specialist_approval_required),
// returning handled=true so the caller does NOT run the factory / trainer.
func (l *PlannerAgentLoop) gateSpecialistAction(ctx context.Context, planId, action string) (handled bool, err error) {
	plan, lerr := l.loadPlan(ctx, planId)
	if lerr != nil {
		// Can't classify -> don't block (the loop's other caps still apply).
		return false, nil
	}
	gate := evaluateSpecialistGate(plan, action, specialistGateEnabled())
	if !gate.Blocked {
		return false, nil
	}
	l.logger.Info("planner specialist gate: parking for approval (no factory / trainer run)",
		"planId", planId, "action", action, "kind", getString(plan, "kind"))
	return true, l.parkForSpecialistApproval(ctx, planId, gate.Message)
}

// parkForSpecialistApproval transitions the Plan to
// awaitingFeedback(specialist_approval_required) with the message in the
// feedbackRequest so the canvas card can render an Approve/Decline.
func (l *PlannerAgentLoop) parkForSpecialistApproval(ctx context.Context, planId, message string) error {
	fbReq := map[string]any{
		"question": message,
		"kind":     "choice",
		"options": []map[string]any{
			{"label": "Approve & create", "value": "approve"},
			{"label": "Decline (use existing agent)", "value": "decline"},
		},
		"askedAt": time.Now().UTC().Format(time.RFC3339),
	}
	fbReqJSON, err := json.Marshal(fbReq)
	if err != nil {
		fbReqJSON = []byte(`{}`)
	}
	q := fmt.Sprintf(
		`mutationUpdatePlanStatus({planId:%q, status:"awaitingFeedback", feedbackReason:"specialist_approval_required", feedbackRequest:%s})`,
		planId, string(fbReqJSON),
	)
	_, err = l.engine.Execute(systemActorContext(ctx), q)
	return err
}
