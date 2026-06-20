package memql

import (
	"fmt"
	"testing"
)

// TestForgeRequestTransitionAllowed covers the full v1:forge:request
// approval-pipeline state machine, including the owner fast-track.
// Pure logic, no database -- mirrors TestStepTransitionAllowed.
func TestForgeRequestTransitionAllowed(t *testing.T) {
	const owner = "owner"
	const writer = "writer"
	const reader = "reader"

	cases := []struct {
		from string
		to   string
		role string
		want bool
		note string
	}{
		// ---------------------------------------------------------------------------
		// Standard forward edges -- any authenticated role may traverse the graph
		// (role-authority is a separate check; here we only test graph validity).
		// ---------------------------------------------------------------------------
		{forgeStatusSubmitted, forgeStatusNeedsValidation, writer, true, "submitted -> needs_validation (developer path)"},
		{forgeStatusSubmitted, forgeStatusNeedsApproval, writer, true, "submitted -> needs_approval (direct to approval)"},
		{forgeStatusSubmitted, forgeStatusQueued, owner, true, "submitted -> queued (owner fast-track)"},
		{forgeStatusNeedsValidation, forgeStatusValidated, writer, true, "needs_validation -> validated"},
		{forgeStatusNeedsValidation, forgeStatusChangesReq, writer, true, "needs_validation -> changes_requested"},
		{forgeStatusNeedsValidation, forgeStatusRejected, writer, true, "needs_validation -> rejected"},
		{forgeStatusValidated, forgeStatusNeedsApproval, writer, true, "validated -> needs_approval"},
		{forgeStatusNeedsApproval, forgeStatusApproved, owner, true, "needs_approval -> approved"},
		{forgeStatusNeedsApproval, forgeStatusQueued, owner, true, "needs_approval -> queued"},
		{forgeStatusNeedsApproval, forgeStatusChangesReq, writer, true, "needs_approval -> changes_requested"},
		{forgeStatusNeedsApproval, forgeStatusRejected, writer, true, "needs_approval -> rejected"},
		{forgeStatusChangesReq, forgeStatusNeedsValidation, writer, true, "changes_requested -> needs_validation"},
		{forgeStatusChangesReq, forgeStatusNeedsApproval, writer, true, "changes_requested -> needs_approval"},

		// ---------------------------------------------------------------------------
		// Owner fast-track: from any non-terminal state to any valid target.
		// ---------------------------------------------------------------------------
		{forgeStatusSubmitted, forgeStatusApproved, owner, true, "owner fast-track: submitted -> approved"},
		{forgeStatusNeedsValidation, forgeStatusApproved, owner, true, "owner fast-track: needs_validation -> approved"},
		{forgeStatusValidated, forgeStatusApproved, owner, true, "owner fast-track: validated -> approved"},
		{forgeStatusChangesReq, forgeStatusApproved, owner, true, "owner fast-track: changes_requested -> approved"},
		{forgeStatusNeedsValidation, forgeStatusQueued, owner, true, "owner fast-track: needs_validation -> queued"},
		{forgeStatusValidated, forgeStatusQueued, owner, true, "owner fast-track: validated -> queued"},
		{forgeStatusChangesReq, forgeStatusQueued, owner, true, "owner fast-track: changes_requested -> queued"},

		// ---------------------------------------------------------------------------
		// Self-transitions are always allowed (non-status field updates re-version
		// the same row without changing status).
		// ---------------------------------------------------------------------------
		{forgeStatusSubmitted, forgeStatusSubmitted, reader, true, "self-transition submitted"},
		{forgeStatusNeedsValidation, forgeStatusNeedsValidation, writer, true, "self-transition needs_validation"},
		{forgeStatusApproved, forgeStatusApproved, owner, true, "self-transition approved (terminal)"},
		{forgeStatusQueued, forgeStatusQueued, owner, true, "self-transition queued (terminal)"},

		// ---------------------------------------------------------------------------
		// Fresh insert (empty from): no graph constraint here.
		// ---------------------------------------------------------------------------
		{"", forgeStatusSubmitted, reader, true, "fresh insert: submitted"},
		{"", forgeStatusApproved, owner, true, "fresh insert: no constraint (status check is separate)"},

		// ---------------------------------------------------------------------------
		// Invalid transitions -- non-owner cannot skip validation.
		// ---------------------------------------------------------------------------
		{forgeStatusSubmitted, forgeStatusApproved, writer, false, "writer cannot skip to approved"},
		{forgeStatusSubmitted, forgeStatusApproved, reader, false, "reader cannot skip to approved"},
		{forgeStatusValidated, forgeStatusSubmitted, writer, false, "cannot go backwards to submitted"},
		{forgeStatusNeedsApproval, forgeStatusSubmitted, writer, false, "cannot go backwards to submitted"},
		{forgeStatusNeedsApproval, forgeStatusValidated, writer, false, "cannot go backwards to validated"},
		{forgeStatusNeedsApproval, forgeStatusNeedsValidation, writer, false, "cannot go backwards to needs_validation"},

		// ---------------------------------------------------------------------------
		// Terminal state lock: once in a terminal state, no standard edges out
		// (owner fast-track does NOT apply FROM terminal states).
		// ---------------------------------------------------------------------------
		{forgeStatusApproved, forgeStatusSubmitted, owner, false, "approved (terminal) -> submitted rejected even for owner"},
		{forgeStatusApproved, forgeStatusNeedsValidation, owner, false, "approved (terminal) -> needs_validation rejected"},
		{forgeStatusRejected, forgeStatusSubmitted, owner, false, "rejected (terminal) -> submitted rejected"},
		{forgeStatusQueued, forgeStatusApproved, owner, false, "queued (terminal) -> approved rejected"},
		{forgeStatusApproved, forgeStatusQueued, owner, false, "approved -> queued rejected (both terminal)"},

		// ---------------------------------------------------------------------------
		// Unknown statuses are rejected.
		// ---------------------------------------------------------------------------
		{forgeStatusSubmitted, "bogus", writer, false, "unknown target status"},
		{"bogus", forgeStatusValidated, writer, false, "unknown source status"},
	}

	for _, c := range cases {
		name := fmt.Sprintf("%s_%s_to_%s", c.role, emptyAsDash(c.from), c.to)
		t.Run(name, func(t *testing.T) {
			got := forgeRequestTransitionAllowed(c.from, c.to, c.role)
			if got != c.want {
				t.Errorf("forgeRequestTransitionAllowed(%q, %q, role=%q) = %v, want %v (%s)",
					c.from, c.to, c.role, got, c.want, c.note)
			}
		})
	}
}

// TestForgeRequestRoleAllowed covers the role-authority matrix for each
// target status. Pure logic, no database.
func TestForgeRequestRoleAllowed(t *testing.T) {
	cases := []struct {
		status string
		role   string
		want   bool
		note   string
	}{
		// approved: owner only
		{forgeStatusApproved, "owner", true, "owner may approve"},
		{forgeStatusApproved, "admin", false, "admin cannot approve"},
		{forgeStatusApproved, "writer", false, "writer cannot approve"},
		{forgeStatusApproved, "reader", false, "reader cannot approve"},
		{forgeStatusApproved, "", false, "empty role cannot approve"},

		// queued: owner only
		{forgeStatusQueued, "owner", true, "owner may queue"},
		{forgeStatusQueued, "admin", false, "admin cannot queue directly"},
		{forgeStatusQueued, "writer", false, "writer cannot queue directly"},
		{forgeStatusQueued, "reader", false, "reader cannot queue directly"},

		// needs_approval (validate): owner, admin, writer (developers)
		{forgeStatusNeedsApproval, "owner", true, "owner may validate"},
		{forgeStatusNeedsApproval, "admin", true, "admin may validate"},
		{forgeStatusNeedsApproval, "writer", true, "writer may validate"},
		{forgeStatusNeedsApproval, "reader", false, "reader cannot validate"},
		{forgeStatusNeedsApproval, "", false, "empty role cannot validate"},

		// submitted: any role (reader can create a request)
		{forgeStatusSubmitted, "reader", true, "reader may submit"},
		{forgeStatusSubmitted, "writer", true, "writer may submit"},
		{forgeStatusSubmitted, "admin", true, "admin may submit"},
		{forgeStatusSubmitted, "owner", true, "owner may submit"},

		// needs_validation: any role (routing step, not privileged)
		{forgeStatusNeedsValidation, "reader", true, "reader can reach needs_validation"},
		{forgeStatusNeedsValidation, "writer", true, "writer can reach needs_validation"},

		// validated: any role
		{forgeStatusValidated, "writer", true, "writer can mark validated"},
		{forgeStatusValidated, "reader", true, "reader role passes (graph guards separately)"},

		// changes_requested: any role (rejections are gated by graph, not role here)
		{forgeStatusChangesReq, "writer", true, "writer can request changes"},
		{forgeStatusChangesReq, "reader", true, "reader role passes"},

		// rejected: any role (graph controls who can actually reach it)
		{forgeStatusRejected, "writer", true, "writer can reject (graph-gated)"},
		{forgeStatusRejected, "reader", true, "reader role passes for rejected"},

		// Case-insensitivity
		{forgeStatusApproved, "OWNER", true, "OWNER (uppercase) may approve"},
		{forgeStatusApproved, "Owner", true, "Owner (mixed) may approve"},
		{forgeStatusNeedsApproval, "WRITER", true, "WRITER (uppercase) may validate"},
	}

	for _, c := range cases {
		name := fmt.Sprintf("role_%s_status_%s", emptyAsDash(c.role), c.status)
		t.Run(name, func(t *testing.T) {
			got := forgeRequestRoleAllowed(c.status, c.role)
			if got != c.want {
				t.Errorf("forgeRequestRoleAllowed(%q, %q) = %v, want %v (%s)",
					c.status, c.role, got, c.want, c.note)
			}
		})
	}
}

// TestValidForgeRequestStatus guards the enum/state-machine lockstep.
func TestValidForgeRequestStatus(t *testing.T) {
	valid := []string{
		forgeStatusSubmitted, forgeStatusNeedsValidation, forgeStatusValidated,
		forgeStatusNeedsApproval, forgeStatusApproved, forgeStatusChangesReq,
		forgeStatusRejected, forgeStatusQueued,
	}
	for _, s := range valid {
		if !validForgeRequestStatus(s) {
			t.Errorf("validForgeRequestStatus(%q) = false, want true", s)
		}
	}
	invalid := []string{"", "open", "pending", "running", "done", "bogus", "APPROVED"}
	for _, s := range invalid {
		if validForgeRequestStatus(s) {
			t.Errorf("validForgeRequestStatus(%q) = true, want false", s)
		}
	}
}

// TestIsForgeTerminalStatus guards that only the three terminal states are
// recognised as terminal.
func TestIsForgeTerminalStatus(t *testing.T) {
	terminal := []string{forgeStatusApproved, forgeStatusRejected, forgeStatusQueued}
	for _, s := range terminal {
		if !isForgeTerminalStatus(s) {
			t.Errorf("isForgeTerminalStatus(%q) = false, want true", s)
		}
	}
	nonTerminal := []string{
		forgeStatusSubmitted, forgeStatusNeedsValidation, forgeStatusValidated,
		forgeStatusNeedsApproval, forgeStatusChangesReq, "", "bogus",
	}
	for _, s := range nonTerminal {
		if isForgeTerminalStatus(s) {
			t.Errorf("isForgeTerminalStatus(%q) = true, want false", s)
		}
	}
}

// TestForgeRequestTransitionFullReachability asserts no undocumented edges
// slip through: every (from, to) pair that is NOT in forgeRequestTransitions
// is rejected for a non-owner, and terminal states block even owners.
func TestForgeRequestTransitionFullReachability(t *testing.T) {
	allStatuses := []string{
		forgeStatusSubmitted, forgeStatusNeedsValidation, forgeStatusValidated,
		forgeStatusNeedsApproval, forgeStatusApproved, forgeStatusChangesReq,
		forgeStatusRejected, forgeStatusQueued,
	}
	for _, from := range allStatuses {
		for _, to := range allStatuses {
			// Self-transitions are always allowed.
			if from == to {
				if !forgeRequestTransitionAllowed(from, to, "writer") {
					t.Errorf("self-transition %q -> %q should be allowed", from, to)
				}
				continue
			}
			// For a non-owner (writer): allowed iff explicitly in the map.
			wantWriter := forgeRequestTransitions[from][to]
			gotWriter := forgeRequestTransitionAllowed(from, to, "writer")
			if gotWriter != wantWriter {
				t.Errorf("writer: forgeRequestTransitionAllowed(%q, %q) = %v, want %v",
					from, to, gotWriter, wantWriter)
			}
			// For an owner: allowed from any non-terminal source to any valid
			// target (the terminal source lock is enforced separately).
			wantOwner := !isForgeTerminalStatus(from)
			gotOwner := forgeRequestTransitionAllowed(from, to, "owner")
			if gotOwner != wantOwner {
				t.Errorf("owner: forgeRequestTransitionAllowed(%q, %q) = %v, want %v",
					from, to, gotOwner, wantOwner)
			}
		}
	}
}

// TestForgeRoleXTransitionMatrix exercises every role against the three
// critical target statuses (approved, queued, needs_approval) to validate
// that the combined guard (role check AND transition check) blocks
// unauthorised callers. The two pure functions are tested together as a
// pair the way validateForgeRequestTransition uses them.
//
// Note: forgeRequestTransitionAllowed is purely a graph check; it returns
// true for "submitted -> queued" for ANY role because that edge exists
// in the standard graph. The role-authority block is enforced separately
// by forgeRequestRoleAllowed. The combined test below checks BOTH and
// asserts that at least one of them rejects an unauthorised transition.
func TestForgeRoleXTransitionMatrix(t *testing.T) {
	type cell struct {
		role     string
		status   string
		wantRole bool // forgeRequestRoleAllowed must return this
		// wantAllowed is the expected result of the combined check:
		// both forgeRequestRoleAllowed AND forgeRequestTransitionAllowed
		// must be true for the action to proceed.
		wantAllowed bool
	}
	// from = submitted (a non-terminal, edge-rich prior state).
	from := forgeStatusSubmitted
	cases := []cell{
		// approved: owner-only; submitted -> approved is only in graph for owner.
		{"owner", forgeStatusApproved, true, true},
		{"admin", forgeStatusApproved, false, false},
		{"writer", forgeStatusApproved, false, false},
		{"reader", forgeStatusApproved, false, false},
		// queued: owner-only (role); graph has submitted -> queued for all,
		// so role check is what stops non-owners.
		{"owner", forgeStatusQueued, true, true},
		{"admin", forgeStatusQueued, false, false},
		{"writer", forgeStatusQueued, false, false},
		{"reader", forgeStatusQueued, false, false},
		// needs_approval: owner/admin/writer allowed; reader is not.
		{"owner", forgeStatusNeedsApproval, true, true},
		{"admin", forgeStatusNeedsApproval, true, true},
		{"writer", forgeStatusNeedsApproval, true, true},
		{"reader", forgeStatusNeedsApproval, false, false},
	}

	for _, c := range cases {
		name := fmt.Sprintf("role_%s_to_%s", c.role, c.status)
		t.Run(name, func(t *testing.T) {
			gotRole := forgeRequestRoleAllowed(c.status, c.role)
			if gotRole != c.wantRole {
				t.Errorf("forgeRequestRoleAllowed(%q, %q) = %v, want %v",
					c.status, c.role, gotRole, c.wantRole)
			}
			// The combined guard: both checks must pass for the action to succeed.
			gotTrans := forgeRequestTransitionAllowed(from, c.status, c.role)
			gotAllowed := gotRole && gotTrans
			if gotAllowed != c.wantAllowed {
				t.Errorf("combined guard for role=%q status=%q: role=%v trans=%v combined=%v, want combined=%v",
					c.role, c.status, gotRole, gotTrans, gotAllowed, c.wantAllowed)
			}
		})
	}
}
