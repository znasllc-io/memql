package dslconformance

// The gates in this package walk the unified tree via dslfs.WalkMemqlFiles
// and deliberately carry NO per-test _reference/ exclusions (memql#2651):
// the exemption is structural, not policy. WalkMemqlFiles skips every
// underscore-prefixed directory and file (pinned in the walker's own tests)
// and the go:embed directive omits _reference/ entirely. This test pins
// the COMPOSED end condition against the real tree: the walk never yields
// an underscore path. (Underscore paths INSIDE the tree are normal
// soft-disabled content -- common/_partials is embedded today -- so only
// the walk OUTPUT matters here; the per-half single-fault pins live in
// component/memql/dslfs/walker_test.go.) If this fails, every gate in this
// package has silently started scanning paths it was written to never see.

import (
	"github.com/znasllc-io/memql/dsl"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/dslfs"
)

func TestWalkedTreeHasNoUnderscorePaths(t *testing.T) {
	paths, err := dslfs.WalkMemqlFiles(dsl.Tree())
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
