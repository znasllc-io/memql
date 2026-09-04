package memql

import (
	"log/slog"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// TestLoad_LibraryArtifactProbeIsNeverCached pins that the query the upload
// path POLLS carries an explicit never-cache hint.
//
// libraryArtifactBySourceConceptRef is a probe: waitForPromotion on the bff,
// the chunked /complete resolver and the analysis pass all call it every 50ms
// to observe a row that indexFileOnCreate writes on ANOTHER node a few
// milliseconds later. With no @cache hint the result cache holds a query for
// 60s by default and an empty answer is cached like any other -- so the first
// miss was replayed for the whole 3s wait, the handler answered 201 with an
// empty artifactId, and the OS said "Upload landed but named no artifact" and
// invited a retry that uploaded the file a second time. Seen on aks-memql on
// 2026-09-04 with the workbench committing the artifact 2.98s before the bff
// gave up.
func TestLoad_LibraryArtifactProbeIsNeverCached(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(nullWriter{}, &slog.HandlerOptions{Level: slog.LevelError}))

	if _, err := LoadUnifiedConcepts(logger); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	concepts := memoryNodes.DefaultRegistry()
	functionRegistry, err := loadEmbeddedFunctions(logger, concepts)
	if err != nil {
		t.Fatalf("loadEmbeddedFunctions: %v", err)
	}
	if _, _, err := LoadUnifiedFunctions(logger, functionRegistry, concepts); err != nil {
		t.Fatalf("LoadUnifiedFunctions: %v", err)
	}

	fn, err := functionRegistry.Get("libraryArtifactBySourceConceptRef")
	if err != nil {
		t.Fatalf("libraryArtifactBySourceConceptRef: not registered: %v", err)
	}
	hints := map[string]int64{}
	collectCacheHints(fn.Expr, hints)
	if len(hints) == 0 {
		t.Fatal("libraryArtifactBySourceConceptRef carries no @cache hint, so the default TTL applies and a cached empty answer outlives the upload's promotion wait")
	}
	for _, ttl := range hints {
		if ttl != 0 {
			t.Fatalf("libraryArtifactBySourceConceptRef cache hint = %ds, want 0: this query is a probe for a row another node is about to write", ttl)
		}
	}
}
