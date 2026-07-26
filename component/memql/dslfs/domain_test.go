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
