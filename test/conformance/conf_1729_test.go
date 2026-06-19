package conformance

// conf_1729_test.go -- regression dimension for memql#1729.
//
// mutationRevokeDelegation used to be authored as
// `insert { id: args.delegationId; args.payload }` -- it spread the caller's
// payload and never forced the revoked terminal state. A revoke call that
// omitted `active` therefore left the row active=true (the engine read-merge
// from memql#1709 preserves the omitted value), so the "revoked" delegation
// kept resolving on the auth hot path.
//
// The fix rewrites the mutation to an update{} (read-merge) that AUTHORITATIVELY
// forces `active: false` and guarantees `revokedAt` is stamped, regardless of
// caller input -- mirroring the sibling mutationRevokePATIdentity. The caller
// supplies only delegationId (+ optional revokedBySubject / revokedAt); the
// terminal state can no longer depend on (or be subverted by) the payload.
//
// This dimension asserts both halves of the contract:
//   1. A revoke that supplies NEITHER active NOR revokedAt still yields
//      active=false with a non-empty revokedAt (the engine forces them), while
//      every other field is preserved by the read-merge.
//   2. The revoked row is excluded from the active-delegation query
//      (queryActiveDelegationsForAgent), i.e. the revoke actually takes effect.

import (
	"fmt"
	"testing"
)

func TestConf1729RevokeForcesTerminalState(t *testing.T) {
	e := newEnv(t)
	if !e.HasDB {
		t.Skip("requires Postgres (set MEMORY_NODES_DATABASE_DSN); validated in the CI conformance job")
	}

	sfx := uniqueSuffix("1729")
	delegationID := "deleg-1729-" + sfx
	identityID := "identity-1729-" + sfx
	identitySubject := "subject-1729-" + sfx
	agentID := "agent-1729-" + sfx

	// CREATE -- a live delegation.
	createOut := e.runMutation(t, "mutationCreateDelegation", map[string]any{
		"delegationId":     delegationID,
		"identityId":       identityID,
		"identitySubject":  identitySubject,
		"identityType":     "human",
		"agentId":          agentID,
		"roleCeiling":      "writer",
		"scopes":           []any{"query:*", "mutation:cognition.*"},
		"createdBySubject": ownerUID,
		"note":             "memql#1729 conformance fixture",
	})
	storedID := storedID(t, createOut)

	before := e.latestPayload(t, "v1:identity:delegation", storedID)
	if before["active"] != true {
		t.Fatalf("#1729 precondition: seed delegation must start active=true; got %v", before["active"])
	}

	// Present in the active-delegation query while live.
	activeBefore := asArray(t, e.runQuery(t, "queryActiveDelegationsForAgent", map[string]any{"agentId": agentID}))
	if !containsID(activeBefore, storedID) {
		t.Fatalf("#1729 precondition: active delegation %s missing from queryActiveDelegationsForAgent; got %v", storedID, idIDs(activeBefore))
	}

	// REVOKE -- the caller supplies ONLY the revoker subject. It passes neither
	// `active` nor `revokedAt`; the mutation must force both itself. This is the
	// exact "partial payload that omits active" case from the bug report.
	e.runMutation(t, "mutationRevokeDelegation", map[string]any{
		"delegationId":     storedID,
		"revokedBySubject": ownerUID,
	})

	after := e.latestPayload(t, "v1:identity:delegation", storedID)

	// (1a) active forced to false even though the caller never supplied it.
	if after["active"] != false {
		t.Fatalf("#1729: revoke must authoritatively force active=false even when omitted by caller; got %v", after["active"])
	}
	// (1b) revokedAt forced to a non-empty timestamp by the engine.
	revokedAt, _ := after["revokedAt"].(string)
	if revokedAt == "" {
		t.Fatalf("#1729: revoke must stamp a non-empty revokedAt even when the caller omits it; got %#v", after["revokedAt"])
	}
	// (1c) revokedBySubject recorded.
	if after["revokedBySubject"] != ownerUID {
		t.Fatalf("#1729: revokedBySubject must be recorded; got %v", after["revokedBySubject"])
	}
	// (1d) read-merge preserved every omitted discriminator field.
	for _, f := range []string{"identityId", "identitySubject", "identityType", "agentId", "roleCeiling", "createdBySubject"} {
		if fmt.Sprintf("%v", after[f]) != fmt.Sprintf("%v", before[f]) {
			t.Fatalf("#1729: omitted field %q not preserved across revoke read-merge: before=%v after=%v", f, before[f], after[f])
		}
	}

	// (2) excluded from the active-delegation query -- the revoke took effect.
	activeAfter := asArray(t, e.runQuery(t, "queryActiveDelegationsForAgent", map[string]any{"agentId": agentID}))
	if containsID(activeAfter, storedID) {
		t.Fatalf("#1729: revoked delegation %s still returned by queryActiveDelegationsForAgent; got %v", storedID, idIDs(activeAfter))
	}

	t.Logf("#1729: revoke forced active=false + revokedAt=%s with no caller-supplied terminal state, preserved siblings, and excluded the row from the active query", revokedAt)
}
