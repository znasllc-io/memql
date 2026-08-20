package dsl

import "testing"

func TestDisabledPackSet(t *testing.T) {
	t.Cleanup(func() { SetDisabledPackDomains(nil) })

	if PackDomainDisabled("referencepack") {
		t.Fatalf("fresh set: no domain should be disabled")
	}

	SetDisabledPackDomains([]string{" referencepack ", "", "harness"})
	if !PackDomainDisabled("referencepack") || !PackDomainDisabled("harness") {
		t.Fatalf("set membership lost trimmed entries: %v", DisabledPackDomains())
	}
	if got := DisabledPackDomains(); len(got) != 2 || got[0] != "harness" || got[1] != "referencepack" {
		t.Fatalf("DisabledPackDomains = %v; want sorted [harness referencepack]", got)
	}

	// Replace semantics, not merge: a later boot read owns the whole set.
	SetDisabledPackDomains([]string{"harness"})
	if PackDomainDisabled("referencepack") {
		t.Fatalf("SetDisabledPackDomains must replace, not merge")
	}
}

func TestSkipsBehavioralLoad(t *testing.T) {
	t.Cleanup(func() { SetDisabledPackDomains(nil) })
	SetDisabledPackDomains([]string{"harness"})

	cases := []struct {
		path string
		want bool
	}{
		// Behavioral files of the disabled domain are invisible to loaders.
		{"harness/mutations.memql", true},
		{"harness/queries.memql", true},
		{"harness/automations.memql", true},
		{"harness/prompts.memql", true},
		{"harness/logic.memql", true},
		// Mounted-inert: concepts still load so cross-domain imports,
		// relationship targets, and existing rows keep resolving.
		{"harness/concepts.memql", false},
		// Enabled domains are untouched.
		{"cognition/mutations.memql", false},
		{"platform/concepts.memql", false},
		// Degenerate inputs.
		{"", false},
		{"harness", true}, // a domain-level path with no basename split
	}
	for _, tc := range cases {
		if got := SkipsBehavioralLoad(tc.path); got != tc.want {
			t.Errorf("SkipsBehavioralLoad(%q) = %v; want %v", tc.path, got, tc.want)
		}
	}
}

func TestSkipsBehavioralLoadDefaultAllEnabled(t *testing.T) {
	SetDisabledPackDomains(nil)
	for _, p := range []string{"harness/mutations.memql", "referencepack/tools.memql"} {
		if SkipsBehavioralLoad(p) {
			t.Errorf("empty disabled set must skip nothing, skipped %q", p)
		}
	}
}
