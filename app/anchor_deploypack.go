//go:build identity

// anchor_deploypack.go mounts the deploy pack (examples/deploypack) on the
// identity binary -- the node that hosts the Deploy Console + deploycontrol --
// so the E2.3/E2.4 lifecycle automations (driveDeploymentInProgress,
// recordReconciledState) actually load and fire where deployment records are
// written. Without this anchor the pack is mountable but mounted in no prod
// binary (#2115, step 1).
//
// The mount is gated behind MEMQL_DEPLOY_AUTOMATION_DRIVEN and happens at
// init() time -- BEFORE app.Build()'s engineAndBus() phase loads the DSL tree,
// so the pack's tree + IntegrationProvider are registered in time to be loaded.
// (setupDeployControlService runs in a later phase, too late to register a
// tree.) The deploypack package's own auto-register init lives behind the
// separate `deploypack` build tag and does NOT fire here, so this conditional
// Register is the single mount point: flag off -> pack absent -> the
// synchronous Go apply stays authoritative; flag on -> pack mounted + the
// thinned automation-driven Deploy/RollbackDeployment path (service.go) takes
// over. Both read the same flag, so mounting and the Go path can never diverge.
//
// Default off keeps merging this a no-op on the live identity binary; the
// owner-gated staging cutover (#2115, step 5) flips the flag to validate parity.
package app

import (
	"os"
	"strconv"
	"strings"

	"github.com/znasllc-io/memql/examples/deploypack"
)

// deployAutomationDriven reports whether the automation-driven deploy cutover
// is enabled via MEMQL_DEPLOY_AUTOMATION_DRIVEN. Accepts the usual truthy
// spellings (1/true/yes/on, case-insensitive); anything else (incl. unset) is
// false, preserving the authoritative synchronous Go apply.
func deployAutomationDriven() bool {
	v := strings.TrimSpace(os.Getenv("MEMQL_DEPLOY_AUTOMATION_DRIVEN"))
	if v == "" {
		return false
	}
	if b, err := strconv.ParseBool(v); err == nil {
		return b
	}
	switch strings.ToLower(v) {
	case "yes", "on":
		return true
	}
	return false
}

func init() {
	if deployAutomationDriven() {
		// Mount the embedded DSL tree + register the Go IntegrationProvider
		// before the engine loads its tree. Anchors on MEMQL_DEPLOY_REPO_ROOT
		// for the Executor, mirroring setupDeployControlService.
		deploypack.Register(deploypack.Domain)
	}
}
