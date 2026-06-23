package memql

import (
	"log/slog"
	"os"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// TestLibraryQueriesRegister guards memql#771: every @enabled query in
// dsl/library/queries.memql must register in the function registry and
// carry its signature-bound concept. A regression silently drops the
// whole file's slices at load time (logged at Warn, slice skipped) so
// the Library panel fails every read with `function "..." not found`.
func TestLibraryQueriesRegister(t *testing.T) {
	// DEBUG logger to stderr so a failing run surfaces the per-slice
	// "skipping slice that failed to parse" Warn with the real error.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	if _, err := LoadUnifiedConcepts(logger); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}

	registry := newFunctionRegistry()
	if _, _, err := LoadUnifiedFunctions(logger, registry, memoryNodes.DefaultRegistry()); err != nil {
		t.Fatalf("LoadUnifiedFunctions: %v", err)
	}

	cases := []struct {
		name    string
		concept string
	}{
		{"libraryArtifacts", "v1:library:artifact"},
		{"libraryArtifactsByLens", "v1:library:artifact"},
		{"libraryArtifactsByKind", "v1:library:artifact"},
		// queryLibraryArtifactsBySpace moved to the CoPresent pack
		// (dsl/copresent) in Epic 3 3.6 (memql#1903) -- the per-space facet
		// is a product surface; it's no longer registered in engine-only core.
		{"libraryArtifactById", "v1:library:artifact"},
		{"generatedOutputById", "v1:library:generatedOutput"},
		{"memoryById", "v1:library:memory"},
		{"libraryWorkspaceLiveSources", "v1:library:artifact"},
	}
	for _, tc := range cases {
		fn, err := registry.Get(tc.name)
		if err != nil {
			t.Errorf("%s: not registered: %v", tc.name, err)
			continue
		}
		if fn.BoundConcept != tc.concept {
			t.Errorf("%s: BoundConcept=%q, want %q", tc.name, fn.BoundConcept, tc.concept)
		}
	}
}
