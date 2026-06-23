package conformance

// conf_1728_test.go -- regression dimension for memql#1728.
//
// delegationsByIdentity is documented as returning "all delegations
// (active and revoked) for a given identity ID" -- it is the delegation-history
// audit query. It used to carry an `&& isActiveRecord` predicate, which
// silently filtered out every revoked row, so after revoking a delegation the
// audit query returned []. The fix drops that predicate so the name, the doc,
// and the predicate agree (active AND revoked rows come back).
//
// The active-only behaviour still has a separate home:
// activeDelegationsByIdentitySubject (the DelegationResolver hot-path
// query) must continue to EXCLUDE the revoked row. This test asserts both
// halves -- inclusion in the all-rows query, exclusion from the active-only
// query -- so a future re-introduction of the active filter on the audit query
// fails here.

import (
	"testing"
)

func TestConf1728DelegationsByIdentityIncludesRevoked(t *testing.T) {
	e := newEnv(t)
	if !e.HasDB {
		t.Skip("requires Postgres (set MEMORY_NODES_DATABASE_DSN); validated in the CI conformance job")
	}

	sfx := uniqueSuffix("1728")
	delegationID := "deleg-1728-" + sfx
	identityID := "identity-1728-" + sfx
	identitySubject := "subject-1728-" + sfx
	agentID := "agent-1728-" + sfx

	// CREATE -- a live delegation for this identity.
	createOut := e.runMutation(t, "createDelegation", map[string]any{
		"delegationId":     delegationID,
		"identityId":       identityID,
		"identitySubject":  identitySubject,
		"identityType":     "human",
		"agentId":          agentID,
		"roleCeiling":      "writer",
		"scopes":           []any{"query:*"},
		"createdBySubject": ownerUID,
		"note":             "memql#1728 conformance fixture",
	})
	storedID := storedID(t, createOut)

	// While active: present in BOTH the all-rows audit query (by identityId)
	// and the active-only resolver query (by identitySubject).
	byIdentity := asArray(t, e.runQuery(t, "delegationsByIdentity", map[string]any{"identityId": identityID}))
	if !containsID(byIdentity, storedID) {
		t.Fatalf("#1728 pre-revoke: active delegation %s missing from delegationsByIdentity; got %v", storedID, idIDs(byIdentity))
	}
	activeBefore := asArray(t, e.runQuery(t, "activeDelegationsByIdentitySubject", map[string]any{"identitySubject": identitySubject}))
	if !containsID(activeBefore, storedID) {
		t.Fatalf("#1728 pre-revoke: active delegation %s missing from activeDelegationsByIdentitySubject; got %v", storedID, idIDs(activeBefore))
	}

	// REVOKE -- revokeDelegation authoritatively forces active=false
	// and stamps revokedAt regardless of caller input (memql#1729); the caller
	// supplies only the revoker subject. The engine read-merge preserves every
	// other required field from the stored row.
	e.runMutation(t, "revokeDelegation", map[string]any{
		"delegationId":     storedID,
		"revokedBySubject": ownerUID,
	})

	// Ground-truth: the latest version really is inactive.
	after := e.latestPayload(t, "v1:identity:delegation", storedID)
	if after["active"] != false {
		t.Fatalf("#1728: revoke must flip active=false; got %v", after["active"])
	}

	// INCLUSION (the bug fix): the audit query STILL returns the revoked row.
	byIdentityAfter := asArray(t, e.runQuery(t, "delegationsByIdentity", map[string]any{"identityId": identityID}))
	if !containsID(byIdentityAfter, storedID) {
		t.Fatalf("#1728 REGRESSION: delegationsByIdentity dropped revoked delegation %s -- the active-only predicate is back; got %v", storedID, idIDs(byIdentityAfter))
	}

	// EXCLUSION (the contract that must NOT change): the active-only resolver
	// query no longer returns the revoked row.
	activeAfter := asArray(t, e.runQuery(t, "activeDelegationsByIdentitySubject", map[string]any{"identitySubject": identitySubject}))
	if containsID(activeAfter, storedID) {
		t.Fatalf("#1728: activeDelegationsByIdentitySubject leaked revoked delegation %s; got %v", storedID, idIDs(activeAfter))
	}

	t.Logf("#1728: revoked delegation %s included in delegationsByIdentity (all-rows) and excluded from activeDelegationsByIdentitySubject (active-only)", storedID)
}
