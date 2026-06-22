package app

import (
	"context"
	"os"
	"time"

	"github.com/znasllc-io/memql/component/automations"
	automationSteps "github.com/znasllc-io/memql/component/automations/steps"
	"github.com/znasllc-io/memql/component/bus"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
	nodeMetadata "github.com/znasllc-io/memql/component/metadata"
	"github.com/znasllc-io/memql/component/observe"
	"github.com/znasllc-io/memql/component/router"
)

// engineAndBus creates the MemQL engine, sets up the component bus wiring,
// event bus, telemetry, automation scheduler, and Polyphon score engine.
func (a *App) engineAndBus() {
	memEngine, err := a.overrides.NewEngine(nil)
	if err != nil {
		a.fatal("failed to create memory engine", "error", err, "component", memql.ComponentName)
	}
	a.engine = memEngine

	// Start the database synchronously and wire its handle into the
	// engine BEFORE Init. Init runs the provider loader, which
	// resolves auth.apiKey="${MEMQL_SI_X_API_KEY}" placeholders
	// against v1:platform:globalSecret rows -- that lookup needs a
	// live DB handle. Database.Start is idempotent
	// (ErrLifecycleAlreadyRunning is swallowed), so main.go's later
	// StartDependencies call is safe.
	//
	// Subtle: the context passed to a.db.Start becomes the lifetime
	// context of the database's run loop. We MUST NOT cancel it when
	// this function returns -- doing so triggers cleanupAfterRun,
	// which nils out d.Bun and turns every subsequent query into
	// "memory engine database not configured." Use a process-
	// lifetime context here. The wait-for-ready is bounded by a
	// SEPARATE short-lived timeout so a stuck DB still fails fast at
	// boot without coupling that timeout to the run loop's lifetime.
	a.db.Start(context.Background())
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer waitCancel()
	select {
	case <-a.db.Ready():
	case <-waitCtx.Done():
		a.fatal("database did not become ready before engine init", "error", waitCtx.Err(), "component", memql.ComponentName)
	}
	if a.db.BunDB() == nil {
		a.fatal("memory nodes database bun handle not available", "component", memql.ComponentName)
	}
	a.engine.SetDatabaseGetter(a.db.BunDB)

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

	// #561: elect one cluster-wide owner of SCHEDULED (cron) automations via
	// a Postgres advisory lock, so the daily purges + the */5min feedback
	// sweep run once across the whole cluster instead of once per node-type
	// (and once per replica once the node-types scale). Started BEFORE the
	// scheduler so the lock is held by the time the first cron can fire.
	//
	// memql#1930: the lock is session-scoped and held for the leader's
	// LIFETIME, so it resolves its connection via the DIRECT (non-pooled)
	// endpoint -- a transaction-mode pooler would recycle the backend out
	// from under the held lock. Unchanged when DIRECT_DSN is unset.
	cronLeader := automations.NewCronLeader(a.directDBGetter(), a.Logger)
	a.Dependencies = append(a.Dependencies, cronLeader)

	// #561: cross-replica idempotency for EVENT-triggered automations. When a
	// node-type runs >=2 replicas an event can reach more than one (the
	// EventBridge dedup is per-process); the guard claims each execution in
	// the DB so exactly one replica runs it, and logs/counts any prevented
	// duplicate so a double-fire is always observable.
	clusterGuard := automations.NewClusterExecutionGuard(a.db.BunDB, a.Logger)
	a.Dependencies = append(a.Dependencies, clusterGuard)
	// Stash for the planner integration's plan-execution claim (memql#1363):
	// the same guard instance gates approved-plan dispatch across replicas.
	a.clusterGuard = clusterGuard

	a.automationScheduler, err = automations.NewScheduler(automations.SchedulerOptions{
		Logger:              nil,
		Loader:              a.automationLoader,
		Engine:              a.engine,
		EventBus:            a.eventBus,
		StepRegistry:        a.stepRegistry,
		StepCacheEnabled:    os.Getenv("MEMQL_STEP_CACHE_ENABLED") == "true",
		StepCacheDefaultTTL: 5 * time.Minute,
		LeaderGate:          cronLeader.IsLeader,
		ClusterGuard:        clusterGuard,
	})
	if err != nil {
		a.fatal("failed to create automation scheduler", "error", err, "component", automations.ComponentName)
	}
	a.automationScheduler.SetWiring(a.wiring)

	// F.5 of the ctx-envelope purge: wire the multi-step Logic
	// dispatcher. Reuses the same step registry the automation
	// scheduler uses so Logic step executors (function / query /
	// mutation / forEach / parallel / switch) get the same
	// integration plug-ins, SI providers, and caching behaviour.
	// When this binary doesn't include the automations package
	// the engine keeps its "no LogicRunner wired" error path and
	// single-step Logic dispatch continues to work unchanged.
	a.engine.SetLogicRunner(automations.NewLogicRunner(a.engine, a.stepRegistry, a.Logger))

	a.Dependencies = append(a.Dependencies, a.engine)
	a.Dependencies = append(a.Dependencies, a.automationScheduler)

	// Stand up the owner-scoped authored-construct runtime (epic memql#954,
	// #961 / #959 live glue): registry + per-owner scheduler (run hook -> the
	// live Executor) + per-automation breaker (onTrip -> Deactivate +
	// registry-pause) + the cluster-wide global kill switch. The cluster owner
	// gate is wired later in the cluster phase.
	a.wireAuthoredRuntime()

	// Kick off the seed materializer in the background, but only AFTER
	// the engine's Ready channel closes -- the engine's database
	// getter (SetDatabaseGetter) is wired by its PrepareHook during
	// startDependencies, NOT during Init. If the materializer's
	// goroutine fires too early it gets "memory engine database not
	// configured" from every engine.Execute and writes zero rows
	// (which is exactly the bug the agents-empty cockpit was
	// reporting). The startup sweep iterates v1:identity:user to
	// materialize per-user seeds (GA + Planner + Trainer rows for
	// every existing user) and subscribes to user-create events for
	// runtime fanout. Failures stay logged and don't crash boot.
	if sm := a.engine.SeedMaterializer(); sm != nil {
		go func() {
			<-a.engine.Ready()
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
