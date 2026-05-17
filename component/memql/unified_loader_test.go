package memql

import (
	"log/slog"
	"os"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// TestLoadUnifiedConcepts_RegistersFromNewTree verifies the unified
// concept loader runs and registers SOMETHING from the new tree.
//
// Known partial-state: only concepts in files that parse cleanly via
// compiler.ParseFileSource are visible (the rest get the
// ExtractImports fallback in dslimports.Load, which strips all
// Definitions). At the time of writing many consolidated files
// fail the mutation/query struct-form rewriters because those
// rewriters assume a single-concept-per-file binding (see
// docs/dsl-import-model-refactor.md "Pass 2 blockers"). Until the
// rewriters get a multi-concept-aware code path, the unified
// loader is best-effort -- the legacy loader covers the gap during
// the transitional state.
//
// The test verifies the wiring works (>0 concepts registered, no
// fatal errors) without asserting full coverage.
func TestLoadUnifiedConcepts_RegistersFromNewTree(t *testing.T) {
	// Use a discard logger so test output isn't polluted by the
	// per-file warning messages the loader emits for files that
	// can't be parsed via the full rewriter chain.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	count, err := LoadUnifiedConcepts(logger)
	if err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	if count == 0 {
		t.Fatal("loader registered 0 concepts; new tree should contain at least some that parse cleanly")
	}
	t.Logf("registered %d concepts from unified tree (partial -- some files hit the rewriter blocker)", count)

	// Sample at least one concept that we know parses cleanly (no
	// mutations bind to it, so the mutation rewriter doesn't fire).
	// Pick from the registry to find what actually loaded.
	concepts := memoryNodes.List()
	if len(concepts) == 0 {
		t.Fatal("expected at least one concept in the registry after LoadUnifiedConcepts")
	}
}
