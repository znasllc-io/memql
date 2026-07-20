package dsl

// The gates in this package walk the unified tree via dslfs.WalkMemqlFiles
// and deliberately carry NO per-test _reference/ exclusions (memql#2651):
// the exemption is structural, not policy. WalkMemqlFiles skips every
// underscore-prefixed directory and file (pinned in the walker's own tests)
// and the go:embed directive omits _reference/ entirely. This test states
// that fact once, against the REAL tree: if either half ever changes -- a
// walker regression, or an underscore path reaching the embedded/registered
// tree -- every gate in this package would silently start scanning
// documentation skeletons, so fail loudly here instead.

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql/dslfs"
)

func TestWalkedTreeHasNoUnderscorePaths(t *testing.T) {
	paths, err := dslfs.WalkMemqlFiles(Tree())
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("walked tree is empty; every gate in this package would pass vacuously")
	}
	for _, p := range paths {
		for _, seg := range strings.Split(p, "/") {
			if strings.HasPrefix(seg, "_") {
				t.Errorf("walk yielded underscore path %q; the per-gate _reference/ exclusions were removed on the strength of this invariant", p)
			}
		}
	}
}
