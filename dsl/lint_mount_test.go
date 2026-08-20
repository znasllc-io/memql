package dsl

import (
	"os"
	"testing"
)

// TestMountOverlayDomainsOverTheRepositoryTree is the memql#4190 regression:
// memqllint points MountOverlayDomains at the repo's own dsl/ directory, and
// that directory now contains a domain (harness) an IN-TREE PACK has already
// registered from this package's own init(). Before the in-tree-pack skip,
// the disk walk hit RegisterTree's duplicate-domain panic -- in CI's dsl-lint
// step, and only there, because no in-process test ran the mount over the
// real tree with the pack init loaded. This test is exactly that process
// shape: package dsl's tests run with harness_pack.go's init() done, and the
// package directory IS the DSL root.
func TestMountOverlayDomainsOverTheRepositoryTree(t *testing.T) {
	if !pluginTreeRegistered(HarnessPackDomain) {
		t.Fatalf("precondition: the harness pack init() must have registered %q for this regression to mean anything", HarnessPackDomain)
	}

	var mounted, skippedCore []string
	var unmount func()
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("MountOverlayDomains over the repository dsl/ tree panicked: %v", r)
			}
		}()
		mounted, skippedCore, unmount = MountOverlayDomains(nil, os.DirFS("."))
	}()
	defer unmount()

	// Every other top-level domain is core (skipped through skippedCore);
	// harness is the in-tree pack (skipped through the registration check).
	// Nothing in the engine's own tree is mountable, so mounted is empty --
	// and critically, unmount therefore cannot unregister the pack's own
	// init-time registration.
	if len(mounted) != 0 {
		t.Errorf("mounting the engine's own dsl/ tree mounted %v; want nothing (core + in-tree packs both skip)", mounted)
	}
	for _, d := range skippedCore {
		if d == HarnessPackDomain {
			t.Errorf("harness reported through skippedCore; it is not core (memql#4190) -- its registered tree IS the validated content, and the skippedCore channel would tell callers its content was never looked at")
		}
	}

	unmount()
	if !pluginTreeRegistered(HarnessPackDomain) {
		t.Fatalf("unmount unregistered the in-tree harness pack; it must only remove what the mount itself registered")
	}
}
