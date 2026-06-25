//go:build identity

// anchor_deploypack.go mounts the deploy pack (examples/deploypack) on the
// identity binary -- the node that hosts the Deploy Console + deploycontrol --
// so the E2.3/E2.4 lifecycle automations (driveDeploymentInProgress,
// recordReconciledState) load and fire where deployment records are written.
//
// Since #2115 step 6 retired the synchronous Go apply, this anchor is
// UNCONDITIONAL: the gRPC Deploy / RollbackDeployment actions only validate +
// kick off the lifecycle (transition the record to in_progress), and the pack
// automations own promote + the terminal transition. Without the pack mounted
// a deploy would strand at in_progress with nothing to drive it -- so the pack
// MUST be present on the identity binary.
//
// anchorDeployPack() is called from build_identity.go's Build() AFTER genesis
// autoload (main.go applies the sealed envelope to the process env before
// app.Build()) and BEFORE engineAndBus() loads the DSL tree -- so the pack's
// tree + IntegrationProvider are registered in time to be loaded.
//
// The deploypack package's own auto-register init lives behind the separate
// `deploypack` build tag and does NOT fire here, so this Register is the
// single mount point on the identity binary.
package app

import (
	"github.com/znasllc-io/memql/examples/deploypack"
)

// anchorDeployPack mounts the deploy pack on the identity binary: it mounts
// the embedded DSL tree and registers the Go IntegrationProvider (anchored on
// MEMQL_DEPLOY_REPO_ROOT for its Executor, mirroring setupDeployControlService),
// so the deploy lifecycle automations drive promote + the terminal transition
// (#2115).
func (a *App) anchorDeployPack() {
	if a == nil {
		return
	}
	deploypack.Register(deploypack.Domain)
	if a.Logger != nil {
		a.Logger.Info("deploy pack anchored on identity node; the deploy " +
			"lifecycle automations drive promote + terminal (#2115)")
	}
}
