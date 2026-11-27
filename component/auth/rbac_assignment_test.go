package auth

import "testing"

// THE ASSIGNMENT MATRIX (epic memql#5166, D4).
//
// One cell per row, because "who may give whom which role" is the decision this
// epic exists to make expressible for a custom role, and a rule asserted only
// on its happy path is a rule with an untested refusal.

// assignmentCatalog is the ladder every case below is decided against: the five
// seeded rungs plus a custom `support-lead` at 150 and a scoped `acct-lead`.
func withAdmission(set map[VerbResource]bool) map[VerbResource]bool {
	set[VerbResource{Verb: VerbCreate, Resource: ResourceAdmission}] = true
	return set
}

func assignmentCatalog() *fakeCatalog {
	principal := func(verbs ...string) map[VerbResource]bool {
		out := map[VerbResource]bool{}
		for _, v := range verbs {
			out[VerbResource{Verb: v, Resource: ResourcePrincipal}] = true
		}
		return out
	}
	return &fakeCatalog{
		ranks: map[string]int{
			"owner": 400, "developer": 300, "admin": 200,
			"user": 100, "writer": 100, "viewer": 50, "reader": 50,
			"support-lead": 150, "acct-lead": 140,
		},
		names: map[string]string{
			"owner": "Owner", "developer": "Developer", "admin": "Admin",
			"user": "Member", "viewer": "Viewer",
			"support-lead": "Support Lead", "acct-lead": "Account Lead",
		},
		scopes: map[string]string{"acct-lead": "acct-1"},
		grants: map[string]map[VerbResource]bool{
			// ADMISSION IS PART OF THE FIXTURE, not decoration: it is the grant
			// that lets a developer invite people while holding no
			// update-on-principal, and it is exactly the pair MayAssignRole
			// branches on. A fixture that omitted it would make every developer
			// invitation refuse for the wrong reason and the test would agree.
			"owner":        withAdmission(principal(VerbRead, VerbCreate, VerbUpdate, VerbDelete)),
			"admin":        withAdmission(principal(VerbRead, VerbCreate, VerbUpdate, VerbDelete)),
			"developer":    withAdmission(principal(VerbRead)),
			"support-lead": withAdmission(principal(VerbRead, VerbCreate, VerbUpdate)),
			"acct-lead":    principal(VerbRead, VerbUpdate),
			"user":         {},
			"viewer":       {},
		},
	}
}

func TestMayAssignRole(t *testing.T) {
	installFake(t, assignmentCatalog())

	member := func(string) bool { return true }
	notMember := func(string) bool { return false }

	cases := []struct {
		name          string
		callerRole    Role
		callerId      string
		targetId      string
		targetCurrent string
		newRole       string
		isMember      func(string) bool
		want          AssignRefusal
	}{
		{
			name:       "owner may re-role an admin to developer",
			callerRole: RoleOwner, callerId: "u-owner", targetId: "u-a", targetCurrent: "admin",
			newRole: "developer", want: AssignAllowed,
		},
		{
			name:       "owner must transfer the single ownership slot",
			callerRole: RoleOwner, callerId: "u-owner", targetId: "u-b", targetCurrent: "user",
			newRole: "owner", want: AssignOwnershipTransfer,
		},
		{
			name:       "admin may not mint a developer -- 300 is not below 200",
			callerRole: RoleAdmin, callerId: "u-adm", targetId: "u-c", targetCurrent: "writer",
			newRole: "developer", want: AssignAboveCaller,
		},
		{
			name:       "admin may not mint a peer admin",
			callerRole: RoleAdmin, callerId: "u-adm", targetId: "u-c", targetCurrent: "writer",
			newRole: "admin", want: AssignAboveCaller,
		},
		{
			name:       "admin may not touch a developer -- the target outranks them",
			callerRole: RoleAdmin, callerId: "u-adm", targetId: "u-d", targetCurrent: "developer",
			newRole: "writer", want: AssignTargetOutranks,
		},
		{
			name:       "admin may demote a member to viewer",
			callerRole: RoleAdmin, callerId: "u-adm", targetId: "u-c", targetCurrent: "writer",
			newRole: "reader", want: AssignAllowed,
		},
		{
			name:       "developer may not RE-ROLE anybody -- it holds no update on principal",
			callerRole: RoleDeveloper, callerId: "u-dev", targetId: "u-c", targetCurrent: "writer",
			newRole: "viewer", want: AssignNotAUserManager,
		},
		{
			// REVERSED DELIBERATELY (memql#5236), and the argument it used to
			// carry is still true: developer (300) outranks admin (200) and holds
			// strictly fewer principal verbs, so a developer can invite an address
			// they control AS an admin and act through it.
			//
			// The cluster owner accepted that knowingly, because the clause was
			// taking back most of the create-on-admission the seeds grant a
			// developer on purpose, and `admin` is the rung somebody standing a
			// cluster up most often needs to hand out. The people-authority clause
			// now runs on AssignOnReRole only; the mitigation (a second sign-off,
			// an owner notification, or an explicit admission ceiling) is a
			// recorded open question, not an oversight.
			//
			// The sibling case below is what keeps this honest: the SAME pair on
			// the re-role seam is still refused.
			name:       "developer may invite an admin -- admitting is not wielding",
			callerRole: RoleDeveloper, callerId: "u-dev", targetId: "", targetCurrent: "",
			newRole: "admin", want: AssignAllowed,
		},
		{
			// The capability a developer DOES hold is create-on-admission, so
			// it may invite below itself. Taking that away through this
			// function would remove invitations from every developer in every
			// cluster (memql#4917).
			name:       "developer may invite a member",
			callerRole: RoleDeveloper, callerId: "u-dev", targetId: "", targetCurrent: "",
			newRole: "writer", want: AssignAllowed,
		},
		{
			name:       "an invitation at the inviter's own rung is refused -- D4 is strictly below",
			callerRole: RoleAdmin, callerId: "u-adm", targetId: "", targetCurrent: "",
			newRole: "admin", want: AssignAboveCaller,
		},
		{
			name:       "a member may not assign anything",
			callerRole: RoleWriter, callerId: "u-c", targetId: "u-e", targetCurrent: "reader",
			newRole: "reader", want: AssignNotAUserManager,
		},
		{
			name:       "an unknown slug is refused before anything else is asked",
			callerRole: RoleOwner, callerId: "u-owner", targetId: "u-c", targetCurrent: "writer",
			newRole: "ghost", want: AssignUnknownRole,
		},
		{
			name:       "a custom rung below the caller is assignable",
			callerRole: RoleAdmin, callerId: "u-adm", targetId: "u-c", targetCurrent: "writer",
			newRole: "support-lead", want: AssignAllowed,
		},
		{
			name:       "a scoped role needs the target to be a member",
			callerRole: RoleAdmin, callerId: "u-adm", targetId: "u-c", targetCurrent: "writer",
			newRole: "acct-lead", isMember: member, want: AssignAllowed,
		},
		{
			name:       "a scoped role is refused for a non-member",
			callerRole: RoleAdmin, callerId: "u-adm", targetId: "u-c", targetCurrent: "writer",
			newRole: "acct-lead", isMember: notMember, want: AssignNotAMember,
		},
		{
			// FAIL-CLOSED ON A MISSING MEMBERSHIP READER. A caller that cannot
			// answer the question is not a caller whose answer is yes.
			name:       "a scoped role is refused when membership cannot be resolved",
			callerRole: RoleAdmin, callerId: "u-adm", targetId: "u-c", targetCurrent: "writer",
			newRole: "acct-lead", isMember: nil, want: AssignNotAMember,
		},
		{
			name:       "an alias resolves on both sides",
			callerRole: RoleAdmin, callerId: "u-adm", targetId: "u-c", targetCurrent: "writer",
			newRole: "user", want: AssignAllowed,
		},
		{
			// SELF. GovernPrincipal admits a self-edit, and the new-rank bound
			// still applies -- an admin cannot re-name themselves admin,
			// because 200 is not below 200. That reads oddly until you notice
			// it is a no-op nobody needs and a step nobody should be able to
			// take toward a higher rung.
			name:       "an admin cannot re-assign their own rung",
			callerRole: RoleAdmin, callerId: "u-adm", targetId: "u-adm", targetCurrent: "admin",
			newRole: "admin", want: AssignAboveCaller,
		},
		{
			name:       "an admin may demote themselves",
			callerRole: RoleAdmin, callerId: "u-adm", targetId: "u-adm", targetCurrent: "admin",
			newRole: "writer", want: AssignAllowed,
		},
		{
			name:       "nobody but an owner may re-role an owner",
			callerRole: RoleAdmin, callerId: "u-adm", targetId: "u-owner", targetCurrent: "owner",
			newRole: "writer", want: AssignTargetOutranks,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Every case in this table was authored when an empty
			// targetCurrent WAS the invitation signal, so deriving the kind
			// here preserves each case's original meaning exactly.
			// TestAssignmentCrossProduct pins the two kinds explicitly.
			kind := AssignOnReRole
			if tc.targetCurrent == "" {
				kind = AssignOnInvitation
			}
			got := MayAssignRole(
				UserContext{ID: tc.callerId, Role: tc.callerRole},
				kind,
				tc.targetId, tc.targetCurrent, tc.newRole, tc.isMember,
			)
			if got != tc.want {
				t.Fatalf("MayAssignRole(caller=%s, targetNow=%s, new=%s) = %q, want %q",
					tc.callerRole, tc.targetCurrent, tc.newRole, got, tc.want)
			}
		})
	}
}

// TestMayAssignRoleWithNoCatalogUsesTheCompiledLadder covers the boot window:
// the identity service issues invitations before the catalog is readable, and a
// rule that refused everything there would make a fresh cluster unusable.
func TestMayAssignRoleWithNoCatalogUsesTheCompiledLadder(t *testing.T) {
	SetCapabilityCatalog(nil)

	if got := MayAssignRole(UserContext{ID: "u-owner", Role: RoleOwner}, AssignOnInvitation, "u-a", "", "admin", nil); got != AssignAllowed {
		t.Fatalf("owner inviting an admin on a catalog-less node = %q, want allowed", got)
	}
	if got := MayAssignRole(UserContext{ID: "u-adm", Role: RoleAdmin}, AssignOnInvitation, "u-a", "", "developer", nil); got != AssignAboveCaller {
		t.Fatalf("admin inviting a developer on a catalog-less node = %q, want %q", got, AssignAboveCaller)
	}
}

// TestRoleAtMostCapsByRankAndNeverWidensPeopleAuthority.
//
// RoleAtMost caps an agent's effective role at the delegation ceiling, and the
// naive rank comparison ESCALATES on one pair. developer (300) with a ceiling of
// admin (200) reads as "admin is more restrictive" by rank alone -- and admin
// holds create, update and delete on `principal` while developer holds only
// read, so capping there would GRANT user management to an agent whose
// delegator has none.
func TestRoleAtMostCapsByRankAndNeverWidensPeopleAuthority(t *testing.T) {
	installFake(t, assignmentCatalog())

	cases := []struct {
		identity, ceiling, want Role
	}{
		{RoleOwner, RoleReader, RoleReader},
		{RoleReader, RoleOwner, RoleReader},
		{RoleWriter, RoleReader, RoleReader},
		{RoleAdmin, RoleWriter, RoleWriter},
		// The pair that matters: the ceiling ranks lower and holds MORE
		// people-authority, so the identity stands.
		{RoleDeveloper, RoleAdmin, RoleDeveloper},
		{RoleAdmin, RoleDeveloper, RoleAdmin},
		// A custom rung caps correctly, which the coarse RoleLevel scale --
		// where every custom role collapses to "least privileged" -- could not
		// express.
		{Role("support-lead"), Role("acct-lead"), Role("acct-lead")},
		{Role("acct-lead"), Role("support-lead"), Role("acct-lead")},
	}
	for _, tc := range cases {
		if got := RoleAtMost(tc.identity, tc.ceiling); got != tc.want {
			t.Errorf("RoleAtMost(%q, %q) = %q, want %q", tc.identity, tc.ceiling, got, tc.want)
		}
	}
}
