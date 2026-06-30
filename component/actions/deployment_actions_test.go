package actions

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDeploymentActionsValidateStrictly loads the 9 authored deployment
// actions (dsl/deployment/actions.memql) under Story 8 STRICT capability
// arg-typing and confirms every one validates against the reconciled catalog.
// This is the live proof that the real authored actions satisfy the strict
// rules without weakening the check.
func TestDeploymentActionsValidateStrictly(t *testing.T) {
	// Locate the in-tree deployment actions file relative to this package.
	path := filepath.Join("..", "..", "dsl", "deployment", "actions.memql")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	acts, err := LoadSource(string(raw), "dsl/deployment/actions.memql")
	if err != nil {
		t.Fatalf("the authored deployment actions must validate under strict capability arg-typing: %v", err)
	}
	if len(acts) != 9 {
		t.Fatalf("expected 9 authored deployment actions, got %d", len(acts))
	}
	// Spot-check the integration-backed action resolves to the write capability.
	var sawTagRelease bool
	for _, a := range acts {
		if a.Name == "tagRelease" {
			sawTagRelease = true
			if a.Capability != "integration.github.tagRelease" {
				t.Errorf("tagRelease capability = %q, want integration.github.tagRelease", a.Capability)
			}
			if a.SideEffect != "write" {
				t.Errorf("tagRelease sideEffect = %q, want write", a.SideEffect)
			}
		}
	}
	if !sawTagRelease {
		t.Error("expected a tagRelease action among the deployment actions")
	}
}
