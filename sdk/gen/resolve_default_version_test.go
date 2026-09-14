package gen

import (
	"os"
	"path/filepath"
	"testing"
)

// TestBuildConceptIndex_RealTreeDefaultVersion runs the concept index over
// the repository's real dsl/ tree. After #2613 stripped the redundant
// @version("1.0.0") annotations, every concept relies on the absent-version
// default -- so a resolver that still requires @version zeroes the ENTIRE
// index while every fixture-based test stays green. This is the regression
// the adversarial review of PR #2657 caught: dirs=0/concepts=0 on the
// stripped tree with a fully passing suite.
func TestBuildConceptIndex_RealTreeDefaultVersion(t *testing.T) {
	root := filepath.Join("..", "..", "dsl")
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("real dsl tree not found at %s: %v", root, err)
	}

	idx := buildConceptIndex([]string{root})
	if len(idx) == 0 {
		t.Fatal("concept index over the real dsl/ tree is empty -- absent @version must default to major 1 (#2613)")
	}

	cognition, ok := idx["worker"]
	if !ok || len(cognition) == 0 {
		t.Fatalf("worker namespace missing from real-tree index (dirs=%d)", len(idx))
	}
	for name, id := range cognition {
		if len(id) < 4 || id[:3] != "v1:" {
			t.Errorf("concept %q: id %q must carry the v1: default prefix", name, id)
		}
	}
}

// TestAssembleConceptIdFromPreamble_VersionDefault pins the unit contract:
// absent @version means major 1; an explicit non-default @version wins; and
// the namespace is whatever the caller resolved (the directory's
// namespace.pin, else the directory) -- lockstep with
// AssembleConceptIdFromDeclInDir.
//
// The @namespace cases this used to carry are gone with the annotation (epic
// memql#5375). What replaced them is the `ns` argument: buildConceptIndex
// resolves pin-else-directory and passes it, so a pinned divergence still
// reaches the generated SDK -- which is the case the retired annotation
// existed to cover.
func TestAssembleConceptIdFromPreamble_VersionDefault(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{
			name: "absent version defaults to v1",
			src:  "concept widget {\n}\n",
			want: "v1:gadgets:widget",
		},
		{
			name: "explicit non-default version wins",
			src:  "@version(\"2.5.7\")\nconcept widget {\n}\n",
			want: "v2:gadgets:widget",
		},
		{
			name: "an explicit 1.0.0 is the default spelled out",
			src:  "@version(\"1.0.0\")\nconcept widget {\n}\n",
			want: "v1:gadgets:widget",
		},
	}
	for _, tc := range cases {
		m := conceptHeaderRe.FindStringSubmatchIndex(tc.src)
		if m == nil {
			t.Fatalf("%s: fixture has no concept header", tc.name)
		}
		got := assembleConceptIdFromPreamble(tc.src, m[0], "widget", "gadgets")
		if got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}
