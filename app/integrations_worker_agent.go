//go:build agent

package app

import (
	"errors"
	"os"
	"strings"

	memqlgrpc "github.com/znasllc-io/memql/component/grpc"
	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/worker"
	agentworker "github.com/znasllc-io/memql/integrations/agent/worker"
)

// setupWorkerService stands up the WorkerService gRPC surface, the
// in-memory worker registry, and the agent-side dispatcher that
// bridges the engine's tool loop to the connected workers. Called
// from transportAgent after the gRPC server is constructed.
func (a *App) setupWorkerService() {
	if a == nil {
		return
	}
	if a.engine == nil {
		a.fatal("worker setup: engine not initialized")
	}
	if a.grpcServer == nil {
		a.fatal("worker setup: grpc server not initialized before worker setup")
	}

	store := &worker.EngineStore{Engine: a.engine, Logger: a.Logger}
	auditor := workerAuditorForApp(a)

	svc, err := worker.NewService(worker.Options{
		Logger:  a.Logger,
		Store:   store,
		Auditor: auditor,
	})
	if err != nil {
		a.fatal("worker service: build failed", "error", err)
	}

	// Mount the gRPC service handler on the existing grpc server.
	a.grpcServer.RegisterService(svc.Register)

	// Wrap the existing stream interceptor with the worker-aware
	// interceptor so mql_wkr_<token> is accepted on WorkerService
	// paths and rejected everywhere else.
	resolver := &memqlgrpc.EngineWorkerTokenResolver{Engine: a.engine, Logger: a.Logger}
	if base := a.grpcServer.StreamInterceptor(); base != nil {
		a.grpcServer.SetStreamInterceptor(memqlgrpc.NewWorkerAwareStreamInterceptor(base, resolver, a.Logger))
	}

	// Build the agent-side dispatcher that routes tool-loop calls
	// into the registry.
	dispatcher, err := agentworker.NewDispatcher(agentworker.Options{
		Logger:   a.Logger,
		Registry: svc.Registry(),
		Engine:   a.engine,
		Auditor:  auditor,
		Store:    &agentworker.EngineStore{Engine: a.engine},
	})
	if err != nil {
		a.fatal("worker dispatcher: build failed", "error", err)
	}

	integration := agentworker.NewIntegration(dispatcher, svc.Registry(), a.engine, a.Logger)
	if err := a.engine.RegisterIntegration(integration); err != nil {
		a.fatal("worker integration: register failed", "error", err)
	}

	a.setupCockpitAppExecutor(svc, dispatcher, store, auditor)

	a.workerService = svc
	a.Dependencies = append(a.Dependencies, svc)
	a.Logger.Info("worker service registered on agent node")
}

// workerAuditorForApp resolves an Auditor for the agent build. The
// agent node typically does not own an identity AuditLogger (those
// live on the identity binary); for now we hand back a NoopAuditor
// and the SlogAuditLogger gets wired during identity rollout when
// audit relays for non-identity nodes go in.
func workerAuditorForApp(a *App) worker.Auditor {
	if a == nil {
		return worker.NoopAuditor{}
	}
	// Identity service exposes the audit logger interface only on
	// identity-tagged binaries; a future identity-side relay will
	// publish audit events back to agent nodes for cross-cutting
	// security telemetry. Until then this is a no-op.
	var _ identity.AuditLogger = (*identity.SlogAuditLogger)(nil)
	return worker.NoopAuditor{}
}

// ErrWorkerNotConfigured is returned by helpers that look up the
// worker service when it hasn't been wired (e.g., on non-agent
// binaries that nonetheless invoked an agent-only helper).
var ErrWorkerNotConfigured = errors.New("worker service not configured on this build")

// lookupWorkerIntegration finds the registered agentworker integration so
// transport wiring can inject the GCS attachment uploader (memql#794). Mirrors
// lookupWorkbenchIntegration. Returns nil if the integration didn't register
// or the engine isn't available.
func (a *App) lookupWorkerIntegration() *agentworker.Integration {
	if a == nil || a.engine == nil {
		return nil
	}
	provider := a.engine.IntegrationByName("agentworker")
	if provider == nil {
		return nil
	}
	if integ, ok := provider.(*agentworker.Integration); ok {
		return integ
	}
	return nil
}

// setupCockpitAppExecutor completes the container-executor
// registration made at init() by integrations/agent/worker
// (memql#4361). The seam registers a NAME on every agent binary so
// ValidateExecutorBackend can accept "cockpit-app:*" at task
// creation; the implementation needs a worker registry and an engine,
// neither of which exists at init() time, so it is installed here.
//
// A node that cannot mint the back-channel credential still installs
// the executor. The refusal then comes from Run with a reason naming
// the missing credential, which is far more useful than the executor
// being silently absent and a Task failing with "no backend
// registered" on a node that plainly has one.
func (a *App) setupCockpitAppExecutor(
	svc *worker.Service,
	dispatcher *agentworker.Dispatcher,
	store *worker.EngineStore,
	auditor worker.Auditor,
) {
	minter := worker.NewBootstrapCredentialMinter(
		os.Getenv("MEMQL_IDENTITY_VERIFIER_BASE_URL"),
		os.Getenv("MEMQL_NODE_BOOTSTRAP_TOKEN"),
	)
	if minter == nil {
		a.Logger.Warn("cockpit-app executor: no back-channel credential minter",
			"reason", "MEMQL_IDENTITY_VERIFIER_BASE_URL or MEMQL_NODE_BOOTSTRAP_TOKEN is unset",
			"effect", "delegated app sessions will refuse with a named reason rather than run without MemQL's tools",
		)
	}

	mcpEndpoint := strings.TrimRight(os.Getenv("MEMQL_MCP_PUBLIC_URL"), "/")
	if mcpEndpoint != "" {
		mcpEndpoint += "/mcp"
	}

	runner := &worker.SessionRunner{
		Logger:      a.Logger,
		Registry:    svc.Registry(),
		Store:       store,
		Auditor:     auditor,
		MCPEndpoint: mcpEndpoint,
	}
	if minter != nil {
		runner.Minter = minter
	}

	exec, err := agentworker.NewCockpitAppExecutor(
		a.Logger, dispatcher, runner, &agentworker.EngineStore{Engine: a.engine},
	)
	if err != nil {
		a.fatal("cockpit-app executor: build failed", "error", err)
	}
	agentworker.InstallCockpitAppExecutor(
		exec.WithLedger(&agentworker.LedgerWriter{Engine: a.engine}),
	)
	a.Logger.Info("cockpit-app container executor installed",
		"mcp_endpoint", mcpEndpoint,
		"credential_minter", minter != nil,
	)
}
