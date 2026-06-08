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

	// #1061: the heartbeat-based cluster-node prune cron must load with
	// its 10-minute six-field cron schedule. This proves the new
	// automation + its logic + queryStaleClusterNodes parse and register
	// from the unified tree.
	var prune *Automation
	for _, a := range automations {
		if a.Name == "pruneStaleClusterNodes" {
			prune = a
			break
		}
	}
	if prune == nil {
		t.Fatal("pruneStaleClusterNodes automation did not load from the unified tree (#1061)")
	}
	if prune.Schedule != "0 */10 * * * *" {
		t.Errorf("pruneStaleClusterNodes: expected cron schedule %q, got %q", "0 */10 * * * *", prune.Schedule)
	}
	if len(prune.Steps) == 0 {
		t.Error("pruneStaleClusterNodes: expected at least one step")
	}
}
