//go:build identity

package app

import (
	"context"
	"os"

	"github.com/znasllc-io/memql/component/deploycontrol"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/admin"
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
	})
	if err != nil {
		a.fatal("deploy-control service: build failed", "error", err,
			"component", identity.ComponentName)
	}

	a.grpcServer.RegisterService(svc.Register)

	a.deployControlService = svc
	a.Logger.Info("deploy-control service registered on identity node",
		"component", identity.ComponentName)

	// Wire the read-only /admin/deployments view (memql#726) to the
	// in-process service. The admin server is constructed earlier in
	// integrationsIdentity and stashed on a.adminServer. The reader
	// is a thin adapter that turns the admin handler's
	// (admin-context-stamped) call into a GetDeploymentStatus RPC; the
	// read RPC's owner/admin gate (#728) admits it because the handler
	// stamps an auth.AccessContext from the verified admin claims.
	if adminSrv, ok := a.adminServer.(*admin.AdminServer); ok && adminSrv != nil {
		adminSrv.SetDeployControlReader(&deployControlReaderAdapter{svc: svc})
		a.Logger.Info("deploy-control reader wired into admin portal",
			"component", identity.ComponentName)
	}
}

// deployControlReaderAdapter satisfies admin.DeployControlReader via
// the in-process *deploycontrol.Service (memql#726). Lives here
// rather than in the admin package so the admin layer stays decoupled
// from the concrete deploy-control type -- the package depends on the
// narrow port shape, the wiring layer satisfies it.
type deployControlReaderAdapter struct {
	svc *deploycontrol.Service
}

func (a *deployControlReaderAdapter) DeploymentStatus(ctx context.Context, env string) (*memqlv1.DeploymentStatus, error) {
	return a.svc.GetDeploymentStatus(ctx, &memqlv1.GetDeploymentStatusRequest{Env: env})
}
