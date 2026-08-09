//go:build identity

package app

import (
	"os"

	"github.com/znasllc-io/memql/component/deploycontrol"
	"github.com/znasllc-io/memql/component/identity"
)

// setupDeployControlService stands up the DeployControlService gRPC
// surface behind the memQL Deployment Console (znasllc-io/memql#725 +
// #728). It lives on the identity node because the identity binary
// hosts the admin portal; the same audit logger that backs the rest
// of the identity service backs the console's write-action audit
// trail.
//
// Called from build_identity.go after transportBase() has constructed
// the gRPC server, mirroring setupWorkerService() on the agent node.
func (a *App) setupDeployControlService() {
	if a == nil {
		return
	}
	if a.grpcServer == nil {
		a.fatal("deploy-control setup: grpc server not initialized before setup",
			"component", identity.ComponentName)
	}

	// Mirror the audit logger wired in integrationsIdentity: slog
	// stream + the engine-backed v1:identity:auditEvent sink. The
	// engine satisfies identity.EngineExecutor directly.
	auditLogger := &identity.SlogAuditLogger{
		Logger: a.Logger,
		DB: &identity.EngineAuditSink{
			Engine: a.engine,
			Logger: a.Logger,
		},
	}

	// RepoRoot anchors on-disk overlay/lockfile reads + action-script
	// invocation. MEMQL_DEPLOY_REPO_ROOT overrides for deployments
	// where the checkout lives outside the process working directory;
	// otherwise the working directory (the repo root in dev + the
	// image's app dir in the cluster) is used.
	repoRoot := os.Getenv("MEMQL_DEPLOY_REPO_ROOT")

	svc, err := deploycontrol.NewService(deploycontrol.Options{
		Logger:   a.Logger,
		Audit:    auditLogger,
		RepoRoot: repoRoot,
		// Engine persists deployments as v1:cluster:deployment records
		// (#1872): write RPCs record at deploy start + transition on
		// resolution. The engine satisfies identity.EngineExecutor.
		Engine: a.engine,
	})
	if err != nil {
		a.fatal("deploy-control service: build failed", "error", err,
			"component", identity.ComponentName)
	}

	a.grpcServer.RegisterService(svc.Register)

	// Bridge the SAME service instance onto MemqlService.Stream (memql#3311).
	// DeployControlService is unary-only, which a browser -- and every other
	// WebSocket client -- structurally cannot dial, leaving the VS Code
	// extension and the portal with no route to the deploy surface. Handing
	// the bridge the identical *deploycontrol.Service (not a second
	// construction) is what makes the streamed and unary paths share one role
	// gate and one audit logger; they cannot drift because there is only one
	// of each.
	a.grpcServer.SetDeployControlHandler(svc)

	// The mesh route in (memql#3380) is installed later, in app/cluster.go:
	// the NodeServer it hangs off does not exist yet at this point in the
	// identity build. This method's job is to construct the ONE service
	// instance all three surfaces share -- unary, streamed, and forwarded --
	// which is what keeps the role matrix and the audit write from drifting
	// between them. Nothing here is per-surface.

	a.deployControlService = svc
	// Deploy / RollbackDeployment are automation-driven (#2115 step 6
	// retired the synchronous Go apply): they kick off the lifecycle and the
	// deploy pack automations (anchored on this binary via anchorDeployPack)
	// own promote + the terminal transition.
	a.Logger.Info("deploy-control service registered on identity node "+
		"(automation-driven: Deploy/RollbackDeployment kick off the deploy pack "+
		"automations which own promote + terminal, #2115)",
		"component", identity.ComponentName)
}
