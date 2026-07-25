package dsl

import (
	"io/fs"
	"testing"
	"testing/fstest"
)

// TestMountOverlayDomains_SkipRules asserts MountOverlayDomains mounts exactly
// the product-namespace directories (a top-level dir that directly holds a
// .memql file) and skips core-domain collisions, hidden/soft-disabled dirs,
// sidecar dirs with no .memql, and top-level files.
func TestMountOverlayDomains_SkipRules(t *testing.T) {
	root := fstest.MapFS{
		"mypack/concepts.memql":    {Data: []byte("// concepts")},
		"cognition/concepts.memql": {Data: []byte("// collides with a core domain")},
		"_disabled/concepts.memql": {Data: []byte("// soft-disabled")},
		// A CORE-named directory holding no .memql. It was never a product
		// domain, so it must NOT be reported as a collision skip -- this is
		// what pins the .memql-before-collision check order (memql#2782).
		// Without this entry the fixture passes under either order.
		"identity/prompts/x.tmpl":    {Data: []byte("core-named, but no .memql at top")},
		"sidecaronly/prompts/x.tmpl": {Data: []byte("template, no .memql at top")},
		"README.md":                  {Data: []byte("top-level file")},
	}

	mounted, skippedCore, unmount := MountOverlayDomains(nil, root)
	defer unmount()

	got := map[string]bool{}
	for _, m := range mounted {
		got[m] = true
	}
	if !got["mypack"] {
		t.Errorf("expected mypack to be mounted; mounted=%v", mounted)
	}
	for _, skip := range []string{"cognition", "_disabled", "identity", "sidecaronly", "README.md"} {
		if got[skip] {
			t.Errorf("expected %q to be skipped; mounted=%v", skip, mounted)
		}
	}

	// memql#2782: the core-collision skip is REPORTED, so a caller can tell a
	// namespace that was validated from one that was silently left out.
	//
	// Exactly one entry. "cognition" collides AND holds a .memql, so it would
	// have mounted but for the collision -- that is the reportable case. The
	// other four are skipped for reasons visible in the root's own shape and
	// would only dilute the signal; "identity" in particular collides too, but
	// carries no .memql, so it was never a product domain and reporting it
	// would be a false alarm.
	if len(skippedCore) != 1 || skippedCore[0] != "cognition" {
		t.Errorf("skippedCore = %v, want exactly [cognition]", skippedCore)
	}

	// The mounted domain is visible through the unified Tree().
	if _, err := fs.Stat(Tree(), "mypack/concepts.memql"); err != nil {
		t.Errorf("mounted domain should be visible in Tree(): %v", err)
	}

	// A core domain is never shadowed by an overlay: cognition still resolves
	// to the embedded tree, not the fixture's stub.
	if _, err := fs.Stat(Tree(), "cognition/concepts.memql"); err != nil {
		t.Errorf("core cognition domain should still resolve from embedded: %v", err)
	}

	unmount()
	if _, err := fs.Stat(Tree(), "mypack/concepts.memql"); err == nil {
		t.Error("unmount should remove the overlay from Tree()")
	}
}
