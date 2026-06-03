package dsl

import (
	"os"
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

// TestUnifiedTreeCoversAllDomains guards against the embed-omission
// class (memql#771: the whole dsl/library/ namespace was authored on
// disk but never added to the go:embed directive, so none of its
// concepts / queries / mutations loaded and every Library read failed
// `function not found`). Rather than a hand-maintained allow-list --
// which is precisely what let library slip through -- it derives the
// expected domains from the on-disk dsl/ tree (the test's CWD is the
// package source dir) and asserts every authored namespace directory
// is reachable through the embedded Tree(). Add a namespace dir on
// disk without updating the embed list and this fails.
func TestUnifiedTreeCoversAllDomains(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir(.): %v", err)
	}
	var want []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		// `_reference` (and any underscore-prefixed dir) holds
		// authoring skeletons that are intentionally NOT embedded.
		if strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") {
			continue
		}
		// A namespace dir is one that actually carries .memql sources.
		hasMemql := false
		files, err := os.ReadDir(name)
		if err != nil {
			continue
		}
		for _, f := range files {
			if strings.HasSuffix(f.Name(), ".memql") {
				hasMemql = true
				break
			}
		}
		if hasMemql {
			want = append(want, name)
		}
	}
	if len(want) == 0 {
		t.Fatal("no on-disk namespace dirs found; test harness misconfigured")
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
			t.Errorf("namespace %q exists on disk but is missing from the embedded Tree() -- add `all:%s` to the go:embed directive in embed.go", d, d)
		}
	}
}
