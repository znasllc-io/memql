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
	if err := memoryNodes.LoadConcepts(nil); err != nil {
		t.Fatalf("LoadConcepts: %v", err)
	}
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
		{"queryLibraryArtifacts", "v1:library:artifact"},
		{"queryLibraryArtifactsByLens", "v1:library:artifact"},
		{"queryLibraryArtifactsByKind", "v1:library:artifact"},
		{"queryLibraryArtifactsBySpace", "v1:library:artifact"},
		{"queryLibraryArtifactById", "v1:library:artifact"},
		{"queryGeneratedOutputById", "v1:library:generatedOutput"},
		{"queryMemoryById", "v1:library:memory"},
		{"queryLibraryWorkspaceLiveSources", "v1:library:artifact"},
		{"queryLatestLiveSnapshotForSource", "v1:knowledge:liveSnapshot"},
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
