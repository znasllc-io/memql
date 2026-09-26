package offline_test

import (
	"io"
	"log/slog"
	"testing"

	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	memql "github.com/znasllc-io/memql/component/memql"
)

// Language-line rejection tests need independent boots with their own overlay
// and environment. They cannot borrow the shared read-only engine.

// newQuietEngine builds an engine with an error-only discard logger --
// with no DB + no seeded secrets the provider loader WARNs once per
// provider (auth falls back to OS env), which is expected here and not
// what these tests assert.
func newQuietEngine(t *testing.T) *memql.MemQLEngine {
	t.Helper()
	eng, err := memql.New(nil)
	if err != nil {
		t.Fatalf("construct engine: %v", err)
	}
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	return eng
}

// loadedConceptRegistry mirrors app/database.go: load the unified
// domain-first concept tree into the global registry so engine.Init has a
// non-empty concept set (the SAME sequence TestEngineInitLoadsFullDSL
// uses).
func loadedConceptRegistry(t *testing.T) concept.Registry {
	t.Helper()
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	registry := concept.DefaultRegistry()
	if registry == nil || len(registry.List()) == 0 {
		t.Fatal("concept registry empty after LoadUnifiedConcepts")
	}
	return registry
}
