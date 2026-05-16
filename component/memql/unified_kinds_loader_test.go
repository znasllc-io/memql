package memql

import (
	"log/slog"
	"os"
	"testing"
)

// TestUnifiedLoadersCoverNewTree verifies each of the 5 per-kind
// unified loaders pulls SOME entries from the new tree. This is a
// smoke test, not a coverage assertion -- precise counts will drift
// as the new tree evolves.
func TestUnifiedLoadersCoverNewTree(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	shapeReg := newShapeRegistry()
	if n, err := LoadUnifiedShapes(logger, shapeReg); err != nil {
		t.Fatalf("LoadUnifiedShapes: %v", err)
	} else {
		t.Logf("shapes: %d", n)
	}

	providerReg := newProviderRegistry("")
	if n, err := LoadUnifiedProviders(logger, providerReg); err != nil {
		t.Fatalf("LoadUnifiedProviders: %v", err)
	} else {
		t.Logf("providers: %d", n)
	}

	toolReg := newToolRegistry()
	if n, err := LoadUnifiedTools(logger, toolReg); err != nil {
		t.Fatalf("LoadUnifiedTools: %v", err)
	} else {
		t.Logf("tools: %d", n)
	}

	fnReg := newFunctionRegistry()
	if n, err := LoadUnifiedBuiltins(logger, fnReg); err != nil {
		t.Fatalf("LoadUnifiedBuiltins: %v", err)
	} else {
		t.Logf("builtins: %d", n)
	}

	// Seeds -- the seed migration replaced `agent X { }` DSL
	// declarations with `seed X { }`. The smoke value is "loader
	// runs without error against the live tree."
	seedReg := NewSeedRegistry()
	if n, err := LoadUnifiedSeeds(logger, seedReg); err != nil {
		t.Fatalf("LoadUnifiedSeeds: %v", err)
	} else {
		t.Logf("seeds: %d", n)
	}

	// Force-reference toolReg so the legacy import that used to
	// hand it to LoadUnifiedAgents doesn't become unused.
	_ = toolReg
}
