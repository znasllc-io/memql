package memql

import (
	"strings"
	"testing"
)

// agentauthz_rowauthz_3177_db_test.go -- memql#3177, end to end against a real
// store.
//
// Postgres-gated like its neighbours: readMergeTestEngine skips when no DB is
// reachable. CI's db-tests lane runs this package with MEMQL_REQUIRE_DB=1, so a
// skip there is a failure rather than a green.
//
// The claim that can only be made HERE is that the write does not LAND. The
// unit tests in agentauthz_rowauthz_3177_test.go prove the guard answers
// correctly and the template carries the stamp; these three prove that the
// engine consults both on the real mutation path, in the exact call shape the
// generated SDK sends.
//
// The rows under test are standing grants: `computerUseScope` is the ceiling on
// what an agent may do on the user's own machine, and `skillTierAllowlist`
// decides which skill tiers the planner may mint at unattended. So "the write
// did not land" is the whole security property, not an implementation detail.

// grantCtx is an ordinary authenticated caller, the shape the request path
// builds: an AccessContext (which actor.userId resolves from) plus a TokenInfo
// (which the mutation executor attributes writes to).
//
// rowAuthzCallerCtx already builds exactly that; named through here so these
// tests read in their own terms.
var grantCtx = rowAuthzCallerCtx

// THE memql#3129 BEHAVIOUR CHANGE, end to end: A cannot revoke B's standing
// grant, and B's row survives the attempt untouched.
//
// `revokeAgentAuthorization` is `update { id: args.authId; active: false }` --
// a caller-supplied target and nothing relating it to the caller. Before the
// concept declared a tier, knowing an authId was sufficient to switch off
// somebody else's standing authorization.
func TestRevokingAnotherCallersGrantIsRefusedEndToEnd(t *testing.T) {
	eng, db, _ := readMergeTestEngine(t)

	suffix := uniqueSuffix("rowauthz3177revoke")
	userA := "user-a-" + suffix
	userB := "user-b-" + suffix
	authB := "auth-b-" + suffix

	ctxA := grantCtx(userA)
	ctxB := grantCtx(userB)

	// createAgentAuthorization stamps userId from actor.userId (memql#3081), so
	// the row is genuinely B's. A create is unguarded by design -- there is no
	// target row to resolve an owner from.
	storedId := runMutation(t, ctxB, eng, "createAgentAuthorization", map[string]any{
		"authId": authB, "agentId": "v1:agents:agent:some-agent",
		"planKind": "*", "spaceScope": "*", "computerUseScope": "full",
	})

	before := latestPayload(t, ctxB, db, agentAuthzConcept, storedId)
	// sameRowAuthzOwner, not `!=`: `userId` is an outgoing @relationship,
	// so the STORED value is canonical (`v1:identity:user:<userB>`) while
	// userB is the bare form the actor envelope carries (memql#3172).
	if owner, _ := before["userId"].(string); !sameRowAuthzOwner(owner, userB) {
		t.Fatalf("the fixture row is owned by %v, not %s -- the create did not stamp the "+
			"actor and this test would prove nothing", before["userId"], userB)
	}

	_, err := eng.Execute(ctxA, `mutation revokeAgentAuthorization(authId: "`+authB+`")`)
	if err == nil {
		t.Fatal("caller A revoked caller B's standing authorization. Revocation is the " +
			"user's own control over an agent's autonomy; anyone able to fire it for a " +
			"stranger can silently switch off their grants (memql#3129).")
	}
	if !strings.Contains(err.Error(), "row-authz") {
		t.Fatalf("the write failed for the wrong reason: %v", err)
	}

	after := latestPayload(t, ctxB, db, agentAuthzConcept, storedId)
	if after["active"] != true {
		t.Fatalf("the grant is no longer active (%v) after a REFUSED revoke", after["active"])
	}
	if after["userId"] != userB {
		t.Fatalf("the row's owner is now %v, not %s", after["userId"], userB)
	}
}

// The same for the update path, and this one is the elevation rather than the
// denial: `updateAgentAuthorization` splats a caller payload, so an unguarded
// write could set `computerUseScope: "full"` and `skillTierAllowlist` on
// somebody else's grant.
func TestUpdatingAnotherCallersGrantIsRefusedEndToEnd(t *testing.T) {
	eng, db, _ := readMergeTestEngine(t)

	suffix := uniqueSuffix("rowauthz3177update")
	userA := "user-a-" + suffix
	userB := "user-b-" + suffix
	authB := "auth-b-" + suffix

	ctxA := grantCtx(userA)
	ctxB := grantCtx(userB)

	// B's grant is deliberately NARROW -- the attack is widening it.
	storedId := runMutation(t, ctxB, eng, "createAgentAuthorization", map[string]any{
		"authId": authB, "agentId": "v1:agents:agent:some-agent",
		"planKind": "*", "spaceScope": "*", "computerUseScope": "",
	})

	_, err := eng.Execute(ctxA, `mutation updateAgentAuthorization(authId: "`+authB+
		`", payload: {"computerUseScope": "full", "skillTierAllowlist": ["A", "B", "C"]})`)
	if err == nil {
		t.Fatal("caller A wrote caller B's standing authorization. computerUseScope is the " +
			"ceiling on what an agent may do on the user's own machine, so this is a " +
			"privilege grant on somebody else's account (memql#3129).")
	}
	if !strings.Contains(err.Error(), "row-authz") {
		t.Fatalf("the write failed for the wrong reason: %v", err)
	}

	after := latestPayload(t, ctxB, db, agentAuthzConcept, storedId)
	if got := after["computerUseScope"]; got != "" {
		t.Fatalf("the refused write still widened the grant: computerUseScope is %v", got)
	}
	if _, widened := after["skillTierAllowlist"].([]any); widened {
		if len(after["skillTierAllowlist"].([]any)) > 0 {
			t.Fatalf("the refused write still set skillTierAllowlist: %v",
				after["skillTierAllowlist"])
		}
	}
}

// THE memql#3138 BEHAVIOUR CHANGE, end to end. This is the half the write guard
// CANNOT catch, and the reason the stamp had to land with the declaration.
//
// The attacker updates a row they legitimately own, so the guard admits the
// write -- correctly. What must not happen is the row changing hands: the
// payload names a `userId`, and before #3177 that value landed verbatim,
// handing the victim a standing `full` ceiling on a grant they never approved
// while the attacker kept control of the id.
//
// The legitimate part of the same write is asserted too. A fix that dropped the
// caller's fields along with the forged owner would break the SPA's "Approve &
// always allow this tier" action, which writes the whole skillTierAllowlist
// array back through this mutation.
func TestAGrantCannotChangeHandsThroughItsPayloadEndToEnd(t *testing.T) {
	eng, db, _ := readMergeTestEngine(t)

	suffix := uniqueSuffix("rowauthz3177reassign")
	attacker := "user-attacker-" + suffix
	victim := "user-victim-" + suffix
	authId := "auth-attacker-" + suffix

	ctxAttacker := grantCtx(attacker)

	storedId := runMutation(t, ctxAttacker, eng, "createAgentAuthorization", map[string]any{
		"authId": authId, "agentId": "v1:agents:agent:some-agent",
		"planKind": "*", "spaceScope": "*", "computerUseScope": "full",
	})

	// Step 2 of the escalation, on the attacker's OWN row.
	if _, err := eng.Execute(ctxAttacker, `mutation updateAgentAuthorization(authId: "`+authId+
		`", payload: {"userId": "`+victim+`", "skillTierAllowlist": ["A", "B"]})`); err != nil {
		t.Fatalf("the caller's write onto their OWN grant was refused: %v.\n"+
			"That is not the defence against this attack -- the defence is the owner "+
			"re-stamp -- and refusing here would break self-service entirely", err)
	}

	after := latestPayload(t, ctxAttacker, db, agentAuthzConcept, storedId)
	// sameRowAuthzOwner for the same reason as the fixture assertion
	// above: the stored `userId` is canonical, the bare form is what the
	// caller is identified by (memql#3172).
	if owner, _ := after["userId"].(string); !sameRowAuthzOwner(owner, attacker) {
		t.Fatalf(`the grant changed hands: userId is %v, want %s.

A caller-supplied payload reassigned the row to somebody else while keeping
computerUseScope=%v, which hands that user a standing ceiling they never
approved and is exactly memql#3138. The overlay re-stamp in
updateAgentAuthorization is what must have overwritten it.`,
			after["userId"], attacker, after["computerUseScope"])
	}
	// The rest of the payload still landed: the re-stamp overwrites the owner
	// field alone.
	tiers, ok := after["skillTierAllowlist"].([]any)
	if !ok || len(tiers) != 2 {
		t.Fatalf("the caller's own legitimate field did not land: skillTierAllowlist is %v.\n"+
			"The owner re-stamp must overwrite `userId` and nothing else, or the SPA's "+
			"'Approve & always allow this tier' action stops working.",
			after["skillTierAllowlist"])
	}
}
