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

// forge_request_validation.go enforces the v1:forge:request status state
// machine at the engine pre-insert boundary. The append-only DSL cannot
// express "only an owner may set status=approved" on its own, so the
// role guard lives here -- mirroring the harness step-transition guard
// (harness_step_validation.go) and the feedback-intake guard
// (planner_feedback_validation.go).
//
// Two independent rules run on every write to v1:forge:request:
//
//  1. Transition-graph check (forgeRequestTransitionAllowed): the
//     (priorStatus -> newStatus) edge must exist in the approval-pipeline
//     adjacency. Owner actors may fast-track from any non-terminal state to
//     approved or queued; all other roles follow the strict graph.
//
//  2. Role-authority check (forgeRequestRoleAllowed): the incoming status
//     requires a minimum cluster role:
//     - "approved" / "queued"      -> owner only
//     - "needs_approval"           -> owner, admin, or writer
//     - everything else            -> any authenticated actor (reader can
//       submit; rejections/revisions are validator/approver actions, but
//       the DSL already gates those via traits)
//
// Both rules are pure, table-testable functions so the full matrix can be
// unit-tested without a database, like stepTransitionAllowed.

// conceptForgeRequest is the canonical concept id for v1:forge:request.
// Defined here (not in concept_ids.go) because forge is a product DSL
// bundle, not a filesystem-concept constant -- the same pattern used by
// conceptPlannerPlan.
const conceptForgeRequest = "v1:forge:request"

// Forge request status enum values. Must stay in lockstep with the
// `status` enum in dsl/forge/concepts.memql:request.
const (
	forgeStatusSubmitted      = "submitted"
	forgeStatusNeedsValidation = "needs_validation"
	forgeStatusValidated      = "validated"
	forgeStatusNeedsApproval  = "needs_approval"
	forgeStatusApproved       = "approved"
	forgeStatusChangesReq     = "changes_requested"
	forgeStatusRejected       = "rejected"
	forgeStatusQueued         = "queued"
)

// forgeRequestTransitions is the allowed-transition adjacency for the
// forge request approval pipeline:
//
//	submitted        -> needs_validation | needs_approval | queued
//	needs_validation -> validated | changes_requested | rejected
//	validated        -> needs_approval
//	needs_approval   -> approved | queued | changes_requested | rejected
//	changes_requested -> needs_validation | needs_approval
//	approved, rejected, queued are terminal (no outgoing edges in the
//	  standard graph; owner fast-track is handled separately)
//
// A self-transition (status unchanged) is always allowed: the append-only
// versioning re-writes the same row for non-status field updates and must
// not be rejected as "invalid".
var forgeRequestTransitions = map[string]map[string]bool{
	forgeStatusSubmitted: {
		forgeStatusNeedsValidation: true,
		forgeStatusNeedsApproval:   true,
		forgeStatusQueued:          true,
	},
	forgeStatusNeedsValidation: {
		// mutationValidateRequest promotes a developer-validated request
		// straight to needs_approval in one hop (it stamps validatedByUserId
		// and sets status=needs_approval), so this edge MUST exist or the
		// guard rejects the reader-path validation and the request wedges at
		// needs_validation (#1830 per-role coverage caught this; the
		// `validated` intermediate below has no producing mutation).
		forgeStatusNeedsApproval: true,
		forgeStatusValidated:     true,
		forgeStatusChangesReq:    true,
		forgeStatusRejected:      true,
	},
	forgeStatusValidated: {
		forgeStatusNeedsApproval: true,
	},
	forgeStatusNeedsApproval: {
		forgeStatusApproved:  true,
		forgeStatusQueued:    true,
		forgeStatusChangesReq: true,
		forgeStatusRejected:  true,
	},
	forgeStatusChangesReq: {
		forgeStatusNeedsValidation: true,
		forgeStatusNeedsApproval:   true,
	},
	// Terminal states -- no standard outgoing edges.
	forgeStatusApproved: {},
	forgeStatusRejected: {},
	forgeStatusQueued:   {},
}

// canonicalForgeRequestID normalizes a forge-request id to the canonical
// stored form "{concept}:{shortId}". Forge tools + the routeRequest
// automation pass the BARE short id (e.g. "r-selftest-003"); the engine
// stores rows canonical ("v1:forge:request:r-selftest-003"). The guard's
// prior-version lookup must match the stored form, so it canonicalizes here
// (already-canonical ids pass through unchanged, empty stays empty). Pure +
// unit-tested (#1830) -- this is the exact derivation whose absence let the
// pipeline-wedge bug ship (#1826).
func canonicalForgeRequestID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	if strings.HasPrefix(id, conceptForgeRequest+":") {
		return id
	}
	return conceptForgeRequest + ":" + id
}

// validForgeRequestStatus reports whether s is a known request status.
func validForgeRequestStatus(s string) bool {
	switch s {
	case forgeStatusSubmitted, forgeStatusNeedsValidation, forgeStatusValidated,
		forgeStatusNeedsApproval, forgeStatusApproved, forgeStatusChangesReq,
		forgeStatusRejected, forgeStatusQueued:
		return true
	default:
		return false
	}
}

// isForgeTerminalStatus reports whether s is a terminal status that cannot
// be transitioned away from (except by an owner fast-track).
func isForgeTerminalStatus(s string) bool {
	switch s {
	case forgeStatusApproved, forgeStatusRejected, forgeStatusQueued:
		return true
	default:
		return false
	}
}

// forgeRequestTransitionAllowed reports whether a forge request may move
// from `from` to `to` for an actor with the given role. Pure logic, split
// out so the state machine is unit-testable without a database. Rules:
//
//   - both statuses must be known enum values;
//   - a self-transition (from == to) is always allowed;
//   - if from is empty (fresh insert) there is no prior-status constraint;
//   - owners may fast-track from any non-terminal prior state to any valid
//     target (except self-approve from already-terminal, which is a no-op);
//   - all other roles must follow the strict transition graph.
func forgeRequestTransitionAllowed(from, to, role string) bool {
	if !validForgeRequestStatus(to) {
		return false
	}
	if from == "" {
		// Fresh insert: no graph constraint (status=submitted is enforced
		// separately by validateForgeRequestTransition).
		return true
	}
	if !validForgeRequestStatus(from) {
		return false
	}
	if from == to {
		return true
	}
	// Owner fast-track: may move from any non-terminal state to any
	// target that their role authority permits (role check is separate).
	if strings.ToLower(strings.TrimSpace(role)) == string(auth.RoleOwner) {
		if !isForgeTerminalStatus(from) {
			return true
		}
	}
	return forgeRequestTransitions[from][to]
}

// forgeRequestRoleAllowed reports whether an actor with the given role may
// set a forge request to `status`. Pure logic, unit-testable:
//
//   - approved / queued  -> owner only
//   - needs_approval     -> owner, admin, or writer (developers)
//   - all other statuses -> any role (fresh submit is reader-accessible;
//     the DSL traits gate query-layer visibility separately)
func forgeRequestRoleAllowed(status, role string) bool {
	r := strings.ToLower(strings.TrimSpace(role))
	switch strings.TrimSpace(status) {
	case forgeStatusApproved, forgeStatusQueued:
		return r == string(auth.RoleOwner)
	case forgeStatusNeedsApproval:
		switch r {
		case string(auth.RoleOwner), string(auth.RoleAdmin), string(auth.RoleWriter):
			return true
		default:
			return false
		}
	default:
		return true
	}
}

// validateForgeRequestTransition runs the engine pre-insert guard for
// v1:forge:request rows. It enforces:
//  1. For a brand-new request (no prior row): status must land "submitted".
//  2. For a re-version: the (priorStatus -> newStatus) edge is in the
//     approval-pipeline graph (with owner fast-track).
//  3. The actor's cluster role is sufficient to set the target status.
//
// The system actor is exempt from the role-authority check (#1847): the
// routeRequest automation (dsl/forge/automations.memql) enacts the
// submitter-role-derived transition -- owner -> "queued",
// admin/writer -> "needs_approval", reader -> "needs_validation" -- under
// the SYSTEM actor (role="system"). Re-checking the system actor's OWN
// role against forgeRequestRoleAllowed is wrong: it has no submitter role,
// so the role gate would reject the owner/admin/writer routes (only the
// reader path, which is not role-gated, slipped through), aborting
// logicRouteRequest before it recorded the "routed" requestEvent and
// leaving the request stuck "submitted" with empty history. The submitter
// authority was already enforced when the request was submitted; this
// guard exempts the system actor exactly like its sibling guards
// (validateFeedbackIntakeTransition, validateCognitionUtteranceAuth,
// validateAgentKindActor).
//
// Called from executor.executeInsert; not part of the public API.
func (e *MemQLEngine) validateForgeRequestTransition(ctx context.Context, payload map[string]any, requestID string, actor string) error {
	if payload == nil {
		return fmt.Errorf("v1:forge:request: payload is required")
	}

	newStatus := strings.TrimSpace(stringFromAny(payload["status"]))
	if newStatus == "" {
		newStatus = forgeStatusSubmitted
	}
	if !validForgeRequestStatus(newStatus) {
		return fmt.Errorf("v1:forge:request: unknown status %q", newStatus)
	}

	// System-actor bypass (#1847): the routeRequest automation runs the
	// transition under the system actor; its role is not the submitter's, so
	// skip the role/graph guards entirely (the submitter's authority was
	// checked at submit time). Mirrors the sibling guards' isSystemActor
	// early-return.
	identity, _ := auth.UserIdentityFromContext(ctx)
	if isSystemActor(identity, actor) {
		return nil
	}

	// Resolve actor role from context (fails closed: empty = reader).
	role := forgeActorRole(ctx)

	requestID = strings.TrimSpace(requestID)
	prior, err := e.getLatestForgeRequestPayload(ctx, requestID)
	if err != nil {
		return err
	}

	// No prior row: fresh insert must start in "submitted".
	if prior == nil {
		if newStatus != forgeStatusSubmitted {
			return fmt.Errorf(
				"v1:forge:request: a new request must start in status %q, got %q",
				forgeStatusSubmitted, newStatus)
		}
		return nil
	}

	priorStatus := strings.TrimSpace(stringFromAny(prior["status"]))
	if priorStatus == "" {
		priorStatus = forgeStatusSubmitted
	}

	return forgeRequestTransitionDecision(priorStatus, newStatus, role)
}

// forgeRequestTransitionDecision is the pure (priorStatus -> newStatus, role)
// decision applied to a re-version once the prior row has been loaded. Split
// out of validateForgeRequestTransition so the transition-graph + role-
// authority combination is unit-testable without a database (the
// system-actor bypass and the prior-row load are the only impure parts).
// Returns nil when the transition is permitted, or a descriptive error.
func forgeRequestTransitionDecision(priorStatus, newStatus, role string) error {
	if !forgeRequestTransitionAllowed(priorStatus, newStatus, role) {
		return fmt.Errorf(
			"v1:forge:request: invalid transition %q -> %q",
			priorStatus, newStatus)
	}

	if !forgeRequestRoleAllowed(newStatus, role) {
		return fmt.Errorf(
			"v1:forge:request: role %q is not permitted to set status %q "+
				"(requires: approved/queued -> owner; needs_approval -> owner/admin/writer)",
			role, newStatus)
	}

	return nil
}

// forgeActorRole returns the cluster role for the calling actor from the
// request context. Falls back to "reader" (most restrictive) if the
// context carries no access context, so the guard fails closed.
func forgeActorRole(ctx context.Context) string {
	if ac, ok := auth.AccessFromContext(ctx); ok && ac != nil {
		r := strings.TrimSpace(string(ac.Role))
		if r != "" {
			return r
		}
	}
	// Also check UserIdentity for system actors -- isSystemActor callers
	// bypass role checks entirely; here we just surface the raw role string
	// so unit tests that inject identity can see it.
	identity, _ := auth.UserIdentityFromContext(ctx)
	if r := strings.TrimSpace(identity.Role); r != "" {
		return r
	}
	return "reader"
}

// getLatestForgeRequestPayload loads the latest version of a
// v1:forge:request row by id. Returns (nil, nil) when no row exists yet
// (a fresh insert). Mirrors getLatestHarnessStepPayload.
func (e *MemQLEngine) getLatestForgeRequestPayload(ctx context.Context, requestID string) (map[string]any, error) {
	if e == nil {
		return nil, fmt.Errorf("engine is nil")
	}
	if requestID == "" {
		return nil, nil
	}
	db := e.database()
	if db == nil {
		return nil, fmt.Errorf("memory engine database not configured")
	}

	// Forge tools (and the routeRequest automation) pass the BARE short id
	// (e.g. "r-selftest-003"), but the engine stores rows under the canonical
	// "{concept}:{shortId}" id ("v1:forge:request:r-selftest-003"). A bare-id
	// lookup therefore misses the prior version on every UPDATE, so the guard
	// wrongly took the no-prior branch and rejected every transition with
	// "a new request must start in status submitted" -- wedging the whole
	// approval pipeline (and the router's own update) at "submitted" (#1826).
	// Match BOTH the canonical and the raw id so the prior version is found
	// regardless of which form the caller passed.
	canonicalID := canonicalForgeRequestID(requestID)

	var node memorynodes.MemoryNode
	err := db.NewSelect().
		Model(&node).
		Where("(id = ? OR id = ?)", canonicalID, requestID).
		Where("concept = ?", conceptForgeRequest).
		OrderExpr(`"createdAt" DESC`).
		Limit(1).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("load forge request %q: %w", requestID, err)
	}

	result := make(map[string]any)
	if jerr := json.Unmarshal(node.Payload, &result); jerr != nil {
		return nil, fmt.Errorf("decode forge request %q payload: %w", requestID, jerr)
	}
	return result, nil
}
