// Build-speed #1512 (interim parallelization): the operator build path in
// aks-deploy.sh must queue the engine/carrier builds with `az acr build
// --no-wait` and poll them to completion (ACR runs the queue concurrently),
// instead of serial foreground builds. Two waves preserve the #1507 carrier
// dependency: wave A (base + engine-only nodes) before wave B (carriers). These
// are static assertions over the script text (no live ACR), same package as
// aks_deploy_test.go (reuses aksScript()/aksDeployText()).
package deploy

import (
	"strings"
	"testing"
)

func TestAksDeployQueuesBuildsAsync(t *testing.T) {
	text := aksDeployText(t)

	// Builds are queued async and polled (not run foreground/serially).
	for _, want := range []string{
		"--no-wait",
		"--query runId",
		"az acr task show-run",
		"function queue_build()",
		"function wait_for_acr_runs()",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("expected aks-deploy.sh to contain %q for the async ACR build path (#1512)", want)
		}
	}
}

func TestAksDeployTwoWaveOrdering(t *testing.T) {
	text := aksDeployText(t)
	bp := text[strings.Index(text, "function build_and_push()"):]

	// Wave A = base + engine-only nodes; wave B = carriers.
	waveAIdx := strings.Index(bp, "ENGINE_ONLY_NODE_TYPES[@]")
	waveBIdx := strings.Index(bp, "CARRIER_NODE_TYPES[@]")
	if waveAIdx < 0 {
		t.Fatal("build_and_push must queue ENGINE_ONLY_NODE_TYPES (wave A) alongside the base")
	}
	if waveBIdx < 0 {
		t.Fatal("build_and_push must queue CARRIER_NODE_TYPES (wave B)")
	}
	if waveAIdx > waveBIdx {
		t.Error("wave A (base + engine-only) must be queued before wave B (carriers), which build FROM the base")
	}

	// Each wave must be awaited (queue-all-then-poll, not queue-then-block-each).
	firstWait := strings.Index(bp, "wait_for_acr_runs")
	if firstWait < 0 || firstWait < waveAIdx {
		t.Error("wave A must be polled (wait_for_acr_runs) after it is queued")
	}
	if strings.Count(bp, "wait_for_acr_runs ") < 2 {
		t.Error("expected both waves to be polled (two wait_for_acr_runs calls)")
	}

	// The base must finish (wave A poll) before the carriers are queued, so
	// their FROM <base> can pull it.
	waveBPoll := strings.LastIndex(bp, "wait_for_acr_runs")
	if waveBPoll <= waveBIdx {
		t.Error("wave B carriers must be queued before their poll, and after wave A's poll")
	}
}

// ENGINE_ONLY_NODE_TYPES must be exactly ENGINE_NODE_TYPES minus the carriers
// (the nodes with no carrier-base dependency), so wave A doesn't accidentally
// include a carrier (which would race the base) or drop an engine node.
func TestAksDeployEngineOnlyNodeTypes(t *testing.T) {
	text := aksDeployText(t)
	if !strings.Contains(text, "readonly ENGINE_ONLY_NODE_TYPES=(identity voice)") {
		t.Error("expected ENGINE_ONLY_NODE_TYPES=(identity voice) -- ENGINE_NODE_TYPES minus CARRIER_NODE_TYPES")
	}
}
