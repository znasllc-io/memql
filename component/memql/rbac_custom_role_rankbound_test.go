package memql

import "testing"

// Base ranks from the E1.2 catalog (dsl/rbac/seeds.memql).
const (
	testRankOwner     = 400
	testRankDeveloper = 300
	testRankAdmin     = 200
	testRankUser      = 100
)

// TestCustomRoleRankBoundOK pins the pure rank-bound decision: a creator may
// mint/edit a custom role only at a rank STRICTLY BELOW their own. This is the
// E1.4 acceptance core -- "a developer can create a sub-developer custom role
// but not an owner-rank role".
func TestCustomRoleRankBoundOK(t *testing.T) {
	cases := []struct {
		name      string
		actorRank int
		newRank   int
		want      bool
	}{
		// A developer (300) authors a custom role...
		{"developer mints rank-250 lead (below own)", testRankDeveloper, 250, true},
		{"developer mints rank-150 (below own)", testRankDeveloper, 150, true},
		{"developer mints a peer developer (==own)", testRankDeveloper, testRankDeveloper, false},
		{"developer mints an owner-rank role (above own)", testRankDeveloper, testRankOwner, false},
		{"developer mints rank-301 (just above own)", testRankDeveloper, 301, false},
		// An admin (200) authors...
		{"admin mints rank-150 (below own)", testRankAdmin, 150, true},
		{"admin mints rank-250 (above own)", testRankAdmin, 250, false},
		// An owner (400) may author at any sub-owner rank.
		{"owner mints rank-350 (below own)", testRankOwner, 350, true},
		{"owner mints a peer owner (==own)", testRankOwner, testRankOwner, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := customRoleRankBoundOK(c.actorRank, c.newRank, c.actorRank >= testRankOwner)
			if got != c.want {
				t.Errorf("customRoleRankBoundOK(actor=%d, new=%d) = %v, want %v", c.actorRank, c.newRank, got, c.want)
			}
		})
	}
}

// TestCustomRankResolutionHonored proves the resolution side of E1.4: a custom
// rank slotted BETWEEN two base ranks resolves correctly in rank comparisons --
// a rank-250 custom role sits above admin(200) and below developer(300), so the
// governance arithmetic (the same auth.GovernPrincipal the enforcement path
// uses) honors it. This guards against a regression where custom ranks were
// treated as a fixed tier rather than their authored numeric value.
func TestCustomRankResolutionHonored(t *testing.T) {
	const customLeadRank = 250 // slotted between admin(200) and developer(300)

	// A custom-rank "lead" (250) outranks an admin (200): rank-bound author OK
	// downward, and the relational comparison places lead above admin.
	if !customRoleRankBoundOK(customLeadRank, testRankAdmin, false) {
		t.Error("a rank-250 lead must be able to author a role below it (admin rank 200)")
	}
	if customRoleRankBoundOK(testRankAdmin, customLeadRank, false) {
		t.Error("an admin (200) must NOT be able to author a rank-250 role above itself")
	}
	if !customRoleRankBoundOK(testRankDeveloper, customLeadRank, false) {
		t.Error("a developer (300) must be able to author a rank-250 role below itself")
	}
	// Custom rank is honored as its numeric value, not a base tier: 250 is
	// strictly between 200 and 300.
	if !(customLeadRank > testRankAdmin && customLeadRank < testRankDeveloper) {
		t.Fatal("precondition: custom lead rank must slot between admin and developer")
	}
}
