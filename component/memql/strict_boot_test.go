package memql

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"testing/fstest"

	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// newQuietEngine builds an engine with an error-only discard logger --
// with no DB + no seeded secrets the provider loader WARNs once per
// provider (auth falls back to OS env), which is expected here and not
// what these tests assert.
func newQuietEngine(t *testing.T) *MemQLEngine {
	t.Helper()
	eng, err := New(nil)
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
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	registry := concept.DefaultRegistry()
	if registry == nil || len(registry.List()) == 0 {
		t.Fatal("concept registry empty after LoadUnifiedConcepts")
	}
	return registry
}

// TestStrictBoot_EmbeddedTreeIsClean is the healthy-path guard (item 5):
// engine.Init over the embedded core tree loads clean under strict boot
// -- zero skipped constructs, zero duplicates -- so the strict gate never
// false-fails a healthy fleet.
func TestStrictBoot_EmbeddedTreeIsClean(t *testing.T) {
	registry := loadedConceptRegistry(t)
	eng := newQuietEngine(t)
	if err := eng.Init(registry); err != nil {
		t.Fatalf("engine.Init over the clean embedded tree failed: %v", err)
	}
	if eng.loadReport == nil {
		t.Fatal("engine.loadReport should be set after Init")
	}
	if eng.loadReport.HasProblems() {
		t.Fatalf("embedded tree should load clean, got problems:\n%s", eng.loadReport.Detail())
	}
	if eng.loadReport.Registered["functions"] == 0 {
		t.Fatal("expected a non-zero registered function count in the load report")
	}
}

// TestStrictBoot_FixtureWithBadConstruct is the core acceptance test
// (item 5): a tree with an unparseable construct REFUSES to boot without
// the escape hatch, and boots (with the skip reported) WITH it. The bad
// construct is injected as a throwaway RegisterTree pack -- the same path
// a real product pack loads through -- so this exercises the merged
// embedded-core + registered-pack tree the strict gate covers.
func TestStrictBoot_FixtureWithBadConstruct(t *testing.T) {
	const domain = "s2fixturebadboot"
	// A spec with a garbage body (the exact `====` / `&&&&` shape from the
	// epic #2351 audit). Balanced braces so slice extraction finds it, but
	// ParseSpecDecl rejects the body -> LoadUnifiedSpecs skips it and
	// records the drop on the report. No concept / query references it, so
	// it trips ONLY the strict-boot gate, not the dependency-tree or CQS
	// validators that run earlier.
	fixture := fstest.MapFS{
		"specs.memql": {Data: []byte("@enabled\n@description(\"bad\")\nspec activeRowTrait fixtureBadSpec {\n  return status ==== \"x\" &&&& true\n}\n")},
	}
	memqldsl.RegisterTree(domain, fixture)
	t.Cleanup(func() { memqldsl.UnregisterTree(domain) })

	// engine.Init normalizes (mutates) the concept registry's relationships
	// in place, so each Init needs a freshly-loaded concept set --
	// LoadUnifiedConcepts re-loads clean definitions into the global
	// registry per call.

	t.Run("refuses to boot without escape hatch", func(t *testing.T) {
		t.Setenv(AllowSkipsEnvVar, "") // force strict regardless of ambient env
		registry := loadedConceptRegistry(t)
		eng := newQuietEngine(t)
		err := eng.Init(registry)
		if err == nil {
			t.Fatal("engine.Init must FAIL when the tree has a skipped construct and the escape hatch is unset")
		}
		if !strings.Contains(err.Error(), "strict DSL boot refused") {
			t.Fatalf("expected a strict-boot error, got: %v", err)
		}
		if !strings.Contains(err.Error(), "fixtureBadSpec") {
			t.Fatalf("strict-boot error should name the skipped construct, got: %v", err)
		}
		if !strings.Contains(err.Error(), AllowSkipsEnvVar) {
			t.Fatalf("strict-boot error should point at the %s break-glass, got: %v", AllowSkipsEnvVar, err)
		}
	})

	t.Run("boots with escape hatch, skip reported", func(t *testing.T) {
		t.Setenv(AllowSkipsEnvVar, "1")
		registry := loadedConceptRegistry(t)
		eng := newQuietEngine(t)
		if err := eng.Init(registry); err != nil {
			t.Fatalf("engine.Init must SUCCEED with %s=1 (break-glass): %v", AllowSkipsEnvVar, err)
		}
		if eng.loadReport == nil || !eng.loadReport.HasProblems() {
			t.Fatal("the load report must still record the skip even when booting via the escape hatch")
		}
		found := false
		for _, s := range eng.loadReport.Skipped {
			if s.Name == "fixtureBadSpec" {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected the report to name the skipped 'fixtureBadSpec', got: %+v", eng.loadReport.Skipped)
		}
	})
}
