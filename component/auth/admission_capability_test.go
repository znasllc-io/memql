package auth

import (
	"context"
	"testing"
)

// ADMITTING SOMEBODY IS NOT MANAGING THEM (epic developer-admission, memql#4917).
//
// A developer helping an owner stand a cluster up must be able to get
// colleagues in. The authority to do that is create-on-`admission`: hand
// somebody a credential that lets them into this cluster, meaning a user
// invitation or an enrolment link.
//
// It is its own resource rather than create-on-`principal`, the
// user-management grant `AtLeastAdmin` tests, so a custom role can invite
// people without managing them. The developer role itself holds both from
// 2026-11-25 (design record 2026-11-25, developer access).

func TestCanAdmitPeopleHoldsForOwnerAdminAndDeveloper(t *testing.T) {
	for _, tc := range []struct {
		role Role
		want bool
	}{
		{RoleOwner, true},
		{RoleAdmin, true},
		{RoleDeveloper, true},
		{RoleWriter, false},
		{RoleReader, false},
		{Role("wizard"), false},
		{Role(""), false},
	} {
		if got := CanAdmitPeople(UserContext{Role: tc.role}); got != tc.want {
			t.Errorf("CanAdmitPeople(%q) = %v, want %v", tc.role, got, tc.want)
		}
	}
}

// A DEVELOPER MANAGES PEOPLE BELOW IT AND NEVER AN OWNER (the owner's
// direction, 2026-11-25: "a developer should be able to do almost everything
// an owner does", except the owner's own functions and information).
//
// This test used to pin the opposite -- developer admits, and manages nothing
// -- and the verbs it refused are the ones the developer role now holds. What
// it must keep pinning is the half that did not change: the verb is not what
// protects an owner, the rank tree is. So both halves are asserted here, and a
// change that seeds the verbs while loosening GovernPrincipal fails this test.
func TestADeveloperManagesPeopleBelowItAndNeverAnOwner(t *testing.T) {
	dev := UserContext{Role: RoleDeveloper}

	if !CanAdmitPeople(dev) || !AtLeastAdmin(dev) {
		t.Fatal("developer does not hold admission and user management")
	}
	for _, verb := range []string{VerbCreate, VerbUpdate, VerbDelete} {
		if !CapableFor(context.Background(), Subject{Role: RoleDeveloper}, verb, ResourcePrincipal) {
			t.Errorf("developer does not hold %s on principal", verb)
		}
	}

	actor := Principal{UserId: "dev", Rank: RoleRank(RoleDeveloper)}
	for _, tc := range []struct {
		target Role
		want   bool
	}{
		{RoleOwner, false},     // an owner is an owner's alone
		{RoleDeveloper, false}, // peers do not outrank each other
		{RoleAdmin, true},
		{RoleWriter, true},
		{RoleReader, true},
	} {
		target := Principal{UserId: "other", Rank: RoleRank(tc.target), IsOwner: tc.target == RoleOwner}
		for _, verb := range []GovernVerb{GovernUpdate, GovernDelete} {
			if got := GovernPrincipal(actor, target, verb); got != tc.want {
				t.Errorf("GovernPrincipal(developer, %s, %s) = %v, want %v", tc.target, verb, got, tc.want)
			}
		}
	}
	// And a developer cannot name a role at or above its own, on either seam.
	for _, slug := range []string{"owner", "developer"} {
		if MayAssignRole(dev, AssignOnReRole, "other", "writer", slug, nil) == AssignAllowed {
			t.Errorf("a developer may re-role somebody to %s", slug)
		}
		if MayAssignRole(dev, AssignOnInvitation, "", "", slug, nil) == AssignAllowed {
			t.Errorf("a developer may invite somebody as %s", slug)
		}
	}
	if refusal := MayAssignRole(dev, AssignOnReRole, "other", "writer", "admin", nil); refusal != AssignAllowed {
		t.Errorf("a developer may not re-role a writer to admin: %s", refusal)
	}
}

// RANK IS NOT AUTHORITY (code review of memql#4917).
//
// Until 2026-11-25 the predefined roles carried the dangerous pair themselves:
// developer ranked 300 above admin's 200 and held only read on `principal`,
// so "the actor strictly outranks the target" admitted developer -> admin
// while admin held the people-authority developer lacked -- a two-move path to
// owner through a minted credential. The developer role now holds every
// principal verb (design record 2026-11-25, developer access), so no
// PREDEFINED pair disagrees any more and the developer rows below answer
// false. The predicate stays for the roles an operator authors: a custom role
// may rank above another and hold fewer principal verbs, and a
// credential-minting gate still has to ask "do they hold people-authority I
// do not" rather than "do I outrank them".
//
// It is scoped to `principal` rather than to every resource on purpose. Full
// containment refuses developer -> writer (writer holds read:group, developer
// does not) and even owner -> developer (developer holds read:agent), which
// would break the feature this epic exists for.
func TestPrincipalAuthorityBeyondCatchesTheRankInversion(t *testing.T) {
	for _, tc := range []struct {
		actor, target Role
		want          bool
		why           string
	}{
		{RoleDeveloper, RoleAdmin, false, "both hold the same four principal verbs"},
		{RoleDeveloper, RoleOwner, false, "both hold the same four; the owner carve-out is what refuses this"},
		{RoleWriter, RoleAdmin, true, "admin holds the principal verbs; writer holds none"},
		{RoleDeveloper, RoleWriter, false, "writer holds no principal verb at all"},
		{RoleDeveloper, RoleReader, false, "reader holds no principal verb at all"},
		{RoleAdmin, RoleReader, false, "reader holds no principal verb at all"},
		{RoleAdmin, RoleOwner, false, "both hold the same four; the owner carve-out is what refuses this"},
		{RoleOwner, RoleDeveloper, false, "an owner holds every principal verb developer does"},
		{RoleOwner, RoleAdmin, false, "an owner holds every principal verb admin does"},
	} {
		if got := GrantsPrincipalAuthorityBeyond(tc.actor, tc.target); got != tc.want {
			t.Errorf("GrantsPrincipalAuthorityBeyond(%q, %q) = %v, want %v -- %s",
				tc.actor, tc.target, got, tc.want, tc.why)
		}
	}
}
