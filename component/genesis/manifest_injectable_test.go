package genesis

import "testing"

// TestInjectableDefaults_Selection verifies the first-deploy injection
// candidate selection (#2109): only entries that BOTH carry a non-empty
// default AND have a global/partition scope are candidates. Node-scoped
// entries and default-less entries are skipped, regardless of which
// list (secrets / variables) they live in.
func TestInjectableDefaults_Selection(t *testing.T) {
	m := &Manifest{
		Secrets: []ManifestEntry{
			// global secret WITH default -> candidate
			{Name: "SECRET_WITH_DEFAULT", Scope: "global", Kind: "integration", Default: "s-default"},
			// global secret, NO default -> skipped
			{Name: "SECRET_NO_DEFAULT", Scope: "global", Kind: "vendor_api_key"},
			// partition secret WITH default -> candidate
			{Name: "PARTITION_SECRET_DEFAULT", Scope: "partition", Default: "ps-default"},
			// node-scoped secret WITH default -> skipped (process env, never concept-stored)
			{Name: "NODE_SECRET_DEFAULT", Scope: "node", Default: "n-default"},
		},
		Variables: []ManifestEntry{
			// global variable WITH default -> candidate
			{Name: "VAR_WITH_DEFAULT", Scope: "global", Default: "v-default"},
			// node-scoped variable WITH default -> skipped
			{Name: "NODE_VAR_DEFAULT", Scope: "node", Default: "nv-default"},
			// global variable, NO default -> skipped
			{Name: "VAR_NO_DEFAULT", Scope: "global"},
			// partition variable WITH default -> candidate
			{Name: "PARTITION_VAR_DEFAULT", Scope: "partition", Default: "pv-default"},
			// unknown scope WITH default -> skipped (conservative)
			{Name: "WEIRD_SCOPE", Scope: "tenant", Default: "x"},
		},
	}

	got := m.InjectableDefaults()

	// Expected, in manifest order: secrets first, then variables.
	type want struct {
		name     string
		isSecret bool
	}
	expected := []want{
		{"SECRET_WITH_DEFAULT", true},
		{"PARTITION_SECRET_DEFAULT", true},
		{"VAR_WITH_DEFAULT", false},
		{"PARTITION_VAR_DEFAULT", false},
	}

	if len(got) != len(expected) {
		t.Fatalf("candidate count: got %d, want %d (%+v)", len(got), len(expected), got)
	}
	for i, w := range expected {
		if got[i].Entry.Name != w.name {
			t.Errorf("candidate[%d] name: got %q, want %q", i, got[i].Entry.Name, w.name)
		}
		if got[i].IsSecret != w.isSecret {
			t.Errorf("candidate[%d] %q IsSecret: got %v, want %v", i, got[i].Entry.Name, got[i].IsSecret, w.isSecret)
		}
	}
}

// TestInjectableDefaults_EmptyManifest confirms an empty manifest yields
// no candidates (and does not panic).
func TestInjectableDefaults_EmptyManifest(t *testing.T) {
	m := &Manifest{}
	if got := m.InjectableDefaults(); len(got) != 0 {
		t.Fatalf("empty manifest: got %d candidates, want 0", len(got))
	}
}

// TestIsInjectableEntry covers the per-entry predicate directly.
func TestIsInjectableEntry(t *testing.T) {
	cases := []struct {
		name  string
		entry ManifestEntry
		want  bool
	}{
		{"global+default", ManifestEntry{Scope: "global", Default: "x"}, true},
		{"partition+default", ManifestEntry{Scope: "partition", Default: "x"}, true},
		{"global+nodefault", ManifestEntry{Scope: "global"}, false},
		{"node+default", ManifestEntry{Scope: "node", Default: "x"}, false},
		{"unknownscope+default", ManifestEntry{Scope: "", Default: "x"}, false},
	}
	for _, c := range cases {
		if got := isInjectableEntry(c.entry); got != c.want {
			t.Errorf("%s: isInjectableEntry = %v, want %v", c.name, got, c.want)
		}
	}
}
