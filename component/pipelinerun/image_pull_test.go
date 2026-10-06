package pipelinerun

import (
	"strings"
	"testing"
)

func TestImagePullSecretUsesOwnerResolverAcrossAgentHop(t *testing.T) {
	manifest := strings.Replace(driveManifest, "secrets: [SHOP_TOKEN]", "imagePullSecret: SHOP_TOKEN", 1)
	dh := newDriveHarness(t, manifest)
	run := dh.openRun(t, pushOpening())
	// A different agent, with no originating agent's local state, resolves the
	// credential under the same stored pipeline owner and completes the run.
	deliver(t, dh.peer("agent-b"), run)
	got, _ := dh.store.run(run.ID)
	if got.Conclusion != ConclusionSuccess {
		t.Fatalf("run failed: %s", got.RefusalMessage)
	}
	var found bool
	for _, req := range dh.exec.sent() {
		if req.StepKey != "tests.unit" {
			continue
		}
		found = true
		if req.Step.ImagePullSecret != "SHOP_TOKEN" || req.Secrets["SHOP_TOKEN"] != shopSecret {
			t.Fatal("owner credential did not reach executor")
		}
		if _, exposed := req.Environment()["SHOP_TOKEN"]; exposed {
			t.Fatal("registry credential exported to command")
		}
	}
	if !found {
		t.Fatal("private-image step never executed")
	}
}
