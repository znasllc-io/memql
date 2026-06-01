package memql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// harness_step_validation.go enforces the v1:harness:step status state
// machine at the engine pre-insert boundary. The append-only DSL
// cannot express "reject done -> running" on its own, so the rule
// lives here (mirroring the participant guard in
// cognition_participant_validation.go). The guard runs on every step
// insert AND on every step update() (which read-merges the prior row
// then routes through executeInsert), so the harness mutations
// (addStep / startStep / completeStep / failStep) stay declarative.
//
// The transition rule itself is a pure, table-testable function
// (stepTransitionAllowed) so the full state machine -- including the
// retry (failed -> ready, attempt++) and blocked (running -> blocked,
// blocked -> ready) paths -- can be unit-tested without a database,
// like humanCapExceeded in the participant guard.

// stepStatusPending and friends are the v1:harness:step status enum
// values. Kept in one place so the state machine and the concept enum
// stay in lockstep.
const (
	stepStatusPending = "pending"
	stepStatusReady   = "ready"
	stepStatusRunning = "running"
	stepStatusBlocked = "blocked"
	stepStatusDone    = "done"
	stepStatusFailed  = "failed"
)

// stepTransitions is the allowed-transition adjacency for the step
// status state machine (issue #582):
//
//	pending -> ready                  (dependsOn satisfied)
//	ready   -> running                (controller claims)
//	running -> done | failed | blocked
//	blocked -> ready                  (blocker done)
//	failed  -> ready                  (retry, attempt++)
//	done is terminal (no outgoing edges)
//
// A self-transition (status unchanged) is always allowed: append-only
// versioning re-writes the same row for non-status field updates
// (e.g. stamping a result onto a still-running step) and must not be
// rejected as an "invalid transition".
var stepTransitions = map[string]map[string]bool{
	stepStatusPending: {stepStatusReady: true},
	stepStatusReady:   {stepStatusRunning: true},
	stepStatusRunning: {stepStatusDone: true, stepStatusFailed: true, stepStatusBlocked: true},
	stepStatusBlocked: {stepStatusReady: true},
	stepStatusFailed:  {stepStatusReady: true},
	stepStatusDone:    {},
}

// validStepStatus reports whether s is a known step status.
func validStepStatus(s string) bool {
	switch s {
	case stepStatusPending, stepStatusReady, stepStatusRunning,
		stepStatusBlocked, stepStatusDone, stepStatusFailed:
		return true
	default:
		return false
	}
}

// stepTransitionAllowed reports whether a step may move from `from` to
// `to`. Pure logic, split out so the state machine is unit-testable
// without a database. Rules:
//
//   - both statuses must be known enum values;
//   - a self-transition (from == to) is always allowed;
//   - otherwise `to` must be reachable from `from` per stepTransitions.
//
// `from` may be empty when there is no prior row (a brand-new step);
// callers handle the no-prior-row case before calling this (a fresh
// insert must land in `pending`), so an empty `from` here is treated
// as "no constraint" and allowed.
func stepTransitionAllowed(from, to string) bool {
	if !validStepStatus(to) {
		return false
	}
	if from == "" {
		return true
	}
	if !validStepStatus(from) {
		return false
	}
	if from == to {
		return true
	}
	return stepTransitions[from][to]
}

// retryBumpsAttempt reports whether a failed -> ready retry transition
// correctly increments the attempt counter. The reconciler (#583)
// owns the retry cap; the engine only enforces that a retry actually
// advances the counter so replays don't silently loop at the same
// attempt number. Non-retry transitions are unaffected (returns true).
func retryBumpsAttempt(from, to string, priorAttempt, newAttempt int) bool {
	if from == stepStatusFailed && to == stepStatusReady {
		return newAttempt > priorAttempt
	}
	return true
}

// validateHarnessStepTransition runs the engine pre-insert guard for
// v1:harness:step rows. For a brand-new step (no prior row at this id)
// it enforces the initial status is `pending`. For a re-version of an
// existing step it enforces the (priorStatus -> newStatus) transition
// is legal per the state machine, and that a retry bumps attempt.
//
// Called from executor.executeInsert; not part of the public API.
func (e *MemQLEngine) validateHarnessStepTransition(ctx context.Context, payload map[string]any, stepID string) error {
	if payload == nil {
		return fmt.Errorf("v1:harness:step: payload is required")
	}

	newStatus := strings.TrimSpace(stringFromAny(payload["status"]))
	if newStatus == "" {
		newStatus = stepStatusPending
	}
	if !validStepStatus(newStatus) {
		return fmt.Errorf("v1:harness:step: unknown status %q", newStatus)
	}

	stepID = strings.TrimSpace(stepID)
	prior, err := e.getLatestHarnessStepPayload(ctx, stepID)
	if err != nil {
		return err
	}

	// No prior row: this is a fresh step. It must start in pending.
	if prior == nil {
		if newStatus != stepStatusPending {
			return fmt.Errorf(
				"v1:harness:step: a new step must start in status %q, got %q",
				stepStatusPending, newStatus)
		}
		return nil
	}

	priorStatus := strings.TrimSpace(stringFromAny(prior["status"]))
	if priorStatus == "" {
		priorStatus = stepStatusPending
	}

	if !stepTransitionAllowed(priorStatus, newStatus) {
		return fmt.Errorf(
			"v1:harness:step: invalid transition %q -> %q",
			priorStatus, newStatus)
	}

	if !retryBumpsAttempt(priorStatus, newStatus,
		intFromAny(prior["attempt"]), intFromAny(payload["attempt"])) {
		return fmt.Errorf(
			"v1:harness:step: retry (%q -> %q) must increment attempt (was %d)",
			priorStatus, newStatus, intFromAny(prior["attempt"]))
	}

	return nil
}

// getLatestHarnessStepPayload loads the latest version of a step row by
// id. Returns (nil, nil) when no row exists yet (a fresh insert).
func (e *MemQLEngine) getLatestHarnessStepPayload(ctx context.Context, stepID string) (map[string]any, error) {
	if e == nil {
		return nil, fmt.Errorf("engine is nil")
	}
	if stepID == "" {
		return nil, nil
	}
	db := e.database()
	if db == nil {
		return nil, fmt.Errorf("memory engine database not configured")
	}

	var node memorynodes.MemoryNode
	err := db.NewSelect().
		Model(&node).
		Where("id = ?", stepID).
		Where("concept = ?", memorynodes.ConceptHarnessStep).
		OrderExpr(`"createdAt" DESC`).
		Limit(1).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("load harness step %q: %w", stepID, err)
	}

	payload := make(map[string]any)
	if jerr := json.Unmarshal(node.Payload, &payload); jerr != nil {
		return nil, fmt.Errorf("decode harness step %q payload: %w", stepID, jerr)
	}
	return payload, nil
}
