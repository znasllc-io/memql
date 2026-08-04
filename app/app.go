// Package app implements the phased bootstrap for the memQL service.
// It separates the monolithic mustCreateDependencies into clearly scoped
// phases: config, database, engine, integrations, transport, and cluster.
//
// Build tags control which phases run for each node type binary:
//
//	go build .                  → standalone (all phases, all code)
//	go build -tags cognition .  → cognition binary
//	go build -tags agent .      → agent binary
//	go build -tags planner .    → planner binary
//	go build -tags bff .        → bff binary
//
// The Build() function is defined in build_*.go files gated by build tags.
package app

import (
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component"
	"github.com/znasllc-io/memql/component/automations"
	automationSteps "github.com/znasllc-io/memql/component/automations/steps"
	"github.com/znasllc-io/memql/component/bus"
	"github.com/znasllc-io/memql/component/database"
	memoryNodesDatabase "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
	memqlgrpc "github.com/znasllc-io/memql/component/grpc"
	"github.com/znasllc-io/memql/component/identity/verifier"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/node"
	"github.com/znasllc-io/memql/component/router"
	"github.com/znasllc-io/memql/component/server"
	"github.com/znasllc-io/memql/component/service"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/logger"
)

// Overrides holds injectable factory functions for testing.
// When a field is nil, the real implementation is used.
type Overrides struct {
	NewDatabase       func(args ...database.DatabaseArg) (*memoryNodesDatabase.MemoryNodesDatabase, error)
	NewHTTPServer     func(args ...server.ServerArg) (*server.Server, error)
	NewEngine         func(db *bun.DB, args ...component.Arg) (*memql.MemQLEngine, error)
	FatalWithLogger   func(logger *slog.Logger, msg string, args ...any)
	LoadServiceEnvOpt func() (service.ServiceEnvOptions, error)
	// AssertUnauthenticatedSurface is the boot check that every route
	// reachable without auth middleware is declared (memql#2939). Injectable
	// so the wiring test can drive its error path without a mutable global or
	// an exported test mutator in the shipped binary.
	AssertUnauthenticatedSurface func(routes []string) error
}

// App holds the shared state accumulated across bootstrap phases.
type App struct {
	Logger       *slog.Logger
	Version      string
	Dependencies []common.Dependency

	overrides Overrides

	// Phase 1: config
	mux *http.ServeMux
	// registeredRoutes records every route app code mounts on mux, so the
	// identity binary can assert its unauthenticated surface is declared
	// (memql#2939). Populated only via handleRoute/handleRouteFunc.
	registeredRoutes []string
	// surfaceSealed is set once createHTTPServer has asserted the
	// unauthenticated surface. Registering after that point would mount a
	// route the assertion never saw, so handleRoute/handleRouteFunc refuse
	// it. See the comment on handleRoute.
	surfaceSealed    bool
	httpArgs         []server.ServerArg
	middlewares      []server.MiddlewareFunc
	identityVerifier *verifier.Verifier
	// authDisabled is set on a verifier-consuming node when the master
	// auth toggle MEMQL_IDENTITY_ENABLED is explicitly false
	// (troubleshooting only). It selects the local-dev no-auth admit
	// path in transportBase instead of the verifier chain.
	authDisabled bool

	// Phase 2: database
	db       *memoryNodesDatabase.MemoryNodesDatabase
	registry memoryNodesDatabase.Registry

	// Phase 3: engine
	engine              *memql.MemQLEngine
	eventBus            *events.Bus
	wiring              *bus.Wiring
	automationScheduler *automations.Scheduler
	automationLoader    *automations.Loader
	stepRegistry        *automationSteps.Registry
	// clusterGuard is the cross-replica execution claim (#561, bounded
	// fail-open per #1142). Built for the automation scheduler and shared
	// with the planner integration's plan-execution dispatcher (memql#1363)
	// so both claim through the same DB ledger + counters.
	clusterGuard *automations.ClusterExecutionGuard

	// Phase 3d: the owner-scoped authored-construct runtime (epic memql#954,
	// #961 / #959 live glue): the registry activated bundles register into,
	// plus the per-owner authored scheduler that fires their automations under
	// the author's envelope. Constructed in wireAuthoredRuntime during the
	// engine phase; the cluster owner gate is wired later (cluster phase).
	authoredRuntime   *memql.AuthoredRuntimeRegistry
	authoredScheduler *automations.AuthoredScheduler
	authoredBreaker   *automations.AuthoredBreaker

	// Phase 3c: AI Router -- the single point every AI call flows through.
	// Constructed after the engine is initialized so it can read the
	// provider registry. Embedded in every node that calls AI; it is
	// never a standalone node type.
	router *router.Router

	// Phase 3b: polyphon score engine (set by initPolyphonScoreEngine in cognition+standalone builds)
	// Stored as any to avoid importing component/polyphon in all builds.
	polyphonScoreEngine any

	// Phase 4: integrations
	// Stored as any to avoid importing integrations/stt (pulls in go-openai SDK) in all builds.
	sttProvider any

	// Phase 5: transport
	grpcServer *memqlgrpc.Server
	httpServer *server.Server

	// Phase 6: cluster
	nodeIdentity *node.Identity

	// nodeLifecycle is THIS node's lifecycle state machine
	// (Starting/Ready/Draining/Stopped, epic memql#1259 #4, memql#1268),
	// resolved from the PeerManager during cluster bootstrap. Nil on
	// non-mesh binaries (no PeerManager). MarkNodeReady / BeginNodeDrain
	// drive it from the Run lifecycle.
	nodeLifecycle *node.NodeLifecycle

	// opDrain carries an operator-initiated maintenance/drain request
	// (memql#1270). An owner/admin operator's gRPC NodeMaintenanceMsg
	// lands in the handler, which calls RequestOperatorDrain; that closes
	// opDrain exactly once. app.Run's wait path selects on this channel
	// alongside the OS shutdown signal, so an operator trigger funnels
	// into the IDENTICAL graceful-drain sequence #1269 runs on SIGTERM
	// (Draining advertised in gossip + readiness 503, in-flight drained
	// within MEMQL_SHUTDOWN_GRACE_PERIOD, Stopped, then the Stop sweep) --
	// the operator path is a second TRIGGER into the one drain mechanism,
	// not a re-implementation. opDrainReason records why, for logs.
	// Constructed in cluster() on mesh binaries; nil on non-mesh ones.
	opDrain       chan struct{}
	opDrainOnce   sync.Once
	opDrainReason atomic.Value // string

	// agentForwarder is the AiForwardRouter used on cognition binaries
	// to forward AgentGenerateTurnMsg to agent peers. Nil on all other
	// node types (BFF uses its own aiForwarder on grpcServer for a
	// different forwarding flow).
	agentForwarder *memqlgrpc.AiForwardRouter

	// deliverySubstrate is the durable outbox+cursor mesh delivery substrate
	// (memql#1263), constructed once in the cluster phase and shared by every
	// path migrated onto it: chat-reply (memql#1264) and the client-tool RPC
	// return leg (memql#1265). Nil on single-node / non-mesh binaries.
	deliverySubstrate node.DeliverySubstrate

	// cognitionIntegration is stashed here so cluster.go can inject
	// the agent-turn forwarder after creating it. Stored as `any` to
	// avoid importing integrations/cognition in all builds; the
	// cognition-tagged helper in integrations_cognition_init.go reads
	// it back via type assertion.
	cognitionIntegration any

	// plannerIntegration is the planner-node analog of
	// cognitionIntegration: same cluster.go-injects-forwarder
	// pattern, but on planner-tagged binaries. Owns Plan / Task
	// lifecycle dispatch (separate from cognition's chat-turn
	// routing). Stored as `any` for the same import-isolation
	// reason; the planner-tagged helper in
	// integrations_planner_init.go reads it back.
	plannerIntegration any

	// identityService is the in-house identity provider that owns
	// magic-link auth, JWT issuance, JWKS publishing, and the admin
	// surfaces on identity-tagged binaries. Stored as `any` to avoid
	// importing component/identity in builds that don't need it; the
	// identity-tagged helpers in integrations_identity.go and
	// transport_identity.go read it back via type assertion.
	identityService any

	// workerService is the WorkerService gRPC implementation + the
	// in-memory registry of connected memql-cockpit workers. Set on
	// the agent build only; stored as `any` to avoid importing
	// component/worker in non-agent binaries. The agent-tagged helper
	// in integrations_worker_agent.go reads it back via type
	// assertion.
	workerService any

	// deployControlService is the DeployControlService gRPC
	// implementation behind the memQL Deployment Console
	// (znasllc-io/memql#725 + #728). Set on the identity build only
	// (the identity node hosts the admin portal). Stored as `any` to
	// avoid importing component/deploycontrol in non-identity binaries;
	// the identity-tagged helper in integrations_deploy_control.go
	// reads it back via type assertion, and the in-process admin portal
	// reads it via DeployControlService().
	deployControlService any

	// adminServer is the *admin.AdminServer wired during the identity
	// integrations phase (the /admin/* operator portal). Stored as
	// `any` to avoid importing component/identity/admin in non-identity
	// builds; setupDeployControlService reads it back via type
	// assertion to wire the in-process deployment-status reader
	// (memql#726). Nil on binaries built without -tags identity.
	adminServer any
}

// newApp creates an App with the given overrides, applying defaults for nil fields.
// Called by build-tagged Build() functions in build_*.go files.
func newApp(serviceLogger *slog.Logger, version string, overrides Overrides) *App {
	a := &App{
		Logger:    serviceLogger,
		Version:   version,
		overrides: overrides,
	}

	if a.overrides.NewDatabase == nil {
		a.overrides.NewDatabase = memoryNodesDatabase.NewMemoryNodesDatabase
	}
	if a.overrides.NewHTTPServer == nil {
		a.overrides.NewHTTPServer = server.NewServer
	}
	if a.overrides.NewEngine == nil {
		a.overrides.NewEngine = memql.New
	}
	if a.overrides.FatalWithLogger == nil {
		a.overrides.FatalWithLogger = logger.FatalWithLogger
	}
	if a.overrides.LoadServiceEnvOpt == nil {
		a.overrides.LoadServiceEnvOpt = service.LoadDefaultServiceEnvOptions
	}
	if a.overrides.AssertUnauthenticatedSurface == nil {
		a.overrides.AssertUnauthenticatedSurface = server.AssertUnauthenticatedSurfaceDeclared
	}

	return a
}

// fatal logs a fatal error and exits. Uses the injectable FatalWithLogger.
func (a *App) fatal(msg string, args ...any) {
	a.overrides.FatalWithLogger(a.Logger, msg, args...)
}

// directDBGetter returns the *bun.DB resolver that session-scoped,
// lifetime-held coordination locks (cron leader election, topology
// reconciler leader) must use: the DIRECT (non-pooled) endpoint
// (epic memql#1925). These locks live on one held connection for the
// leader's lifetime, so they CANNOT ride a transaction-mode PgBouncer
// pooler -- it recycles the server backend between statements and would
// silently drop the advisory lock. DirectBunDB() falls back to the main
// (pooled) pool when DIRECT_DSN is unset, so local/dev is unchanged.
func (a *App) directDBGetter() func() *bun.DB {
	return func() *bun.DB {
		if a.db == nil {
			return nil
		}
		return a.db.DirectBunDB()
	}
}

// Engine exposes the MemQL engine wired during the engine-and-bus phase.
// Used by operator subcommands (mint, etc.) that bootstrap the App but
// don't bring up transport. Nil until engineAndBus() has run.
func (a *App) Engine() *memql.MemQLEngine { return a.engine }

// IdentityService exposes the identity service wired during the
// identity integrations phase. Returned as `any` to keep
// component/identity out of non-identity builds' import graphs; the
// identity-tagged subcommand handler type-asserts back to
// *identity.Service. Nil on binaries built without -tags identity.
func (a *App) IdentityService() any { return a.identityService }

// DeployControlService exposes the deploy-control service wired during
// the identity integrations phase. Returned as `any` to keep
// component/deploycontrol out of non-identity builds' import graphs;
// the identity-tagged admin portal type-asserts back to
// *deploycontrol.Service for in-process calls. Nil on binaries built
// without -tags identity.
func (a *App) DeployControlService() any { return a.deployControlService }

// handleRoute mounts an HTTP route and records it, so the identity binary's
// boot check can account for the routes app code mounts -- not just the
// OpenAPI contract's five (memql#2939).
//
// Register through this rather than a.mux directly. A bare a.mux.HandleFunc is
// invisible to the check, which is precisely the silence #2939 exists to
// remove; app/mux_registration_test.go enforces it.
//
// Registering here is also only half of what the check needs. createHTTPServer
// reads a.registeredRoutes ONCE and every Build runs later phases after it
// (a.cluster()), so a route mounted after that point would be served by the
// same mux while the assertion never saw it -- a compliant handleRoute call in
// a late phase passed every gate green. Being recorded is not enough; being
// recorded IN TIME is the requirement, so the set seals when it is asserted
// and registering afterwards is fatal rather than silent.
func (a *App) handleRoute(pattern string, handler http.Handler) {
	a.requireUnsealedSurface(pattern)
	a.registeredRoutes = append(a.registeredRoutes, pattern)
	a.mux.Handle(pattern, handler)
}

// handleRouteFunc is handleRoute for a HandlerFunc.
func (a *App) handleRouteFunc(pattern string, handler http.HandlerFunc) {
	a.requireUnsealedSurface(pattern)
	a.registeredRoutes = append(a.registeredRoutes, pattern)
	a.mux.HandleFunc(pattern, handler)
}

// requireUnsealedSurface refuses a route registered after the unauthenticated
// surface has been asserted. Fail-fast for the same reason the assertion
// itself is fatal rather than a warning: the failure it guards against is one
// nobody would notice, and a boot warning is exactly the signal that went
// unread when the automations routes were added.
func (a *App) requireUnsealedSurface(pattern string) {
	if a.surfaceSealed {
		a.fatal("HTTP route registered after the unauthenticated surface was asserted",
			"pattern", pattern,
			"detail", "createHTTPServer has already checked the registered surface, so this "+
				"route would be served without ever being declared. Register it before "+
				"createHTTPServer runs (see app/build_<tag>.go for the phase order).")
	}
}
