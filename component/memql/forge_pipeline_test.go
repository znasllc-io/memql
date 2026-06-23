package memql

import (
	"os"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// Forge pipeline + routing + query-gating contract tests (#1793).
//
// These lock the three DSL behaviours that the engine guard
// (forge_request_validation.go) cannot itself express, and cross-check
// them against the guard so the DSL and the engine can never silently
// drift apart:
//
//  1. ROUTING (dsl/forge/logic.memql:routeRequest) -- the submitter's
//     role picks the next pipeline status on submission.
//  2. EVENT TRAIL (dsl/forge/logic.memql:recordTransition) -- each
//     post-creation transition appends exactly one requestEvent whose kind
//     is derived from the new status.
//  3. QUERY GATING (dsl/forge/traits.memql) -- the developer/owner tiers
//     that keep the validation + approval queues empty for non-developers.
//
// Pure logic, no database -- mirrors forge_request_validation_test.go and
// TestStepTransitionAllowed.

// forgeRoutingContract is the role -> next-status map that
// routeRequest implements on a freshly submitted request.
var forgeRoutingContract = map[string]string{
	"owner":  forgeStatusQueued,          // fast-track: self-approved
	"admin":  forgeStatusNeedsApproval,   // senior dev: skips first-line validation
	"writer": forgeStatusNeedsApproval,   // junior dev: own report skips validation
	"reader": forgeStatusNeedsValidation, // non-dev: must be validated first
}

// forgeEventKindContract is the post-creation toStatus -> requestEvent.kind
// map that recordTransition implements.
var forgeEventKindContract = map[string]string{
	forgeStatusNeedsApproval: "validated",
	forgeStatusQueued:        "approved",
	forgeStatusChangesReq:    "changes_requested",
	forgeStatusRejected:      "rejected",
}

// readForgeLogic loads the authored routing/recording logic from disk so the
// contract maps above can be checked against what the DSL actually says
// (not just restated). The test binary runs in the package directory, so the
// bundle is two levels up.
func readForgeLogic(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../dsl/forge/logic.memql")
	if err != nil {
		t.Fatalf("read dsl/forge/logic.memql: %v", err)
	}
	return string(b)
}

// TestForgeRoutingContractMatchesGuard asserts that every role-routed
// transition the DSL performs on submission is BOTH a valid graph edge and
// role-authorized by the engine guard. If routing ever moved a reader
// straight to `queued`, this fails -- the DSL would be writing a status the
// pre-insert guard rejects.
func TestForgeRoutingContractMatchesGuard(t *testing.T) {
	for role, target := range forgeRoutingContract {
		if !forgeRequestTransitionAllowed(forgeStatusSubmitted, target, role) {
			t.Errorf("routing %s -> %q: not a valid submitted-> transition for role %s", role, target, role)
		}
		if !forgeRequestRoleAllowed(target, role) {
			t.Errorf("routing %s -> %q: role %s is not authorized to set that status (guard would reject the DSL write)", role, target, role)
		}
	}
}

// TestForgeRoutingContractMatchesDSL asserts the authored routeRequest
// actually encodes the role -> status map this test pins. Catches an edit to
// the DSL that changes routing without updating the contract (and vice versa).
func TestForgeRoutingContractMatchesDSL(t *testing.T) {
	src := readForgeLogic(t)
	for role, target := range forgeRoutingContract {
		needRole := `submitterRole == "` + role + `"`
		if !strings.Contains(src, needRole) {
			t.Errorf("routeRequest missing role guard %q", needRole)
		}
		needStatus := `status: "` + target + `"`
		if !strings.Contains(src, needStatus) {
			t.Errorf("routeRequest missing routed status %q (for role %s)", needStatus, role)
		}
	}
}

// TestForgeEventKindContractMatchesDSL asserts recordTransition maps each
// whitelisted toStatus to the expected requestEvent kind, so the audit trail
// stays complete and correctly labelled.
func TestForgeEventKindContractMatchesDSL(t *testing.T) {
	src := readForgeLogic(t)
	for status, kind := range forgeEventKindContract {
		needStatus := `toStatus == "` + status + `"`
		needKind := `kind: "` + kind + `"`
		if !strings.Contains(src, needStatus) {
			t.Errorf("recordTransition missing guard %q", needStatus)
		}
		if !strings.Contains(src, needKind) {
			t.Errorf("recordTransition missing event kind %q (for status %s)", needKind, status)
		}
	}
}

// TestForgePipelineWalkReaderPath walks the full non-developer pipeline the
// way it actually flows -- submit (routed to needs_validation), a developer
// validates, the owner approves -- asserting each edge is graph-valid AND
// that only the right roles can take the privileged steps.
func TestForgePipelineWalkReaderPath(t *testing.T) {
	const (
		owner  = "owner"
		writer = "writer"
		reader = "reader"
	)

	// 1. reader submits -> routed to needs_validation.
	if got := forgeRoutingContract[reader]; got != forgeStatusNeedsValidation {
		t.Fatalf("reader routing: got %q want needs_validation", got)
	}

	// 2. a developer (writer) validates: needs_validation -> validated.
	if !forgeRequestTransitionAllowed(forgeStatusNeedsValidation, forgeStatusValidated, writer) {
		t.Error("writer should be able to validate (needs_validation -> validated)")
	}
	// 3. validated -> needs_approval (queued for the owner).
	if !forgeRequestTransitionAllowed(forgeStatusValidated, forgeStatusNeedsApproval, writer) {
		t.Error("validated -> needs_approval should be allowed")
	}
	if !forgeRequestRoleAllowed(forgeStatusNeedsApproval, writer) {
		t.Error("writer (developer) should be authorized to move a request to needs_approval")
	}
	// 4. owner approves: needs_approval -> approved.
	if !forgeRequestTransitionAllowed(forgeStatusNeedsApproval, forgeStatusApproved, owner) {
		t.Error("owner should be able to approve (needs_approval -> approved)")
	}
	if !forgeRequestRoleAllowed(forgeStatusApproved, owner) {
		t.Error("owner must be authorized to set approved")
	}

	// A reader must NOT be able to approve or to move a request to approval.
	if forgeRequestRoleAllowed(forgeStatusApproved, reader) {
		t.Error("reader must NOT be authorized to approve")
	}
	if forgeRequestRoleAllowed(forgeStatusNeedsApproval, reader) {
		t.Error("reader must NOT be authorized to push a request to needs_approval (validation is a developer act)")
	}
}

// TestForgeOwnerFastTrack asserts the owner self-approve short-circuit
// (submitted -> queued) is owner-only.
func TestForgeOwnerFastTrack(t *testing.T) {
	if !forgeRequestTransitionAllowed(forgeStatusSubmitted, forgeStatusQueued, "owner") {
		t.Error("owner fast-track submitted -> queued should be allowed")
	}
	if !forgeRequestRoleAllowed(forgeStatusQueued, "owner") {
		t.Error("owner must be authorized to set queued")
	}
	for _, role := range []string{"admin", "writer", "reader"} {
		if forgeRequestRoleAllowed(forgeStatusQueued, role) {
			t.Errorf("%s must NOT be authorized to fast-track to queued (owner-only)", role)
		}
	}
}

// TestForgeQueueGatingTiers locks the two query-layer gating tiers from
// traits.memql against the engine role-authority, so the surface a reader can
// reach stays empty of other people's work:
//
//   - approver tier (forgeApprover) == owner only: only the owner may
//     reach the terminal approve/queue statuses.
//   - developer tier (forgeDeveloper) == owner/admin/writer: developers
//     may reach needs_approval (the validation act); a reader may not.
func TestForgeQueueGatingTiers(t *testing.T) {
	developerTier := map[string]bool{
		string(auth.RoleOwner):  true,
		string(auth.RoleAdmin):  true,
		string(auth.RoleWriter): true,
		string(auth.RoleReader): false,
	}
	for role, isDev := range developerTier {
		// needs_approval is the developer-gated validation act.
		if got := forgeRequestRoleAllowed(forgeStatusNeedsApproval, role); got != isDev {
			t.Errorf("developer tier mismatch for %s: needs_approval allowed=%v want %v", role, got, isDev)
		}
	}

	approverTier := map[string]bool{
		string(auth.RoleOwner):  true,
		string(auth.RoleAdmin):  false,
		string(auth.RoleWriter): false,
		string(auth.RoleReader): false,
	}
	for role, isApprover := range approverTier {
		if got := forgeRequestRoleAllowed(forgeStatusApproved, role); got != isApprover {
			t.Errorf("approver tier mismatch for %s: approved allowed=%v want %v", role, got, isApprover)
		}
		if got := forgeRequestRoleAllowed(forgeStatusQueued, role); got != isApprover {
			t.Errorf("approver tier mismatch for %s: queued allowed=%v want %v", role, got, isApprover)
		}
	}

	// And the traits the queues actually reference exist with the right
	// role sets in the bundle.
	traits, err := os.ReadFile("../../dsl/forge/traits.memql")
	if err != nil {
		t.Fatalf("read dsl/forge/traits.memql: %v", err)
	}
	ts := string(traits)
	for _, must := range []string{
		"forgeDeveloper", "forgeApprover",
		`actor.role == "owner"`, `actor.role == "admin"`, `actor.role == "writer"`,
	} {
		if !strings.Contains(ts, must) {
			t.Errorf("traits.memql missing expected fragment %q", must)
		}
	}
}
