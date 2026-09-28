package auth

import (
	"context"
	"testing"
)

// An actor id names a PERSON only when the identity service minted it. Every
// actor the engine synthesizes carries a prefix that says so, and a caller
// holding only the string -- a router request, a ledger row -- must be able to
// tell the two apart without the context it came from.
func TestNamesNoPersonTellsSynthesizedActorsFromPeople(t *testing.T) {
	for _, tc := range []struct {
		id     string
		person bool
	}{
		{"", false},
		{"   ", false},
		{SystemActor("edge").UserId, false},
		{"system:automation:workerAppSessionStaleSweep", false},
		{MaintenanceActor("auditEventRetentionSweep").UserId, false},
		{ConnectorActor("shopify").UserId, false},
		{AnonymousUserId, false},
		// The cluster's own principals: the proving suite, the work journal
		// and the operator credential's stream subject.
		{"cluster:proving-suite", false},
		{"cluster:work-journal", false},
		{"cluster:operator", false},
		{"a248d524-1791-45a7-a537-4a65b5aabdb4", true},
		{"v1:identity:user:a248d524-1791-45a7-a537-4a65b5aabdb4", true},
		// A person's id that merely CONTAINS a synthetic word is a person.
		{"v1:cluster:node:bff-local", true},
	} {
		if got := NamesNoPerson(tc.id); got == tc.person {
			t.Errorf("NamesNoPerson(%q) = %v, want %v", tc.id, got, !tc.person)
		}
	}
}

// WHEN THE CONTEXT IS AT HAND, THE ACTOR'S OWN FLAG ANSWERS TOO. A synthetic
// actor -- the cluster acting, never a row's owner -- names nobody whatever
// its id's prefix, so a principal minted tomorrow under a prefix the string
// test does not know is still nobody. Borrowed authority is NOT synthetic: its
// UserId is a real person's, and a check that refused it would shut the app
// door on every call a trusted automation makes for its owner.
func TestActsForNoPersonReadsTheSyntheticFlag(t *testing.T) {
	synthetic := ContextWithAccess(context.Background(), &AccessContext{UserId: "bench:tomorrow", Role: RoleOwner, Synthetic: true})
	if !ActsForNoPerson(synthetic, "bench:tomorrow") {
		t.Error("a synthetic actor with an unknown prefix was taken for a person")
	}
	// The call names a person explicitly while the context is the cluster's:
	// the explicit person is who the call acts for.
	if ActsForNoPerson(synthetic, "v1:identity:user:alice") {
		t.Error("a call that names a person was refused because its context is synthetic")
	}
	borrowed := ContextWithUserActor(context.Background(), "v1:identity:user:alice")
	if ActsForNoPerson(borrowed, "v1:identity:user:alice") {
		t.Error("borrowed authority was taken for nobody; it acts for the person it borrows")
	}
	if !ActsForNoPerson(context.Background(), "system:automation:sweep") {
		t.Error("with no actor on the context the id alone must still decide")
	}
}
