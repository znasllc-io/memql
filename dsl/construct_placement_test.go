package dsl

import "testing"

// TestLoaderReadsConstruct pins the placement rule both the automation loader
// and the construct-misplaced gate read (memql#5437): an automation is read
// from its domain's automations.memql, at any namespace depth, and from
// nowhere else; every other kind is read from any file.
func TestLoaderReadsConstruct(t *testing.T) {
	for _, tc := range []struct {
		keyword, path string
		want          bool
	}{
		{"automation", "fleet/automations.memql", true},
		{"automation", "beta/sub/automations.memql", true},
		// The ten fleet automations sat in these two files and never loaded.
		{"automation", "fleet/billing.memql", false},
		{"automation", "fleet/trial.memql", false},
		// No domain claims a file at the root of a tree.
		{"automation", "automations.memql", false},
		// The walker skips soft-disabled and hidden directories.
		{"automation", "fleet/_draft/automations.memql", false},
		{"automation", "fleet/.wip/automations.memql", false},
		{"automation", "fleet/automations.memql.bak", false},
		{"automation", "fleet/my-automations.memql", false},
		// Every other kind is read from any .memql file of its domain.
		{"query", "fleet/billing.memql", true},
		{"logic", "fleet/trial.memql", true},
		{"concept", "research/brief.memql", true},
	} {
		if got := LoaderReadsConstruct(tc.keyword, tc.path); got != tc.want {
			t.Errorf("LoaderReadsConstruct(%q, %q) = %v, want %v", tc.keyword, tc.path, got, tc.want)
		}
	}
}

// TestConstructHome pins where a misplaced construct belongs, which the
// refusal names as its fix.
func TestConstructHome(t *testing.T) {
	for _, tc := range []struct {
		keyword, path, want string
	}{
		{"automation", "fleet/billing.memql", "fleet/automations.memql"},
		{"automation", "beta/sub/brief.memql", "beta/sub/automations.memql"},
		{"automation", "fleet/.wip/automations.memql", "fleet/automations.memql"},
		{"automation", "fleet/_draft/sweeps.memql", "fleet/automations.memql"},
		{"automation", "billing.memql", ""},
		{"query", "fleet/billing.memql", ""},
	} {
		if got := ConstructHome(tc.keyword, tc.path); got != tc.want {
			t.Errorf("ConstructHome(%q, %q) = %q, want %q", tc.keyword, tc.path, got, tc.want)
		}
	}
	if file, ok := ConstructFile("automation"); !ok || file != AutomationsFile {
		t.Errorf("ConstructFile(automation) = %q, %v; want %q, true", file, ok, AutomationsFile)
	}
	if _, ok := ConstructFile("query"); ok {
		t.Error("ConstructFile(query) reports a file of its own; a query is read from every file")
	}
}
