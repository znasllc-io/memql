package automations

import (
	"log/slog"
	"os"
	"testing"
)

// loader_unified_only_test.go -- memql#2858.
//
// LoadAll used to run two extra fs.WalkDir passes over an `l.fsys` field after
// the unified-tree load -- one for `automation.memql`, one for `.json`
// automations -- under a doc comment saying they were "retained for tests that
// inject a custom FS via the fsys field".
//
// No such test existed and none could: NewLoader unconditionally set
// `fsys: fstest.MapFS{}`, LoaderOptions exposes only Logger and Registry, and
// the field was unexported. Both passes always walked an empty FS.
//
// That made the code worse than merely unused. It was MISLEADING: a reader
// reasonably concluded automations could be injected from a custom FS, it kept
// a `.json` automation format alive in the mental model with no live source,
// and it preserved a copy of the concept-visibility swallow that #2856 deleted
// from the unified loader as a hazard.
//
// This test guards the shape that was actually removed: a pass that ADDS to or
// DROPS FROM the set LoadFromUnifiedTree returned. It would have failed on the
// old code, because the second pass appended to the slice the first returned.
//
// STATED LIMIT, because "keeps LoadAll a thin wrapper" would overclaim. The
// assertion is over the NAME SET, so a reintroduced pass that MUTATES existing
// automations -- appending a step to each, say -- is invisible to it. Measured:
// injecting an extra Step into every tree-loaded automation leaves this test
// and the whole package suite green.
//
// That gap is narrower than it looks, and deliberately not closed here:
//
//   - the deleted code APPENDED, so this covers the actual regression;
//   - a cruder in-place hijack is caught by neighbours --
//     replacing steps and clearing Trusted reds
//     TestTreeLoadedAutomationReachesDispatchTrusted,
//     TestLoadByName_CanonicalResolution and
//     TestLoadByName_LiveAndDryRunResolveSameConstruct.
//
// It is the additive step-injection variant specifically that slips through
// everything. Closing it wants a fingerprint over the loaded step graph, which
// is a different test than this one.

// TestLoadAllIsExactlyTheUnifiedTree pins that LoadAll adds nothing to
// LoadFromUnifiedTree.
//
// Asserting on the NAME SET rather than the count: a count check passes if a
// future pass both drops one automation and adds another, which is precisely
// the silent-substitution case worth catching.
func TestLoadAllIsExactlyTheUnifiedTree(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	loader := NewLoader(LoaderOptions{Logger: logger})

	unified, err := loader.LoadFromUnifiedTree()
	if err != nil {
		t.Fatalf("LoadFromUnifiedTree: %v", err)
	}
	all, err := loader.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}

	if len(unified) == 0 {
		t.Fatal("the unified tree yielded zero automations -- this test would then pass " +
			"vacuously, and the parity it asserts would mean nothing")
	}

	nameSet := func(as []*Automation) map[string]int {
		out := map[string]int{}
		for _, a := range as {
			if a != nil {
				out[a.Name]++
			}
		}
		return out
	}

	want, got := nameSet(unified), nameSet(all)
	for name, n := range got {
		if want[name] != n {
			t.Errorf("LoadAll returned %q %d time(s), the unified tree %d -- LoadAll must add "+
				"nothing of its own. A second load pass was removed in memql#2858; if one is "+
				"being reintroduced it needs an injection seam on LoaderOptions and a test that "+
				"uses it, not a walk over an empty FS.", name, n, want[name])
		}
	}
	for name, n := range want {
		if got[name] != n {
			t.Errorf("LoadAll DROPPED %q (unified has it %d time(s), LoadAll %d)", name, n, got[name])
		}
	}
}
