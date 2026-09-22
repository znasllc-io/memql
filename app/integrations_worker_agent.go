//go:build agent

package app

import (
	"context"
	"errors"
	"os"
	"strings"

	memqlgrpc "github.com/znasllc-io/memql/component/grpc"
	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/server"
	"github.com/znasllc-io/memql/component/worker"
	"github.com/znasllc-io/memql/component/workjournal"
	agentworker "github.com/znasllc-io/memql/integrations/agent/worker"
	"github.com/znasllc-io/memql/integrations/work"
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
		// Taken from the service rather than read from MEMQL_NODE_ID again:
		// the service is what STAMPS connectedNodeId, so a second reading is
		// a second chance to disagree, and disagreeing here means forwarding
		// every call to a peer for machines connected to this very process.
		// The cross-node forward itself (Remote) is injected later, in the
		// cluster phase -- it needs the PeerManager, which does not exist yet.
		SelfNodeId: svc.NodeId(),
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

	// THE REGISTRATION WATCHER (epic memql#5327, design D1). One subscriber,
	// two decisions: end this replica's stream for a machine whose row has
	// been revoked, and end it for one another replica has taken over.
	//
	// It is wired HERE rather than inside worker.Service because it needs the
	// event bus, which the service has no business holding: the service owns
	// streams, and this reads the graph. The same split
	// edge.SiteInvalidationSubscriber makes, for the same reason.
	//
	// The node id comes off the SERVICE rather than from MEMQL_NODE_ID again,
	// exactly as the dispatcher's does above and for a sharper version of the
	// same reason: the service is what stamps connectedNodeId, and a watcher
	// that disagreed about which node it is would read every one of its own
	// stamps as somebody else's and drain every stream it holds.
	a.Dependencies = append(a.Dependencies, worker.NewRegistrationEventSubscriber(
		a.Logger,
		a.eventBus,
		worker.NewRegistrationWatcher(svc.Registry(), svc.NodeId(), a.Logger),
	))

	a.Logger.Info("worker service registered on agent node")
}

// workerAuditorForApp resolves an Auditor for the agent build (epic
// memql#5327, design D14).
//
// IT RETURNED NoopAuditor, and the comment that stood here explained why that
// was temporary: "the agent node typically does not own an identity
// AuditLogger (those live on the identity binary) ... the SlogAuditLogger gets
// wired during identity rollout when audit relays for non-identity nodes go
// in." The relay never landed and the no-op did, so every `worker_registered`
// and `worker_disconnected` the stream server emitted -- on every agent
// replica, for every machine, for the life of the feature -- went nowhere.
// Pairing a machine was audited; connecting one was not.
//
// No relay is needed. v1:identity:auditEvent is an ordinary graph concept and
// identity.EngineAuditSink writes it through createAuditEvent like any other
// Go writer -- which is exactly what component/packages does from a
// non-identity node. The slog stream stays the always-on destination and the
// row is best-effort beside it, which is SlogAuditLogger's own contract.
func workerAuditorForApp(a *App) worker.Auditor {
	if a == nil || a.engine == nil {
		// No engine means no row to write. The slog half would still work,
		// but a logger with no sink is what the no-op already was and
		// pretending otherwise helps nobody.
		return worker.NoopAuditor{}
	}
	return &worker.IdentityAuditor{
		Logger: a.Logger,
		AuditLogger: &identity.SlogAuditLogger{
			Logger: a.Logger,
			DB:     &identity.EngineAuditSink{Engine: a.engine, Logger: a.Logger},
		},
	}
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
		Logger: a.Logger,
		// No registry: the runner runs the session it is GIVEN. Selection
		// is the Fleet router's, asked by the executor.
		Store:       store,
		Auditor:     auditor,
		MCPEndpoint: mcpEndpoint,
	}
	if minter != nil {
		runner.Minter = minter
	}

	// THE RECORDING (epic memql#5396). Without these two the session still
	// runs and simply records nothing, which is what it did before -- so the
	// wiring IS the feature here, exactly as the app door's is below. A
	// registered seam with no implementation behind it is green and inert.
	runner.Recorder = work.NewSessionWriter(a.engine, a.Logger)
	uploader, bucket := a.resolveBlobStore()
	runner.Contents = &appSessionContentStore{
		engine:   a.engine,
		store:    server.NewEngineLibraryStore(&AttachmentEngineAdapter{Engine: a.engine}),
		uploader: uploader,
		bucket:   bucket,
		logger:   a.Logger,
	}
	a.Logger.Info("app session recording: every action an app takes is a work-spine step",
		"contents_stored", uploader != nil && strings.TrimSpace(bucket) != "",
	)

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

	// THE APP DOOR (epic memql#5096). Installed HERE rather than beside the
	// fleet seam in cluster_worker.go because it needs the SessionRunner,
	// which is built in this function: a door is the same session the
	// delegated-task path opens, reached from a policy instead of from a
	// task. A registered seam with no implementation behind it is green and
	// inert, so the wiring is the feature -- without this line `app:` names
	// resolve to a permanently unavailable provider and every chain simply
	// walks past them.
	if providers := a.engine.Providers(); providers != nil {
		// Kept on the App as well as handed to the registry: the VISION
		// half needs the blob store, which is resolved two phases later in
		// transport (app/transport_agent.go). A registered seam with no
		// stager behind it serves chat and refuses vision BY NAME, which is
		// the right failure but not the intended one.
		appInference := agentworker.NewAppInference(
			dispatcher, runner, &agentworker.EngineStore{Engine: a.engine}, a.Logger,
		)
		a.appInference = appInference
		providers.SetAppInference(appInference)
		a.Logger.Info("app door: this replica can serve inference through a signed-in local app",
			"apps", strings.Join(worker.KnownAppIds(), ", "),
		)

		// THE STEP-HANDOVER SEAM (epic memql#5391, design D7). The door above
		// serves a CHAT turn through an app; this one hands a whole STEP to
		// one, which is what a tool-needing call resolved to an `app:` entry
		// becomes. Same executor, same consent gates, same session runner --
		// a routed step and a delegated task are the same act.
		//
		// The wiring IS the feature here as much as it is above: without this
		// line the router still resolves a `session` winner and every one of
		// them refuses as an unwired seam.
		providers.SetAppSessionDelegate(agentworker.NewAppSessionDelegate(
			exec,
			workjournal.New(
				workjournal.ExecutorFunc(func(ctx context.Context, q string) (any, error) {
					return a.engine.Execute(ctx, q)
				}),
				a.Logger,
				// MEMQL_NODE_ID straight from the environment, for the reason
				// transport_artifacts.go states: the pod carries it in every
				// topology (fieldRef: metadata.name).
				strings.TrimSpace(os.Getenv("MEMQL_NODE_ID")),
			),
			a.engine,
			a.Logger,
		))
		a.Logger.Info("app door: a tool-needing step resolved to an app runs as a session subrun")
	}
}
