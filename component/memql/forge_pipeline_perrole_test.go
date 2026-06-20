package memql

import "testing"

// Per-role forge approval-pipeline coverage (#1830, from the forge validation
// run that found the pipeline wedged). The pre-existing forge_pipeline_test.go
// checks the routing/event CONTRACT maps; these add (a) a direct unit test for
// the canonical-id derivation whose absence let the guard reject every update
// (#1826), and (b) an end-to-end edge walk of each role's FULL path through the
// guard (submit -> route -> validate -> approve), with the correct actor role
// at each hop -- the coverage that would have caught both the id-lookup wedge
// and the needs_validation->needs_approval graph gap.

// TestCanonicalForgeRequestID locks the bare<->canonical normalization the
// guard's prior-version lookup depends on (#1826/#1830).
func TestCanonicalForgeRequestID(t *testing.T) {
	cases := []struct{ in, want string }{
		{"r-selftest-003", "v1:forge:request:r-selftest-003"},                              // bare -> canonical
		{"v1:forge:request:r-selftest-003", "v1:forge:request:r-selftest-003"},             // already canonical -> unchanged
		{"  r-selftest-003  ", "v1:forge:request:r-selftest-003"},                          // trimmed then canonicalized
		{"", ""},                                                                            // empty stays empty
		{"v1:forge:request:", "v1:forge:request:"},                                         // degenerate canonical prefix -> unchanged
	}
	for _, c := range cases {
		if got := canonicalForgeRequestID(c.in); got != c.want {
			t.Errorf("canonicalForgeRequestID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// pipelineHop is one transition in a role's path: who acts, and the
// from->to status edge they drive.
type pipelineHop struct {
	actorRole  string
	from       string
	to         string
}

// TestForgeFullPipelinePerRole walks each submitter role's full pipeline and
// asserts every hop is BOTH a valid graph edge (forgeRequestTransitionAllowed)
// and role-authorized (forgeRequestRoleAllowed) -- the multi-hop coverage the
// guard wedge (#1826) and the validate-edge gap (#1830) both slipped through.
func TestForgeFullPipelinePerRole(t *testing.T) {
	paths := map[string][]pipelineHop{
		// Owner fast-track: submit self-approves straight to queued.
		"owner": {
			{"owner", forgeStatusSubmitted, forgeStatusQueued},
		},
		// Developer (writer): own report skips first-line validation; owner approves.
		"writer": {
			{"writer", forgeStatusSubmitted, forgeStatusNeedsApproval}, // routeRequest
			{"owner", forgeStatusNeedsApproval, forgeStatusQueued},     // owner approves
		},
		// Senior developer (admin): same shape as writer.
		"admin": {
			{"admin", forgeStatusSubmitted, forgeStatusNeedsApproval},
			{"owner", forgeStatusNeedsApproval, forgeStatusQueued},
		},
		// Non-developer (reader): routed to validation; a developer validates
		// (-> needs_approval), then the owner approves (-> queued).
		"reader": {
			{"reader", forgeStatusSubmitted, forgeStatusNeedsValidation}, // routeRequest
			{"writer", forgeStatusNeedsValidation, forgeStatusNeedsApproval}, // mutationValidateRequest
			{"owner", forgeStatusNeedsApproval, forgeStatusQueued},          // owner approves
		},
	}

	for submitter, hops := range paths {
		for i, h := range hops {
			if !forgeRequestTransitionAllowed(h.from, h.to, h.actorRole) {
				t.Errorf("%s path hop %d: %s->%s by %s is not a valid graph edge",
					submitter, i, h.from, h.to, h.actorRole)
			}
			if !forgeRequestRoleAllowed(h.to, h.actorRole) {
				t.Errorf("%s path hop %d: role %s not authorized to set %q",
					submitter, i, h.actorRole, h.to)
			}
		}
	}
}

// TestForgeChangesAndRejectEdges locks the review-rejection edges so a
// reviewer can always send a request back or reject it from either review
// state (regression net for the pipeline's escape hatches).
func TestForgeChangesAndRejectEdges(t *testing.T) {
	reviewStates := []string{forgeStatusNeedsValidation, forgeStatusNeedsApproval}
	for _, from := range reviewStates {
		for _, to := range []string{forgeStatusChangesReq, forgeStatusRejected} {
			if !forgeRequestTransitionAllowed(from, to, "writer") {
				t.Errorf("reviewer must be able to move %s -> %s", from, to)
			}
		}
	}
}
