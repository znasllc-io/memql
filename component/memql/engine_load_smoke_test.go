package memql

import (
	"io"
	"log/slog"
	"testing"

	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// TestEngineInitLoadsFullDSL is the engine-bootstrap smoke test (#599).
//
// It loads the entire embedded DSL tree into the concept registry and runs
// the engine's Init path against it -- the SAME validation the cluster runs
// at startup (concept + relationship validation, spec/shape/function/builtin/
// tool/prompt/provider/policy loaders, cross-registry CQS). No Postgres is
// required: Init's DSL load + validation is DB-free; provider secret
// resolution is lazy (OS-env fallback) and never dereferences the DB here.
//
// This closes the gap that let #597 (an unregistered `dependsOn` relationship
// type) merge green: `dsl lint` and conformance never spin up the engine
// against the loaded concept tree, so the relationship-type validator -- which
// runs only at engine bootstrap -- was never exercised in CI. A regression of
// that class crashes every node at boot; this test turns it into a unit-test
// failure instead.
func TestEngineInitLoadsFullDSL(t *testing.T) {
	// Mirror the app's concept-load sequence (app/database.go): the
	// legacy embedded tree first, then the unified domain-first tree
	// (dsl/<domain>/concepts.memql) where the harness concepts -- and
	// the #597 `dependsOn` relationship -- actually live.
	if err := concept.LoadConcepts(nil); err != nil {
		t.Fatalf("LoadConcepts (legacy embedded tree): %v", err)
	}
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts (dsl/ domain-first tree): %v", err)
	}

	registry := concept.DefaultRegistry()
	if registry == nil || len(registry.List()) == 0 {
		t.Fatal("concept registry is empty after LoadConcepts; embedded DSL tree did not load")
	}

	eng, err := New(nil)
	if err != nil {
		t.Fatalf("construct engine: %v", err)
	}
	// Quiet the loader: with no DB and no seeded secrets, the provider
	// loader logs one WARN per provider as it registers them unavailable
	// (auth resolution falls back to OS env, then skips). That is expected
	// here and not what this test checks, so discard it.
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))

	if err := eng.Init(registry); err != nil {
		t.Fatalf("engine.Init over the full DSL tree failed -- a DSL load/validation "+
			"regression that would crash the cluster at bootstrap: %v", err)
	}
}
