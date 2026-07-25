package memql

// build_offline_sense_test.go proves BuildOfflineSense turns a workspace of
// .memql files into a live Sense service with no database and no network, and
// that its use of process-global DSL registration state is safe to repeat --
// the invariant the offline LSP depends on when it rebuilds on every edit.

import (
	"testing"
	"testing/fstest"

	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// gadgetOverlay is a minimal, well-formed product overlay: one concept in a
// non-core namespace. Its assembled id is v1:gadgets:gadget (only the major
// version enters the prefix).
var gadgetOverlay = fstest.MapFS{
	"gadgets/concepts.memql": {Data: []byte(`@version("1.0.0")
@namespace("gadgets")
@description("A gadget for offline-sense overlay testing.")
concept gadget {
  label  string  @required  @description("Gadget label.")
}
`)},
}

const gadgetConceptID = "v1:gadgets:gadget"

// TestBuildOfflineSense_EmbeddedTreeBuildsNonEmptyRegistry is the core
// acceptance: over the embedded dsl/ tree with a nil DB, the build produces a
// non-empty registry (concepts, functions, shapes) and a usable service.
func TestBuildOfflineSense_EmbeddedTreeBuildsNonEmptyRegistry(t *testing.T) {
	adapter, err := buildOfflineSenseAdapter(nil, nil)
	if err != nil {
		t.Fatalf("buildOfflineSenseAdapter over embedded tree: %v", err)
	}

	concepts := adapter.ConceptNames()
	functions := adapter.FunctionNames()
	shapes := adapter.ShapeNames()
	if len(concepts) == 0 {
		t.Error("expected a non-empty concept registry, got 0 concepts")
	}
	if len(functions) == 0 {
		t.Error("expected a non-empty function registry, got 0 functions")
	}
	if len(shapes) == 0 {
		t.Error("expected a non-empty shape registry, got 0 shapes")
	}

	// A well-known core concept resolves -- proves the registry is really the
	// embedded tree, not merely non-empty.
	if _, ok := adapter.ConceptGet("v1:identity:user"); !ok {
		t.Errorf("expected core concept v1:identity:user to be present; concept count=%d", len(concepts))
	}

	// The public entry point returns a usable service.
	svc, err := BuildOfflineSense(nil)
	if err != nil {
		t.Fatalf("BuildOfflineSense over embedded tree: %v", err)
	}
	if svc == nil {
		t.Fatal("BuildOfflineSense returned a nil service")
	}
	if toks := svc.Tokenize("concept gadget { label string }"); len(toks) == 0 {
		t.Error("expected Tokenize to return tokens from the built service")
	}
}

// TestBuildOfflineSense_RepeatedCallsSafe proves repeated in-process builds are
// safe and deterministic -- no growth or drift in the built registry across
// calls (the additive global registry does not leak between builds).
func TestBuildOfflineSense_RepeatedCallsSafe(t *testing.T) {
	first, err := buildOfflineSenseAdapter(nil, nil)
	if err != nil {
		t.Fatalf("first build: %v", err)
	}
	second, err := buildOfflineSenseAdapter(nil, nil)
	if err != nil {
		t.Fatalf("second build: %v", err)
	}

	if a, b := len(first.ConceptNames()), len(second.ConceptNames()); a == 0 || a != b {
		t.Errorf("concept count not stable across builds: first=%d second=%d", a, b)
	}
	if a, b := len(first.FunctionNames()), len(second.FunctionNames()); a == 0 || a != b {
		t.Errorf("function count not stable across builds: first=%d second=%d", a, b)
	}
	if a, b := len(first.ShapeNames()), len(second.ShapeNames()); a == 0 || a != b {
		t.Errorf("shape count not stable across builds: first=%d second=%d", a, b)
	}
}

// TestBuildOfflineSense_OverlayVisibleToServiceButRestoredGlobally proves the
// two halves of the global-state contract at once: a workspace overlay concept
// is visible to the built service (Init bound the engine to the merged-registry
// clone), yet the process-global concept registry is restored to its
// embedded-only baseline before the call returns (no leak into a later build).
func TestBuildOfflineSense_OverlayVisibleToServiceButRestoredGlobally(t *testing.T) {
	adapter, err := buildOfflineSenseAdapter(nil, gadgetOverlay)
	if err != nil {
		t.Fatalf("build over gadget overlay: %v", err)
	}

	// Visible to the built service.
	if _, ok := adapter.ConceptGet(gadgetConceptID); !ok {
		t.Errorf("overlay concept %s not visible to the built Sense service", gadgetConceptID)
	}

	// Not leaked into the process-global registry.
	if _, err := concept.Get(gadgetConceptID); err == nil {
		t.Errorf("overlay concept %s leaked into the global registry (should have been restored)", gadgetConceptID)
	}

	// A subsequent embedded-only build must not see the overlay either.
	embedded, err := buildOfflineSenseAdapter(nil, nil)
	if err != nil {
		t.Fatalf("embedded build after overlay build: %v", err)
	}
	if _, ok := embedded.ConceptGet(gadgetConceptID); ok {
		t.Errorf("overlay concept %s leaked into a subsequent embedded-only build", gadgetConceptID)
	}
}

// repoShapedOverlay is a workspace shaped like the REPOSITORY an author opens
// in VS Code: the product domains live under dsl/, and a scratch .memql file
// sits directly in that container directory. Scratch files like it exist in
// real working trees.
var repoShapedOverlay = fstest.MapFS{
	"dsl/test.memql": {Data: []byte("// a scratch file an author left in the tree\n")},
	"dsl/gadgets/concepts.memql": {Data: []byte(`@version("1.0.0")
@namespace("gadgets")
@description("A gadget for offline-sense overlay testing.")
concept gadget {
  label  string  @required  @description("Gadget label.")
}
`)},
	"dsl/widgets/concepts.memql": {Data: []byte(`@version("1.0.0")
@namespace("widgets")
@description("A widget for offline-sense overlay testing.")
concept widget {
  label  string  @required  @description("Widget label.")
}
`)},
}

// TestBuildOfflineSense_StrayFileInContainerDirDoesNotBreakTheBuild is the
// #2770 regression net.
//
// MountOverlayDomains treats any immediate subdirectory holding a .memql file
// as a product domain. From a repository root that is normally nothing --
// dsl/ holds only subdirectories -- but ONE scratch file made `dsl` itself
// qualify, so it mounted as a domain named "dsl" and every concept beneath it
// failed namespace validation. The whole registry build aborted, and the
// editor lost all concept knowledge.
//
// The failure was confusing rather than obvious: go-to-definition kept working
// (the workspace graph is built from files independently), so it read as
// "hover is broken", not "one file is misplaced".
func TestBuildOfflineSense_StrayFileInContainerDirDoesNotBreakTheBuild(t *testing.T) {
	adapter, err := buildOfflineSenseAdapter(nil, repoShapedOverlay)
	if err != nil {
		t.Fatalf("a scratch .memql in the DSL container dir must not break the build: %v", err)
	}
	if adapter == nil {
		t.Fatal("no adapter built")
	}
	// The core embedded registry must still be there -- the symptom was it
	// vanishing entirely.
	if len(adapter.ConceptNames()) == 0 {
		t.Error("expected the embedded concept registry to survive; got 0 concepts")
	}
	// And the product domains below dsl/ must still mount, since resolving the
	// root is what lets them be seen as domains at all.
	for _, want := range []string{"v1:gadgets:gadget", "v1:widgets:widget"} {
		if _, ok := adapter.ConceptGet(want); !ok {
			t.Errorf("overlay concept %s not mounted from the resolved DSL root", want)
		}
	}
}
