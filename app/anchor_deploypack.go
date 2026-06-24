//go:build identity

// anchor_deploypack.go mounts the deploy pack (examples/deploypack) on the
// identity binary -- the node that hosts the Deploy Console + deploycontrol --
// so the E2.3/E2.4 lifecycle automations (driveDeploymentInProgress,
// recordReconciledState) actually load and fire where deployment records are
// written. Without this anchor the pack is mountable but mounted in no prod
// binary (#2115, step 1).
//
// The mount is gated behind MEMQL_DEPLOY_AUTOMATION_DRIVEN and happens in
// anchorDeployPack(), called from build_identity.go's Build() AFTER genesis
// autoload (main.go applies the sealed envelope to the process env before
// app.Build()) and BEFORE engineAndBus() loads the DSL tree -- so the pack's
// tree + IntegrationProvider are registered in time to be loaded AND the flag
// is read at the SAME point as the thinned Deploy path (setupDeployControlService).
//
// Why a bootstrap phase and not init(): a package init() runs before main()
// applies the genesis envelope, so a genesis-only flag would be invisible to it
// -- the pack would not mount while the (later-phase) Deploy thinning still
// kicked in, leaving deploys stuck at in_progress with no automation and no Go
// fallback. Reading the flag in a bootstrap phase makes the mount and the
// thinning agree whether the flag arrives via k8s env OR the genesis envelope.
//
// The deploypack package's own auto-register init lives behind the separate
// `deploypack` build tag and does NOT fire here, so this conditional Register
// is the single mount point: flag off -> pack absent -> the synchronous Go
// apply stays authoritative; flag on -> pack mounted + the thinned
// automation-driven Deploy/RollbackDeployment path (service.go) takes over.
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

// anchorDeployPack mounts the deploy pack on the identity binary when the
// automation-driven cutover is enabled. Called from build_identity.go before
// engineAndBus() so the registered tree + provider are loaded by the engine,
// and after genesis autoload so a genesis-supplied flag is visible. No-op when
// the flag is off (the synchronous Go apply stays authoritative).
func (a *App) anchorDeployPack() {
	if a == nil || !deployAutomationDriven() {
		return
	}
	// Mount the embedded DSL tree + register the Go IntegrationProvider.
	// The provider anchors on MEMQL_DEPLOY_REPO_ROOT for its Executor,
	// mirroring setupDeployControlService.
	deploypack.Register(deploypack.Domain)
	if a.Logger != nil {
		a.Logger.Warn("deploy pack anchored on identity node " +
			"(MEMQL_DEPLOY_AUTOMATION_DRIVEN); the deploy lifecycle automations " +
			"will drive promote + terminal (#2115)")
	}
}
