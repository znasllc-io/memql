// Build-speed #1507 (carrier-base dedup): aks-deploy.sh must build the shared
// carrier-base image ONCE and point each carrier compile at it via the
// Dockerfile's CARRIER_BASE build-arg, so a release cut does "1 base build + N
// tag-only compiles" instead of N full carrier builds. These are static
// assertions over the script text (no live ACR), in the same package as
// aks_deploy_test.go (reuses aksScript()).
package deploy

import (
	"os"
	"strings"
	"testing"
)

func aksDeployText(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(aksScript(t, "aks-deploy.sh"))
	if err != nil {
		t.Fatalf("read aks-deploy.sh: %v", err)
	}
	return string(raw)
}

func TestAksDeployBuildsCarrierBaseOnce(t *testing.T) {
	text := aksDeployText(t)

	// The shared base is queued by its own function (async since #1512).
	if !strings.Contains(text, "function queue_carrier_base()") {
		t.Error("expected a queue_carrier_base() function (#1507/#1512)")
	}
	// The base build stops at the carrier-base stage (no compile).
	if !strings.Contains(text, "--target carrier-base") {
		t.Error("queue_carrier_base must use `--target carrier-base` to build only the prefix stage")
	}
	// A repo constant names the shared base image.
	if !strings.Contains(text, `CARRIER_BASE_REPO="memql-carrier-base"`) {
		t.Error("expected the CARRIER_BASE_REPO=\"memql-carrier-base\" constant")
	}
	// build_and_push queues the base (wave A) before the carriers (wave B).
	bp := text[strings.Index(text, "function build_and_push()"):]
	baseIdx := strings.Index(bp, "queue_carrier_base ")
	carrierLoopIdx := strings.Index(bp, "CARRIER_NODE_TYPES[@]")
	if baseIdx < 0 {
		t.Fatal("build_and_push must queue the carrier base")
	}
	if carrierLoopIdx < 0 || baseIdx > carrierLoopIdx {
		t.Error("the carrier base must be queued BEFORE the carrier build loop (carriers FROM the base)")
	}
}

func TestAksDeployCarrierBuildsPassCarrierBaseArg(t *testing.T) {
	text := aksDeployText(t)
	// queue_build passes the CARRIER_BASE build-arg (only for carrier nodes,
	// guarded by is_carrier_node) pointing at the pushed base tag.
	if !strings.Contains(text, `CARRIER_BASE=${ACR_LOGIN_SERVER}/${CARRIER_BASE_REPO}:${tag}`) {
		t.Error("carrier builds must pass --build-arg CARRIER_BASE=<acr>/<repo>:<tag> so the compile reuses the shared base")
	}
}
