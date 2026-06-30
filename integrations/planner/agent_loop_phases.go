// agent_loop_phases.go
//
// Phased execution with per-phase checkpoints (epic #836 / memql#840).
//
// A complex goal decomposes into a Plan.phases[] outline. Rather than
// firing every phase at once, the planner runs phase-by-phase and parks
// at a phase boundary so the user can review progress + spend-so-far and
// approve continuing -- but ONLY when the next phase is non-trivial.
// Trivial next phases auto-continue so the user isn't nagged.
//
// The decision is a PURE state machine over the phases array (each phase
// is {status: pending|active|done, expectedTaskCount, completedTaskCount,
// ...}); the loop consumes it and parks the Plan to
// awaitingFeedback(phase_checkpoint) at a boundary. Kept pure so the
// state machine is unit-testable without an engine.
package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"
)

// phaseView is the decision-relevant projection of one Plan.phases entry.
type phaseView struct {
	Status             string
	ExpectedTaskCount  int
	CompletedTaskCount int
}

// phaseAction is the state machine's verdict.
type phaseAction string

const (
	// phaseActionContinue: start / keep running the active (or next)
	// phase without parking.
	phaseActionContinue phaseAction = "continue"
	// phaseActionCheckpoint: a phase just completed and the NEXT phase is
	// non-trivial -- park for user approval before continuing.
	phaseActionCheckpoint phaseAction = "checkpoint"
	// phaseActionDone: all phases complete.
	phaseActionDone phaseAction = "done"
)

const (
	// defaultPhaseCheckpointMinTasks is the expectedTaskCount at/above
	// which the NEXT phase is considered "non-trivial" and triggers a
	// checkpoint. A next phase with fewer expected tasks auto-continues
	// so the user isn't nagged at every tiny boundary. Override with
	// MEMQL_PLANNER_PHASE_CHECKPOINT_MIN_TASKS; 0 means checkpoint at
	// EVERY boundary, a very large value disables checkpoints.
	defaultPhaseCheckpointMinTasks = 2
)

func phaseCheckpointMinTasks() int {
	if v := os.Getenv("MEMQL_PLANNER_PHASE_CHECKPOINT_MIN_TASKS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return defaultPhaseCheckpointMinTasks
}

// phaseComplete reports whether a phase has finished all its expected
// tasks. A phase explicitly marked "done" is complete; otherwise a phase
// with a positive expectedTaskCount whose completedTaskCount has caught
// up is complete.
func phaseComplete(p phaseView) bool {
	if p.Status == "done" {
		return true
	}
	return p.ExpectedTaskCount > 0 && p.CompletedTaskCount >= p.ExpectedTaskCount
}

// evaluatePhaseProgress is the PURE phase-checkpoint state machine. Given
// the ordered phases and the "non-trivial" threshold, it returns:
//
//   - phaseActionDone               when every phase is complete.
//   - phaseActionCheckpoint, nextIdx when the first incomplete phase
//     follows a run of complete phases AND that next phase is non-trivial
//     (expectedTaskCount >= minTasks) -- the loop should park here.
//   - phaseActionContinue, idx       otherwise (the active/next phase is
//     trivial enough to auto-run, or work is mid-phase).
//
// "park at the boundary" means: at least one phase is already complete
// (we just crossed a boundary) and the next phase hasn't started
// (completedTaskCount == 0). That distinguishes a fresh boundary from
// mid-phase progress.
func evaluatePhaseProgress(phases []phaseView, minTasks int) (action phaseAction, nextPhaseIndex int) {
	if len(phases) == 0 {
		return phaseActionContinue, 0
	}
	// Find the first incomplete phase.
	firstIncomplete := -1
	for i, p := range phases {
		if !phaseComplete(p) {
			firstIncomplete = i
			break
		}
	}
	if firstIncomplete == -1 {
		return phaseActionDone, len(phases)
	}
	// A boundary is "fresh" when a previous phase is complete and the
	// next phase hasn't begun any tasks yet.
	atFreshBoundary := firstIncomplete > 0 && phases[firstIncomplete].CompletedTaskCount == 0
	if atFreshBoundary && phases[firstIncomplete].ExpectedTaskCount >= minTasks {
		return phaseActionCheckpoint, firstIncomplete
	}
	return phaseActionContinue, firstIncomplete
}

// phaseViewsFromPlan projects Plan.phases (a []any of maps) into
// []phaseView for the state machine.
func phaseViewsFromPlan(plan map[string]any) []phaseView {
	raw, ok := plan["phases"].([]any)
	if !ok {
		return nil
	}
	out := make([]phaseView, 0, len(raw))
	for _, p := range raw {
		m, ok := p.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, phaseView{
			Status:             getString(m, "status"),
			ExpectedTaskCount:  asInt(m["expectedTaskCount"]),
			CompletedTaskCount: asInt(m["completedTaskCount"]),
		})
	}
	return out
}

// lastCheckpointedPhase reads the phase index the loop has already parked
// at (persisted on metrics.lastPhaseCheckpoint), so a boundary isn't
// re-parked every time an update event fires for the same plan state.
// Returns -1 when unset.
func lastCheckpointedPhase(plan map[string]any) int {
	metrics, ok := plan["metrics"].(map[string]any)
	if !ok {
		return -1
	}
	if _, present := metrics["lastPhaseCheckpoint"]; !present {
		return -1
	}
	return asInt(metrics["lastPhaseCheckpoint"])
}

// maybeCheckpointAtPhaseBoundary evaluates the phase state machine for a
// RUNNING multi-phase plan and, when a fresh non-trivial boundary is
// reached (and not already parked there), parks the Plan to
// awaitingFeedback(phase_checkpoint). Returns handled=true when it parked.
//
// Idempotent: it records the parked phase index in
// metrics.lastPhaseCheckpoint and won't re-park the same boundary.
func (l *PlannerAgentLoop) maybeCheckpointAtPhaseBoundary(ctx context.Context, planId string) (handled bool, err error) {
	if !phaseCheckpointsEnabled() {
		return false, nil
	}
	plan, lerr := l.loadPlan(ctx, planId)
	if lerr != nil {
		return false, nil
	}
	// Only checkpoint a plan that's actively running its phases.
	if getString(plan, "status") != "running" {
		return false, nil
	}
	phases := phaseViewsFromPlan(plan)
	if len(phases) < 2 {
		return false, nil // single-phase / no outline -> nothing to checkpoint between
	}
	action, idx := evaluatePhaseProgress(phases, phaseCheckpointMinTasks())
	if action != phaseActionCheckpoint {
		return false, nil
	}
	if lastCheckpointedPhase(plan) == idx {
		return false, nil // already parked at this boundary
	}
	l.logger.Info("planner phase checkpoint: parking at phase boundary for review/approval",
		"planId", planId, "nextPhaseIndex", idx, "phaseCount", len(phases))
	return true, l.parkForPhaseCheckpoint(ctx, plan, planId, idx, len(phases))
}

// parkForPhaseCheckpoint transitions the Plan to
// awaitingFeedback(phase_checkpoint), recording the boundary index in
// metrics.lastPhaseCheckpoint for idempotency and surfacing progress +
// spend-so-far in the feedbackRequest for the canvas card.
func (l *PlannerAgentLoop) parkForPhaseCheckpoint(ctx context.Context, plan map[string]any, planId string, nextIdx, phaseCount int) error {
	merged := map[string]any{}
	if metrics, ok := plan["metrics"].(map[string]any); ok {
		for k, v := range metrics {
			merged[k] = v
		}
	}
	merged["lastPhaseCheckpoint"] = nextIdx
	metricsJSON, err := json.Marshal(merged)
	if err != nil {
		metricsJSON = []byte(`{}`)
	}

	fbReq := map[string]any{
		"question":       fmt.Sprintf("Phase %d of %d is complete. Review progress and spend, then approve continuing to the next phase.", nextIdx, phaseCount),
		"kind":           "choice",
		"completedPhase": nextIdx - 1,
		"nextPhase":      nextIdx,
		"phaseCount":     phaseCount,
		"tokenSpent":     asInt(plan["tokenSpent"]),
		"options": []map[string]any{
			{"label": "Continue", "value": "continue"},
			{"label": "Stop here", "value": "stop"},
		},
		"askedAt": time.Now().UTC().Format(time.RFC3339),
	}
	fbReqJSON, err := json.Marshal(fbReq)
	if err != nil {
		fbReqJSON = []byte(`{}`)
	}
	q := fmt.Sprintf(
		`mutation updatePlanStatus(planId:%q, status:"awaitingFeedback", feedbackReason:"phase_checkpoint", feedbackRequest:%s, metrics:%s)`,
		planId, string(fbReqJSON), string(metricsJSON),
	)
	_, err = l.engine.Execute(systemActorContext(ctx), q)
	return err
}

// phaseCheckpointsEnabled gates the whole feature (default on).
func phaseCheckpointsEnabled() bool {
	switch os.Getenv("MEMQL_PLANNER_PHASE_CHECKPOINTS_ENABLED") {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}
