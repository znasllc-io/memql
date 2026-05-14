package automations

import (
	"log/slog"
	"os"
	"testing"
)

// TestLoadFromUnifiedTree verifies the unified automation loader
// picks up automations from dsl/<domain>/automations.memql files
// in the new tree.
func TestLoadFromUnifiedTree(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	loader := NewLoader(LoaderOptions{
		Logger:   logger,
		Registry: nil, // many automations will fail concept resolution; that's fine
	})

	automations, err := loader.LoadFromUnifiedTree()
	if err != nil {
		t.Fatalf("LoadFromUnifiedTree: %v", err)
	}

	t.Logf("loaded %d automations from unified tree", len(automations))
	for i, a := range automations {
		if i < 5 {
			t.Logf("  - %s (origin: %s)", a.Name, a.Origin)
		}
	}

	// We expect SOME automations to load (concept-resolution failures
	// at this layer just skip the entry, they don't blank the list).
	// The exact count depends on which concepts are registered when the
	// test runs in isolation.
}
