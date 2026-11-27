package auth

import "testing"

// ADMITTING SOMEBODY IS NOT WIELDING THEIR POWERS (memql#5236).
//
// developer ranks 300 and admin 200, so the rank cap ALLOWS this. What refused
// it was GrantsPrincipalAuthorityBeyond -- admin holds create/update/delete on
// `principal` and developer holds only read -- applied identically to both
// seams that assign a role. Re-roling hands somebody powers now; an invitation
// opens a door they must still walk through, and the inviter still cannot
// touch the account that results.
func TestDeveloperMayInviteAnAdmin(t *testing.T) {
	installFake(t, assignmentCatalog())

	if got := MayAssignRole(
		UserContext{ID: "u-dev", Role: RoleDeveloper},
		AssignOnInvitation,
		"", "", "admin", nil,
	); got != AssignAllowed {
		t.Fatalf("developer inviting an admin = %q, want allowed", got)
	}
}

// THE WHOLE MATRIX, PER KIND -- the specification of the split and the guard
// against over-applying it.
//
// The failure this pins against is not "the exemption is missing" (one case
// would catch that) but "the exemption leaked onto the re-role seam", which is
// invisible unless both kinds are asserted over the same pairs. A reviewer
// should be able to read the two tables against each other and see exactly
// which cells the kind changes.
func TestAssignmentCrossProduct(t *testing.T) {
	installFake(t, assignmentCatalog())

	inviters := []Role{RoleOwner, RoleDeveloper, RoleAdmin, "user", "viewer", "no-such-role"}
	granted := []string{"owner", "developer", "admin", "user", "viewer"}

	// Read as [inviter][granted]. The re-role table's target sits at `viewer`,
	// the bottom rung, so the TARGET half of the rule passes for every caller
	// and each cell isolates the new-role bound and the capability half.
	want := map[AssignKind]map[Role]map[string]AssignRefusal{
		AssignOnInvitation: {
			RoleOwner: {
				"owner": AssignOwnershipTransfer, "developer": AssignAllowed, "admin": AssignAllowed,
				"user": AssignAllowed, "viewer": AssignAllowed,
			},
			// THE ROW THIS ISSUE CHANGED. `admin` was AssignAuthorityBeyond.
			RoleDeveloper: {
				"owner": AssignOwnershipTransfer, "developer": AssignAboveCaller, "admin": AssignAllowed,
				"user": AssignAllowed, "viewer": AssignAllowed,
			},
			// D4's peer rule, untouched: an admin cannot mint a peer admin.
			RoleAdmin: {
				"owner": AssignOwnershipTransfer, "developer": AssignAboveCaller, "admin": AssignAboveCaller,
				"user": AssignAllowed, "viewer": AssignAllowed,
			},
			"user":         allCells(granted, AssignNotAUserManager),
			"viewer":       allCells(granted, AssignNotAUserManager),
			"no-such-role": allCells(granted, AssignNotAUserManager),
		},
		AssignOnReRole: {
			RoleOwner: {
				"owner": AssignOwnershipTransfer, "developer": AssignAllowed, "admin": AssignAllowed,
				"user": AssignAllowed, "viewer": AssignAllowed,
			},
			// UNCHANGED BY THIS ISSUE, and the reason the split is safe: a
			// developer holds no update-on-principal, so it is refused at the
			// capability half before any rank or authority question is asked.
			// It may open a door; it may not move somebody through one.
			RoleDeveloper: allCells(granted, AssignNotAUserManager),
			RoleAdmin: {
				"owner": AssignOwnershipTransfer, "developer": AssignAboveCaller, "admin": AssignAboveCaller,
				"user": AssignAllowed, "viewer": AssignAllowed,
			},
			"user":         allCells(granted, AssignNotAUserManager),
			"viewer":       allCells(granted, AssignNotAUserManager),
			"no-such-role": allCells(granted, AssignNotAUserManager),
		},
	}

	for kind, label := range map[AssignKind]string{
		AssignOnInvitation: "invitation",
		AssignOnReRole:     "re-role",
	} {
		targetId, targetCurrent := "", ""
		if kind == AssignOnReRole {
			targetId, targetCurrent = "u-target", "viewer"
		}
		for _, inviter := range inviters {
			for _, newRole := range granted {
				name := label + "/" + string(inviter) + "->" + newRole
				t.Run(name, func(t *testing.T) {
					got := MayAssignRole(
						UserContext{ID: "u-caller", Role: inviter},
						kind, targetId, targetCurrent, newRole, nil,
					)
					if w := want[kind][inviter][newRole]; got != w {
						t.Fatalf("%s = %q, want %q", name, got, w)
					}
				})
			}
		}
	}
}

// THE CLAUSE STILL RUNS ON RE-ROLING, and this is the only test that proves it.
//
// The cross product above cannot: in that fixture the one caller lacking a
// principal verb is `developer`, which the capability half refuses on the
// re-role seam before the authority clause is ever reached. So a change that
// deleted the clause outright -- rather than scoping it -- would leave that
// table entirely green.
//
// This installs a caller that DOES hold update-on-principal and still lacks a
// verb the granted role holds, and asserts the same pair answers differently
// per kind. Delete the `kind == AssignOnReRole` guard and the invitation half
// fails; delete the clause and the re-role half fails.
func TestPeopleAuthorityClauseRunsOnReRoleOnly(t *testing.T) {
	installFake(t, &fakeCatalog{
		ranks: map[string]int{"support-lead": 150, "purger": 60, "viewer": 50},
		names: map[string]string{"support-lead": "Support Lead", "purger": "Purger", "viewer": "Viewer"},
		grants: map[string]map[VerbResource]bool{
			// Holds update-on-principal, so it clears the capability half on
			// BOTH seams -- but holds no delete.
			"support-lead": withAdmission(map[VerbResource]bool{
				{Verb: VerbRead, Resource: ResourcePrincipal}:   true,
				{Verb: VerbCreate, Resource: ResourcePrincipal}: true,
				{Verb: VerbUpdate, Resource: ResourcePrincipal}: true,
			}),
			// Ranks below support-lead, so the rank cap passes and the
			// authority clause is the only thing left to refuse it.
			"purger": {{Verb: VerbDelete, Resource: ResourcePrincipal}: true},
			"viewer": {},
		},
	})

	caller := UserContext{ID: "u-sl", Role: "support-lead"}

	if got := MayAssignRole(caller, AssignOnReRole, "u-t", "viewer", "purger", nil); got != AssignAuthorityBeyond {
		t.Errorf("re-roling somebody to a role holding delete-on-principal = %q, want %q",
			got, AssignAuthorityBeyond)
	}
	if got := MayAssignRole(caller, AssignOnInvitation, "", "", "purger", nil); got != AssignAllowed {
		t.Errorf("inviting at the same role = %q, want allowed", got)
	}
}

func allCells(granted []string, r AssignRefusal) map[string]AssignRefusal {
	out := make(map[string]AssignRefusal, len(granted))
	for _, g := range granted {
		out[g] = r
	}
	return out
}
