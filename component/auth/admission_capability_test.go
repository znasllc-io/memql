package auth

import "testing"

// ADMITTING SOMEBODY IS NOT MANAGING THEM (epic developer-admission, memql#4917).
//
// A developer helping an owner stand a cluster up must be able to get
// colleagues in. The authority to do that is create-on-`admission`: hand
// somebody a credential that lets them into this cluster, meaning a user
// invitation or an enrolment link.
//
// It is deliberately NOT create-on-`principal`, which is the user-management
// grant `AtLeastAdmin` tests and which answers for fourteen operations
// including role changes, suspensions and recovery-key rotation.

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

// THE POINT OF A SEPARATE RESOURCE. If admission were expressed by widening
// create-on-principal, a developer would gain the whole identity-admin
// surface. This pins the two apart: developer admits, and manages nothing.
func TestAdmissionDoesNotGrantDeveloperUserManagement(t *testing.T) {
	dev := UserContext{Role: RoleDeveloper}

	if !CanAdmitPeople(dev) {
		t.Fatal("developer cannot admit people, so the rest of this test proves nothing")
	}
	if AtLeastAdmin(dev) {
		t.Error("developer passes AtLeastAdmin -- admission has leaked into the user-management gate")
	}
	for _, verb := range []string{VerbCreate, VerbUpdate, VerbDelete} {
		if Capable(RoleDeveloper, verb, ResourcePrincipal) {
			t.Errorf("developer holds %s on principal -- it may now edit accounts, not just admit them", verb)
		}
	}
}

// RANK IS NOT AUTHORITY, AND THE ONE PAIR WHERE THEY DISAGREE IS THE DANGEROUS
// ONE (code review of memql#4917).
//
// developer ranks 300 and admin 200, so "the actor strictly outranks the
// target" admits developer -> admin. But the capability sets are NON-MONOTONIC
// in rank at exactly that rung: admin holds create, update and delete on
// `principal` and developer holds only read.
//
// That gap is a two-move path to owner. Mint a credential for an existing
// admin (an enrolment link, which registers a passkey as them), sign in as
// that admin, then use SetUserRole -- which caps nothing -- to make anybody an
// owner. So a credential-minting gate cannot ask "do I outrank them"; it has
// to ask "do they hold people-authority I do not".
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
		{RoleDeveloper, RoleAdmin, true, "admin holds create/update/delete on principal; developer holds read"},
		{RoleDeveloper, RoleOwner, true, "owner holds the full principal verbs"},
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
