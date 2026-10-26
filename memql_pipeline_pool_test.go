package main

import (
	"testing"

	"github.com/znasllc-io/memql/component/pipelines"
)

// The installed controller must already support pool placement before this
// manifest lands. Commands and coverage remain pinned by the complete reviewed
// fixture; this assertion protects the routing and receipt-reuse boundaries.
func TestFleetPoolManifestKeepsDedicatedEffectsAndFreshChecks(t *testing.T) {
	spec := engineManifest(t).Pipeline
	pool := 0
	for _, stage := range spec.Stages {
		for _, step := range stage.Steps {
			if step.Placement != pipelines.PlacementPool {
				continue
			}
			pool++
			if step.Verification || step.CPUMilli <= 0 || step.MemoryMiB <= 0 {
				t.Errorf("%s/%s must execute afresh with explicit resource bounds", stage.Name, step.Name)
			}
			if step.ImageBuild != nil || step.GitHubReadToken || step.ImagePullSecret != "" || step.CheckoutHistory != "" || pipelines.ExecutionOf(step.Execution) != pipelines.ExecutionContainer || len(step.Needs) != 0 {
				t.Errorf("%s/%s moved dedicated execution into the pool", stage.Name, step.Name)
			}
		}
	}
	if pool != 48 {
		t.Fatalf("expected the reviewed 48 portable pool steps, got %d", pool)
	}
}
