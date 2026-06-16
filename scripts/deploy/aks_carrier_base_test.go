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

	if !strings.Contains(text, "function build_carrier_base()") {
		t.Error("expected a build_carrier_base() function (#1507)")
	}
	// The base build stops at the carrier-base stage (no compile).
	if !strings.Contains(text, "--target carrier-base") {
		t.Error("build_carrier_base must use `--target carrier-base` to build only the prefix stage")
	}
	// A repo constant names the shared base image.
	if !strings.Contains(text, `CARRIER_BASE_REPO="memql-carrier-base"`) {
		t.Error("expected the CARRIER_BASE_REPO=\"memql-carrier-base\" constant")
	}
	// build_and_push builds the base before looping the per-node builds.
	bp := text[strings.Index(text, "function build_and_push()"):]
	baseIdx := strings.Index(bp, "build_carrier_base ")
	loopIdx := strings.Index(bp, "for nt in ")
	if baseIdx < 0 {
		t.Fatal("build_and_push must call build_carrier_base")
	}
	if loopIdx < 0 || baseIdx > loopIdx {
		t.Error("build_carrier_base must run BEFORE the per-node build loop")
	}
}

func TestAksDeployCarrierBuildsPassCarrierBaseArg(t *testing.T) {
	text := aksDeployText(t)
	// build_one passes the CARRIER_BASE build-arg (only for carrier nodes,
	// guarded by is_carrier_node) pointing at the pushed base tag.
	if !strings.Contains(text, `CARRIER_BASE=${ACR_LOGIN_SERVER}/${CARRIER_BASE_REPO}:${tag}`) {
		t.Error("carrier builds must pass --build-arg CARRIER_BASE=<acr>/<repo>:<tag> so the compile reuses the shared base")
	}
}
