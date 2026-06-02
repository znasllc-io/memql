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
}

// App holds the shared state accumulated across bootstrap phases.
type App struct {
	Logger       *slog.Logger
	Version      string
	Dependencies []common.Dependency

	overrides Overrides

	// Phase 1: config
	mux              *http.ServeMux
	httpArgs         []server.ServerArg
	middlewares      []server.MiddlewareFunc
	identityVerifier *verifier.Verifier

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

	// Phase 3c: SI Router -- the single point every SI call flows through.
	// Constructed after the engine is initialized so it can read the
	// provider registry. Embedded in every node that calls SI; it is
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

	// agentForwarder is the SIForwardRouter used on cognition binaries
	// to forward AgentGenerateTurnMsg to agent peers. Nil on all other
	// node types (BFF uses its own aiForwarder on grpcServer for a
	// different forwarding flow).
	agentForwarder *memqlgrpc.SIForwardRouter

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

	return a
}

// fatal logs a fatal error and exits. Uses the injectable FatalWithLogger.
func (a *App) fatal(msg string, args ...any) {
	a.overrides.FatalWithLogger(a.Logger, msg, args...)
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
