package dslfs

import "testing"

// domain_test.go -- memql#2852.
//
// Two definitions of "same domain" were in force at once: boot's resolver walked
// from the LAST directory segment backwards, while dslimports'
// sameDomainConceptDecl took the FIRST path segment. Both now call
// DomainFromFilePath, so these cases pin the ONE answer -- and the nested cases
// are where the two used to disagree.

func TestDomainFromFilePath(t *testing.T) {
	for _, tc := range []struct{ name, path, want string }{
		// Flat: both rules always agreed here, which is why the divergence
		// stayed latent.
		{"flat domain file", "planner/queries.memql", "planner"},
		{"flat concepts file", "identity/concepts.memql", "identity"},

		// NESTED -- the divergence. The tree carries 23 of these
		// (dsl/agents/tools/*.memql and similar), so the shape is live.
		// First-segment would say "agents"; boot says "tools".
		{"nested tool file", "agents/tools/askSpecialist.memql", "tools"},
		{"nested, three deep", "alpha/beta/gamma/queries.memql", "gamma"},

		// Legacy v<digits> layout segments are skipped, so a versioned path
		// still yields a real domain rather than "v1".
		{"legacy version between", "mutations/v1/cognition/joinSpace.memql", "cognition"},
		{"legacy version last", "cognition/v1/queries.memql", "cognition"},
		{"only a version dir", "v1/queries.memql", ""},

		// Loader origin decorations. The unified loader stamps slice origins as
		// "unified:<path>:<sliceName>"; without stripping, the prefix pollutes
		// the leading segment -- which mattered when the rule read the FIRST
		// segment and is kept because origins still reach this function.
		{"unified origin", "unified:planner/queries.memql:someQuery", "planner"},
		{"unified origin nested", "unified:agents/tools/askSpecialist.memql:t", "tools"},
		{"dryrun origin", "dryrun:identity/queries.memql:q", "identity"},

		// Degenerate inputs: no directory means no domain.
		{"bare filename", "queries.memql", ""},
		{"empty", "", ""},
		{"leading slash", "/queries.memql", ""},
		{"dot segment skipped", "planner/./queries.memql", "planner"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := DomainFromFilePath(tc.path); got != tc.want {
				t.Errorf("DomainFromFilePath(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

// TestRootDomainFromFilePath is the FIRST-segment sibling's table, and it
// exists because the function shipped without one (memql#3026 landing review):
// every branch but the origin-strip could be deleted with the whole repo's
// suite still green, so nothing pinned the behaviour the ambient rule now
// depends on.
//
// The cases deliberately mirror TestDomainFromFilePath's above, so the two
// answers can be read side by side -- the pairs where they DIFFER are the
// point of having both.
func TestRootDomainFromFilePath(t *testing.T) {
	for _, tc := range []struct{ name, path, want string }{
		// Flat: both answers agree, which is why the divergence stayed latent.
		{"flat domain file", "planner/queries.memql", "planner"},
		{"flat concepts file", "identity/concepts.memql", "identity"},

		// NESTED -- where the two answers differ, and the whole reason this
		// function exists. Boot assembles under the FIRST segment, so these
		// concepts are v1:agents:* and it is `agents` that would own a pin.
		{"nested skills file", "agents/skills/categories.memql", "agents"},
		{"nested roles file", "agents/roles/legal.memql", "agents"},
		{"nested under a pinned domain", "deployment/anything/mutations.memql", "deployment"},

		// Loader origin decorations: the unified loader stamps "unified:<path>",
		// and the prefix must not become the domain.
		{"unified origin", "unified:deployment/mutations.memql", "deployment"},
		{"unified origin, nested", "unified:agents/skills/categories.memql", "agents"},
		{"unified slice origin", "unified:deployment/mutations.memql:createDeployment", "deployment"},

		// Legacy v<digits> layout segments are not domains, exactly as in
		// DomainFromFilePath's table.
		{"legacy version first", "v1/cognition/queries.memql", "cognition"},
		{"legacy version between", "mutations/v1/cognition/joinSpace.memql", "mutations"},

		// Dot and empty segments are skipped, not returned.
		{"leading dot", "./planner/queries.memql", "planner"},
		{"leading slash", "/planner/queries.memql", "planner"},
		{"double slash", "planner//queries.memql", "planner"},

		// No directory component: there is no domain to name.
		{"bare filename", "queries.memql", ""},
		{"empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RootDomainFromFilePath(tc.path); got != tc.want {
				t.Errorf("RootDomainFromFilePath(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

// TestDomainAndRootDomainDifferWhereExpected states the relationship between
// the two answers as an assertion rather than as prose. A change that made
// them agree everywhere would mean one had silently become the other, which is
// the #2852 drift this file exists to prevent.
func TestDomainAndRootDomainDifferWhereExpected(t *testing.T) {
	for _, tc := range []struct{ path, last, first string }{
		{"agents/skills/categories.memql", "skills", "agents"},
		{"deployment/anything/mutations.memql", "anything", "deployment"},
		// Flat paths are the case where they must agree.
		{"planner/queries.memql", "planner", "planner"},
	} {
		if got := DomainFromFilePath(tc.path); got != tc.last {
			t.Errorf("DomainFromFilePath(%q) = %q, want %q", tc.path, got, tc.last)
		}
		if got := RootDomainFromFilePath(tc.path); got != tc.first {
			t.Errorf("RootDomainFromFilePath(%q) = %q, want %q", tc.path, got, tc.first)
		}
	}
}

func TestVersionFromFilePath(t *testing.T) {
	for _, tc := range []struct{ path, want string }{
		{"v1/mutationJoinSpace.memql", "v1"},
		{"mutations/v1/mutationJoinSpace.memql", "v1"},
		{"automations/v1/cognition/autoJoinAI/automation.memql", "v1"},
		{"v12/queries.memql", "v12"},
		{"planner/queries.memql", ""},
		// Must NOT match: `v` followed by a non-digit, or digits interrupted.
		{"voice/queries.memql", ""},
		{"v1x/queries.memql", ""},
		{"", ""},
	} {
		if got := VersionFromFilePath(tc.path); got != tc.want {
			t.Errorf("VersionFromFilePath(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}
