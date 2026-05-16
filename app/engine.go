package app

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/visionarys-io/memql/component/automations"
	automationSteps "github.com/visionarys-io/memql/component/automations/steps"
	"github.com/visionarys-io/memql/component"
	"github.com/visionarys-io/memql/component/bus"
	"github.com/visionarys-io/memql/component/events"
	"github.com/visionarys-io/memql/component/memql"
	nodeMetadata "github.com/visionarys-io/memql/component/metadata"
	"github.com/visionarys-io/memql/component/observe"
	"github.com/visionarys-io/memql/component/router"
)

// engineAndBus creates the MemQL engine, sets up the component bus wiring,
// event bus, telemetry, automation scheduler, and Polyphon score engine.
func (a *App) engineAndBus() {
	// Configure engine database lifecycle hook
	setEngineDatabase := func(target *memql.MemQLEngine) error {
		return target.ConfigureLifecycle(component.WithPrepareHook(func(ctx context.Context) (context.Context, context.CancelFunc, error) {
			if a.db.BunDB() == nil {
				return nil, nil, fmt.Errorf("memory nodes database bun handle not available")
			}
			target.SetDatabaseGetter(a.db.BunDB)
			return ctx, nil, nil
		}))
	}

	memEngine, err := a.overrides.NewEngine(nil)
	if err != nil {
		a.fatal("failed to create memory engine", "error", err, "component", memql.ComponentName)
	}
	a.engine = memEngine

	if err := setEngineDatabase(a.engine); err != nil {
		a.fatal("failed to configure memory engine lifecycle", "error", err, "component", memql.ComponentName)
	}

	if err := a.engine.Init(a.registry); err != nil {
		a.fatal("failed to initialize memory engine", "error", err, "component", memql.ComponentName)
	}
	a.engine.SetServiceVersion(a.Version)
	// Engine.Partition() defaults to "default"; the active partition
	// per request is set on the gRPC envelope and propagated through
	// context (see component/memql/partition_context.go).

	// Initialize metadata collector for enriching mutations with contextual metadata.
	serverInstance := os.Getenv("K_REVISION")
	if serverInstance == "" {
		serverInstance, _ = os.Hostname()
	}
	a.engine.SetMetadataCollector(nodeMetadata.NewCollector(
		nodeMetadata.ServerMeta{
			Region:   os.Getenv("MEMQL_REGION"),
			NodeType: os.Getenv("MEMQL_NODE_TYPE"),
			Version:  a.Version,
			Instance: serverInstance,
		},
		os.Getenv("MEMQL_GEOIP_DB_PATH"),
		a.Logger,
	))

	// Create event bus (creates its own logger via common.NewLogger)
	a.eventBus = events.NewBus()

	// Create channel-based communication wiring
	channelCfg := bus.DefaultChannelConfig()
	a.wiring = bus.NewWiring(channelCfg)

	// Bridge channel-based EventPublish messages into existing pub/sub
	go a.eventBus.RunWithChannel(context.Background(), a.wiring.EventPublishCh)

	// Start telemetry collector for channel metrics
	telemetry := bus.NewTelemetry(a.wiring, a.Logger)
	telemetry.Start(context.Background())

	a.Logger.Info("channel-based component bus initialized",
		"default_buffer", channelCfg.Default,
		"channels", len(a.wiring.Channels()),
	)

	// Wire bus and event bus to engine
	a.engine.SetWiring(a.wiring)
	a.engine.SetEventBus(a.eventBus)

	// Bridge codeProfile concept events into the observe runtime's
	// per-FQN cache. Subscribes immediately; lifecycle is owned by
	// the dependency chain so Stop() removes the subscription on
	// shutdown.
	a.Dependencies = append(a.Dependencies, observe.NewCodeProfileSubscriber(a.Logger, a.eventBus))

	// Create automation scheduler (creates its own logger via common.NewLogger)
	a.automationLoader = automations.NewLoader(automations.LoaderOptions{
		Logger:   nil,
		Registry: a.registry,
	})
	a.stepRegistry = automationSteps.NewRegistry()
	a.automationScheduler, err = automations.NewScheduler(automations.SchedulerOptions{
		Logger:              nil,
		Loader:              a.automationLoader,
		Engine:              a.engine,
		EventBus:            a.eventBus,
		StepRegistry:        a.stepRegistry,
		StepCacheEnabled:    os.Getenv("MEMQL_STEP_CACHE_ENABLED") == "true",
		StepCacheDefaultTTL: 5 * time.Minute,
	})
	if err != nil {
		a.fatal("failed to create automation scheduler", "error", err, "component", automations.ComponentName)
	}
	a.automationScheduler.SetWiring(a.wiring)

	a.Dependencies = append(a.Dependencies, a.engine)
	a.Dependencies = append(a.Dependencies, a.automationScheduler)

	// Kick off the seed materializer in the background. The startup
	// sweep iterates v1:identity:user to materialize per-user seeds
	// (GA + Planner + Trainer rows for every existing user) and
	// subscribes to user-create events for runtime fanout. Done in a
	// goroutine so app boot doesn't wait on the first query round-trip;
	// failures are logged and don't crash boot (a cluster with no
	// users present is the common dev case).
	if sm := a.engine.SeedMaterializer(); sm != nil {
		go func() {
			if err := sm.Start(context.Background()); err != nil {
				a.Logger.Warn("seed materializer failed to start",
					"component", "memql.seedMaterializer", "error", err)
			}
		}()
	}

	// SI Router: embedded in every SI-calling node. Takes the provider
	// registry (for lookup + pricing) and the engine (to write
	// v1:router:call rows via mutationRecordRouterCall). The router is
	// never a separate node; it's a library the agent replier, the
	// gRPC AI handlers, and future policy-driven call sites all share.
	a.router = router.New(a.engine.Providers(), a.engine.Policies(), a.engine, a.Logger)
	a.Logger.Info("SI Router initialized",
		"providers", a.engine.Providers().Count(),
		"policies", a.engine.Policies().Count(),
		"default", a.engine.Providers().Default(),
	)
}
