package dsl

import "testing"

// The subject of these tests is the disabled-domain SET and the path matcher
// over it, not any particular pack: nothing here loads the tree, so the domain
// names are fixtures.
//
// They used to be "probepack", which was a real pack. The work spine's epic A1
// retired it, and a fixture named after a domain that does not exist reads as
// a claim about a real thing -- so the pair is now an obviously-fictional
// `probepack` and the one real pack left, `referencepack`, which is what
// docs/public/concepts/modules.md points at as the worked example.

func TestDisabledPackSet(t *testing.T) {
	t.Cleanup(func() { SetDisabledPackDomains(nil) })

	if PackDomainDisabled("referencepack") {
		t.Fatalf("fresh set: no domain should be disabled")
	}

	SetDisabledPackDomains([]string{" referencepack ", "", "probepack"})
	if !PackDomainDisabled("referencepack") || !PackDomainDisabled("probepack") {
		t.Fatalf("set membership lost trimmed entries: %v", DisabledPackDomains())
	}
	if got := DisabledPackDomains(); len(got) != 2 || got[0] != "probepack" || got[1] != "referencepack" {
		t.Fatalf("DisabledPackDomains = %v; want sorted [probepack referencepack]", got)
	}

	// Replace semantics, not merge: a later boot read owns the whole set.
	SetDisabledPackDomains([]string{"probepack"})
	if PackDomainDisabled("referencepack") {
		t.Fatalf("SetDisabledPackDomains must replace, not merge")
	}
}

func TestSkipsBehavioralLoad(t *testing.T) {
	t.Cleanup(func() { SetDisabledPackDomains(nil) })
	SetDisabledPackDomains([]string{"probepack"})

	cases := []struct {
		path string
		want bool
	}{
		// Behavioral files of the disabled domain are invisible to loaders.
		{"probepack/mutations.memql", true},
		{"probepack/queries.memql", true},
		{"probepack/automations.memql", true},
		{"probepack/prompts.memql", true},
		{"probepack/logic.memql", true},
		// Mounted-inert: concepts still load so cross-domain imports,
		// relationship targets, and existing rows keep resolving.
		{"probepack/concepts.memql", false},
		// Enabled domains are untouched.
		{"cognition/mutations.memql", false},
		{"platform/concepts.memql", false},
		// Degenerate inputs.
		{"", false},
		{"probepack", true}, // a domain-level path with no basename split
	}
	for _, tc := range cases {
		if got := SkipsBehavioralLoad(tc.path); got != tc.want {
			t.Errorf("SkipsBehavioralLoad(%q) = %v; want %v", tc.path, got, tc.want)
		}
	}
}

func TestSkipsBehavioralLoadDefaultAllEnabled(t *testing.T) {
	SetDisabledPackDomains(nil)
	for _, p := range []string{"probepack/mutations.memql", "referencepack/tools.memql"} {
		if SkipsBehavioralLoad(p) {
			t.Errorf("empty disabled set must skip nothing, skipped %q", p)
		}
	}
}
