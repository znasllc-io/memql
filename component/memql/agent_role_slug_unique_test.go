package memql

import "testing"

// agent_role_slug_unique_test.go -- memql#3207, the DB-free half.
//
// These pin the two pure decisions the guard makes: what a stored row id looks
// like (so self-exclusion actually excludes self), and when a slug claim
// conflicts. The wiring and the end-to-end refusal are pinned separately, in
// agent_role_slug_unique_db_test.go, because a validator nothing calls is worse
// than no validator (memql#3061's review measured exactly that).

// The self-exclusion compares the id being written against the id of the row
// that already holds the slug. Those arrive in DIFFERENT forms -- the mutation
// carries a bare id, storage holds a concept-qualified one -- so a naive
// comparison never matches and the FIRST idempotent re-seed of every predefined
// role is refused at boot.
func TestCanonicalAgentRoleStorageId(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"family-doctor", "v1:agents:agentRole:family-doctor"},
		{"v1:agents:agentRole:family-doctor", "v1:agents:agentRole:family-doctor"},
		{"  family-doctor  ", "v1:agents:agentRole:family-doctor"},
		{"", ""},
	} {
		if got := canonicalAgentRoleStorageId(tc.in); got != tc.want {
			t.Errorf("canonicalAgentRoleStorageId(%q) = %q, want %q.\n\n"+
				"If the bare and qualified forms do not converge here, self-exclusion fails and "+
				"the seeder's own idempotent re-write is refused on every boot.", tc.in, got, tc.want)
		}
	}
}

func TestAgentRoleSlugConflict(t *testing.T) {
	holder := []agentRoleSlugRow{{Id: "v1:agents:agentRole:family-doctor", Active: true}}

	for _, tc := range []struct {
		name        string
		candidateId string
		active      bool
		existing    []agentRoleSlugRow
		wantConflct bool
		why         string
	}{
		{
			name: "a second active row at a different id is the memql#3066 mint",
			// The shadow-mint: an explicit agentRoleId alongside a slug a
			// seeded row already owns.
			candidateId: "v1:agents:agentRole:attacker-chosen",
			active:      true, existing: holder, wantConflct: true,
			why: "this is the whole defect -- two active rows carrying one slug, both returned by activeAgentRoles",
		},
		{
			name:        "re-writing the row that already holds the slug is not a conflict",
			candidateId: "v1:agents:agentRole:family-doctor",
			active:      true, existing: holder, wantConflct: false,
			why: "the idempotent re-seed writes the same id every boot; refusing it would fail startup",
		},
		{
			name:        "the bare id form still resolves to self",
			candidateId: "family-doctor",
			active:      true, existing: holder, wantConflct: false,
			why: "the mutation carries a bare id while storage holds the qualified one",
		},
		{
			name:        "an INACTIVE candidate does not contest the slug",
			candidateId: "v1:agents:agentRole:retired",
			active:      false, existing: holder, wantConflct: false,
			why: "every reader filters isActiveRecord, so an inactive row is invisible to the catalog",
		},
		{
			name:        "an inactive INCUMBENT does not hold the slug",
			candidateId: "v1:agents:agentRole:new-owner",
			active:      true,
			existing:    []agentRoleSlugRow{{Id: "v1:agents:agentRole:old", Active: false}},
			wantConflct: false,
			why:         "the uniqueness claim is over ACTIVE rows; a deactivated role must not permanently burn its slug",
		},
		{
			name:        "no incumbent at all",
			candidateId: "v1:agents:agentRole:fresh",
			active:      true, existing: nil, wantConflct: false,
			why: "the ordinary create path",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, got := agentRoleSlugConflict(tc.candidateId, tc.active, tc.existing)
			if got != tc.wantConflct {
				t.Errorf("conflict=%v, want %v -- %s", got, tc.wantConflct, tc.why)
			}
		})
	}
}
