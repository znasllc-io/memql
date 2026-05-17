package memql

import (
	"log/slog"
	"os"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// TestLoadUnifiedFunctions_RegistersFromNewTree verifies the unified
// function loader extracts function slices from consolidated files
// in the new tree and registers them via the legacy parse pipeline.
func TestLoadUnifiedFunctions_RegistersFromNewTree(t *testing.T) {
	// Need concepts loaded first so @useConcept resolution works.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	if _, err := LoadUnifiedConcepts(logger); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}

	registry := newFunctionRegistry()
	total, counts, err := LoadUnifiedFunctions(logger, registry, memoryNodes.DefaultRegistry())
	if err != nil {
		t.Fatalf("LoadUnifiedFunctions: %v", err)
	}
	if total == 0 {
		t.Fatal("loader registered 0 functions; expected dozens")
	}
	t.Logf("registered %d functions from unified tree: %v", total, counts)
}
