package dsl

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql/dslfs"
	"github.com/znasllc-io/memql/component/memql/dslimports"
)

// TestUnifiedTreeLoadsClean verifies the unified domain-first tree
// loads via dslimports.Load without any errors. This is the gate
// check before the engine cutover can swap loaders to read from it.
func TestUnifiedTreeLoadsClean(t *testing.T) {
	tree, err := dslimports.Load(Tree())
	if err != nil {
		t.Fatalf("dslimports.Load: %v", err)
	}
	if len(tree.Files) == 0 {
		t.Fatal("Tree() yielded no .memql files; embed directive may be wrong")
	}
	t.Logf("loaded %d files from unified tree", len(tree.Files))
}

// TestUnifiedTreeCoversAllDomains spot-checks that every expected
// top-level domain folder is present in the embed.
func TestUnifiedTreeCoversAllDomains(t *testing.T) {
	want := []string{
		"agents", "cluster", "cognition", "common", "curriculum",
		"data", "guide", "harness", "identity", "knowledge", "memql",
		"notes", "planner", "platform", "policies", "providers", "router",
		"todos", "worker",
	}
	paths, err := dslfs.WalkMemqlFiles(Tree())
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}
	got := make(map[string]bool)
	for _, p := range paths {
		idx := strings.IndexByte(p, '/')
		if idx > 0 {
			got[p[:idx]] = true
		}
	}
	for _, d := range want {
		if !got[d] {
			t.Errorf("domain %q missing from unified tree", d)
		}
	}
}
