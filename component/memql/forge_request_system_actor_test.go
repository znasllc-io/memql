package memql

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// forgeSystemActorContext builds a request context carrying the system actor
// (role="system"), exactly like component/automations/executor.go's
// contextWithSystemActor does for the routeRequest automation. The
// validateForgeRequestTransition system-actor bypass keys off this.
func forgeSystemActorContext(actorID string) context.Context {
	claims := map[string]any{
		"sub":   actorID,
		"email": actorID,
		"role":  "system",
	}
	token := auth.BuildTokenInfo(claims)
	ctx := auth.ContextWithClaims(context.Background(), claims)
	return auth.ContextWithToken(ctx, token)
}

// TestValidateForgeRequestTransition_SystemActorBypass is the #1847
// regression test. The routeRequest automation (dsl/forge/automations.memql)
// enacts the submitter-role-derived transition under the SYSTEM actor:
//
//	owner       -> "queued"
//	admin/writer -> "needs_approval"
//	reader       -> "needs_validation"
//
// Before the fix, validateForgeRequestTransition re-checked the system
// actor's own role via forgeRequestRoleAllowed, which rejected the owner
// ("queued" requires owner) and admin/writer ("needs_approval") routes --
// the system actor has role "system", not the submitter's role. That abort
// happened inside logicRouteRequest's mutationAdvanceRequest step, BEFORE
// the trailing mutationRecordRequestEvent ran, leaving the request stuck
// "submitted" with empty history. Only the reader -> "needs_validation"
// route (not role-gated) survived.
//
// The bypass returns nil before any DB load, so a zero-value engine (nil
// database) is sufficient to exercise it: if the guard ever falls through
// to forgeActorRole/getLatestForgeRequestPayload for a system actor, this
// test surfaces it (either a non-nil error from the role gate, pre-fix, or
// a nil-DB panic).
func TestValidateForgeRequestTransition_SystemActorBypass(t *testing.T) {
	e := &MemQLEngine{}
	const actor = "system:automation:routeRequest"
	ctx := forgeSystemActorContext(actor)

	// Every target status the routeRequest automation can set must pass when
	// the system actor enacts it -- including the role-gated owner/dev routes
	// that the pre-fix guard rejected.
	targets := []string{
		forgeStatusQueued,         // owner submitter route (owner-only role gate)
		forgeStatusNeedsApproval,  // admin/writer submitter route (dev role gate)
		forgeStatusNeedsValidation, // reader submitter route (was the only one working)
	}
	for _, target := range targets {
		payload := map[string]any{"status": target}
		if err := e.validateForgeRequestTransition(ctx, payload, "r-1847", actor); err != nil {
			t.Fatalf("system actor must bypass the forge role guard for status %q, got error: %v",
				target, err)
		}
	}
}

// TestForgeRequestTransitionDecision_RealUserStillGated confirms the fix did
// NOT weaken validation for real user actors: a reader is still rejected for
// the owner-only "queued" target even from a valid prior status, and the
// pure decision matches the role/graph guards. This is the "do not weaken
// real actors" half of #1847.
func TestForgeRequestTransitionDecision_RealUserStillGated(t *testing.T) {
	cases := []struct {
		name        string
		priorStatus string
		newStatus   string
		role        string
		wantErr     bool
	}{
		// Reader is NOT a system actor; the owner-only role gate still rejects.
		{"reader -> queued rejected", forgeStatusSubmitted, forgeStatusQueued, "reader", true},
		// Writer cannot reach owner-only queued either.
		{"writer -> queued rejected", forgeStatusSubmitted, forgeStatusQueued, "writer", true},
		// Reader cannot reach the dev-only needs_approval.
		{"reader -> needs_approval rejected", forgeStatusSubmitted, forgeStatusNeedsApproval, "reader", true},
		// The legitimate user routes still pass.
		{"owner -> queued allowed", forgeStatusSubmitted, forgeStatusQueued, "owner", false},
		{"writer -> needs_approval allowed", forgeStatusSubmitted, forgeStatusNeedsApproval, "writer", false},
		{"reader -> needs_validation allowed", forgeStatusSubmitted, forgeStatusNeedsValidation, "reader", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := forgeRequestTransitionDecision(c.priorStatus, c.newStatus, c.role)
			if (err != nil) != c.wantErr {
				t.Fatalf("forgeRequestTransitionDecision(%q, %q, %q) error = %v, wantErr = %v",
					c.priorStatus, c.newStatus, c.role, err, c.wantErr)
			}
		})
	}
}
