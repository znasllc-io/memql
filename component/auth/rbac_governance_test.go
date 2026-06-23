package auth

import "testing"

// Base ranks (rankOwner / rankDeveloper / rankAdmin / rankUser) are the
// package constants defined in rbac_model.go -- the single source of truth
// shared by the governance core and these tests.

func owner(id string) Principal     { return Principal{UserId: id, Rank: rankOwner, IsOwner: true} }
func developer(id string) Principal { return Principal{UserId: id, Rank: rankDeveloper} }
func admin(id string) Principal     { return Principal{UserId: id, Rank: rankAdmin} }
func member(id string) Principal    { return Principal{UserId: id, Rank: rankUser} }

// TestGovernPrincipal_EditDownRank: a higher-ranked actor may update/delete a
// strictly-lower-ranked target; an equal- or higher-ranked target is refused.
func TestGovernPrincipal_EditDownRank(t *testing.T) {
	cases := []struct {
		name        string
		actor       Principal
		target      Principal
		verb        GovernVerb
		wantAllowed bool
	}{
		{"admin updates member", admin("a"), member("m"), GovernUpdate, true},
		{"admin deletes member", admin("a"), member("m"), GovernDelete, true},
		{"developer updates admin (developer outranks admin)", developer("d"), admin("a"), GovernUpdate, true},
		{"admin cannot update developer (admin is lower)", admin("a"), developer("d"), GovernUpdate, false},
		{"admin cannot update peer admin (not strictly higher)", admin("a1"), admin("a2"), GovernUpdate, false},
		{"member cannot update another member", member("m1"), member("m2"), GovernUpdate, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := GovernPrincipal(c.actor, c.target, c.verb); got != c.wantAllowed {
				t.Errorf("GovernPrincipal(%s) = %v, want %v", c.name, got, c.wantAllowed)
			}
		})
	}
}

// TestGovernPrincipal_SelfEdit: any actor may update themselves regardless of
// rank; a non-owner may delete themselves; an OWNER may NOT delete themselves
// (lockout guard).
func TestGovernPrincipal_SelfEdit(t *testing.T) {
	if !GovernPrincipal(member("m"), member("m"), GovernUpdate) {
		t.Error("a member must be able to update themselves")
	}
	if !GovernPrincipal(admin("a"), admin("a"), GovernUpdate) {
		t.Error("an admin must be able to update themselves")
	}
	if !GovernPrincipal(member("m"), member("m"), GovernDelete) {
		t.Error("a non-owner must be able to delete themselves")
	}
	if GovernPrincipal(owner("o"), owner("o"), GovernDelete) {
		t.Error("an owner must NOT be able to delete themselves (lockout guard)")
	}
	// An owner CAN update their own non-rank fields (self branch for update).
	if !GovernPrincipal(owner("o"), owner("o"), GovernUpdate) {
		t.Error("an owner must be able to update themselves")
	}
}

// TestGovernPrincipal_OwnerOnlyByOwner: an owner-principal is editable ONLY by
// an owner -- no non-owner, however high-ranked, may manage an owner.
func TestGovernPrincipal_OwnerOnlyByOwner(t *testing.T) {
	if !GovernPrincipal(owner("o1"), owner("o2"), GovernUpdate) {
		t.Error("an owner must be able to update another owner")
	}
	if GovernPrincipal(developer("d"), owner("o"), GovernUpdate) {
		t.Error("a developer must NOT be able to update an owner")
	}
	if GovernPrincipal(admin("a"), owner("o"), GovernDelete) {
		t.Error("an admin must NOT be able to delete an owner")
	}
	// Even a hypothetical custom rank above admin but non-owner cannot edit an
	// owner.
	superAdmin := Principal{UserId: "s", Rank: 350, IsOwner: false}
	if GovernPrincipal(superAdmin, owner("o"), GovernUpdate) {
		t.Error("a non-owner above admin must still NOT be able to edit an owner")
	}
}

// TestGovernPrincipal_CreateNotEdit: create is governed by CanCreatePrincipal
// (capability + rank-bound), independently of update -- the create != edit
// split. A role can create principals below its rank while being unable to
// edit an existing same-or-higher-ranked one.
func TestGovernPrincipal_CreateNotEdit(t *testing.T) {
	a := admin("a")

	// Admin can CREATE a member (below admin rank).
	if !CanCreatePrincipal(a, rankUser) {
		t.Error("an admin must be able to create a member-rank principal")
	}
	// Admin can CREATE a developer? No -- developer outranks admin, so an
	// admin may not mint a developer (cannot create at/above own rank).
	if CanCreatePrincipal(a, rankDeveloper) {
		t.Error("an admin must NOT be able to create a developer-rank principal (at/above own rank)")
	}
	// Admin cannot create a peer admin (not strictly below).
	if CanCreatePrincipal(a, rankAdmin) {
		t.Error("an admin must NOT be able to create a peer admin-rank principal")
	}
	// Owner can create at any sub-owner rank.
	if !CanCreatePrincipal(owner("o"), rankDeveloper) {
		t.Error("an owner must be able to create a developer-rank principal")
	}

	// The split: a role with create-on-principal at member rank does NOT thereby
	// gain update on an existing equal-or-higher principal. Admin can create a
	// member but cannot edit a developer.
	if !CanCreatePrincipal(a, rankUser) {
		t.Fatal("precondition: admin can create a member")
	}
	if GovernPrincipal(a, developer("d"), GovernUpdate) {
		t.Error("create-on-principal must NOT confer update on a higher-ranked principal (create != edit)")
	}
}

// TestGovernPrincipal_ReadAlwaysPasses: the relational layer never narrows a
// read -- the capability grant + per-row visibility decide.
func TestGovernPrincipal_ReadAlwaysPasses(t *testing.T) {
	if !GovernPrincipal(member("m"), owner("o"), GovernRead) {
		t.Error("the relational layer must not narrow reads")
	}
}
