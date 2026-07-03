package automations

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/znasllc-io/memql/component/bus"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

// Scheduler manages scheduled automation execution.
type Scheduler struct {
	logger       *slog.Logger
	loader       *Loader
	engine       *memql.MemQLEngine
	eventBus     *events.Bus
	stepRegistry StepExecutorRegistry
	cron         *cron.Cron

	// Separate executors for different trigger types:
	// - eventExecutor: has dedup enabled (idempotency for event-triggered runs)
	// - scheduleExecutor: no dedup (scheduled/manual runs should always execute)
	eventExecutor    *Executor
	scheduleExecutor *Executor

	mu          sync.RWMutex
	automations map[string]*Automation
	entryIds    map[string]cron.EntryID
	eventUnsubs []func() // Event subscription cleanup functions
	running     bool
	cancel      context.CancelFunc
	done        chan struct{}
	readyCh     chan struct{}
	wiring      *bus.Wiring

	// leaderGate, when set, gates SCHEDULED (cron) automations: a firing
	// only executes when it returns true (#561). It elects one cluster-wide
	// runner so crons don't fire once-per-node-type / once-per-replica.
	// nil = ungated (every node runs crons -- the pre-#561 behaviour, and
	// the dev/single-node default).
	leaderGate func() bool
}

// SchedulerOptions configures the automation scheduler.
type SchedulerOptions struct {
	Logger       *slog.Logger
	Loader       *Loader
	Engine       *memql.MemQLEngine
	EventBus     *events.Bus
	StepRegistry StepExecutorRegistry

	// Step-level caching options
	StepCacheEnabled    bool
	StepCacheMaxBytes   int64
	StepCacheDefaultTTL time.Duration

	// LeaderGate, when set, restricts SCHEDULED (cron) automation execution
	// to the one cluster-wide leader node (#561). nil = every node runs
	// crons (pre-#561 / single-node default). Typically CronLeader.IsLeader.
	LeaderGate func() bool

	// ClusterGuard, when set, makes EVENT-triggered automations exactly-once
	// across replicas (#561) -- the event executor claims each (automation,
	// event) in the DB so only one replica runs it. nil = single-replica
	// behaviour. Typically a *ClusterExecutionGuard.
	ClusterGuard executionClaimer
}

// NewScheduler creates a new automation scheduler.
// If opts.Logger is nil, a new logger is created using common.NewLogger with the component's
// configured color and level from environment variables.
func NewScheduler(opts SchedulerOptions) (*Scheduler, error) {
	if opts.Loader == nil {
		return nil, fmt.Errorf("automation loader is required")
	}
	if opts.Engine == nil {
		return nil, fmt.Errorf("memory engine is required")
	}
	if opts.StepRegistry == nil {
		return nil, fmt.Errorf("step registry is required")
	}

	// Create logger via common.NewLogger if not provided
	logger := opts.Logger
	if logger == nil {
		logger = NewLogger()
	}

	s := &Scheduler{
		logger:       logger,
		loader:       opts.Loader,
		engine:       opts.Engine,
		eventBus:     opts.EventBus,
		stepRegistry: opts.StepRegistry,
		automations:  make(map[string]*Automation),
		entryIds:     make(map[string]cron.EntryID),
		eventUnsubs:  make([]func(), 0),
		readyCh:      make(chan struct{}),
		leaderGate:   opts.LeaderGate,
	}

	// Create executor for event-triggered automations
	// Chain tracking + dedup enabled for idempotent event handling
	// Concurrency limited to 10 to prevent database connection exhaustion during event storms
	s.eventExecutor = NewExecutor(ExecutorOptions{
		Logger:                  logger,
		Engine:                  opts.Engine,
		EventBus:                opts.EventBus,
		StepRegistry:            opts.StepRegistry,
		AutomationTrigger:       s,
		ChainTrackingEnabled:    true,
		DedupEnabled:            true,              // Idempotency for event-triggered runs
		ClusterGuard:            opts.ClusterGuard, // cross-replica idempotency (#561)
		StepCacheEnabled:        opts.StepCacheEnabled,
		StepCacheMaxBytes:       opts.StepCacheMaxBytes,
		StepCacheDefaultTTL:     opts.StepCacheDefaultTTL,
		MaxConcurrentExecutions: 10, // Limit concurrent event-triggered automations
	})

	// Create executor for scheduled/manual automations
	// Chain tracking enabled (for verification), but NO dedup
	// (scheduled runs with same config should always execute)
	// Concurrency limited to 5 (fewer than events since scheduled runs are less bursty)
	s.scheduleExecutor = NewExecutor(ExecutorOptions{
		Logger:                  logger,
		Engine:                  opts.Engine,
		EventBus:                opts.EventBus,
		StepRegistry:            opts.StepRegistry,
		AutomationTrigger:       s,
		ChainTrackingEnabled:    true,
		DedupEnabled:            false, // Scheduled/manual runs always execute
		StepCacheEnabled:        opts.StepCacheEnabled,
		StepCacheMaxBytes:       opts.StepCacheMaxBytes,
		StepCacheDefaultTTL:     opts.StepCacheDefaultTTL,
		MaxConcurrentExecutions: 5, // Limit concurrent scheduled automations
	})

	// Log cache configuration
	if opts.StepCacheEnabled {
		effectiveMaxBytes := opts.StepCacheMaxBytes
		if effectiveMaxBytes == 0 {
			effectiveMaxBytes = 256 * 1024 * 1024 // Default from step_cache.go
		}
		effectiveTTL := opts.StepCacheDefaultTTL
		if effectiveTTL == 0 {
			effectiveTTL = 5 * time.Minute // Default from executor.go
		}
		logger.Info("step cache enabled",
			"component", ComponentName,
			"maxBytes", effectiveMaxBytes,
			"defaultTTL", effectiveTTL,
		)
	}

	return s, nil
}

// ComponentName returns the component identifier.
func (s *Scheduler) ComponentName() common.ComponentName {
	return ComponentName
}

// Order returns the startup order (after engine).
func (s *Scheduler) Order() int {
	return 150 // After engine (100)
}

// Ready returns a channel that is closed when the scheduler is ready.
func (s *Scheduler) Ready() <-chan struct{} {
	return s.readyCh
}

// IsRunning reports whether the scheduler is active.
func (s *Scheduler) IsRunning() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.running
}

// Start begins the scheduler.
func (s *Scheduler) Start(ctx context.Context) {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		s.logWarn("scheduler already running")
		return
	}

	runCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.done = make(chan struct{})
	s.running = true
	s.mu.Unlock()

	select {
	case <-s.readyCh:
	default:
		close(s.readyCh)
	}

	go s.run(runCtx)
}

// Stop halts the scheduler.
func (s *Scheduler) Stop(ctx context.Context) {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return
	}

	cancel := s.cancel
	done := s.done
	s.mu.Unlock()

	s.logInfo("stopping scheduler")

	if cancel != nil {
		cancel()
	}

	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			s.logWarn("scheduler stop timed out")
		}
	}
}

// TriggerAutomation manually triggers an automation by name.
func (s *Scheduler) TriggerAutomation(ctx context.Context, name string) (*AutomationExecution, error) {
	s.mu.RLock()
	automation, ok := s.automations[name]
	s.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("automation %q not found", name)
	}

	// Manual triggers use scheduleExecutor (no dedup)
	return s.scheduleExecutor.Execute(ctx, automation, "manual")
}

// TriggerAutomationWithArgs runs a sub-automation by name with a
// synthetic event whose payload carries the supplied args. The
// sub-automation's evaluator sees the args under `event.X`
// (because `event` translates to `ctx.input` and the executor
// stamps the event payload as the input envelope).
//
// Used by the automation-within-automation step kind: when a
// parent automation has `step welcome { automation seedX { ... } }`,
// the AutomationExecutor calls this method so the sub-automation
// receives the call's args as its input.
func (s *Scheduler) TriggerAutomationWithArgs(ctx context.Context, name string, args map[string]any) (*AutomationExecution, error) {
	s.mu.RLock()
	automation, ok := s.automations[name]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("automation %q not found", name)
	}
	// Build a synthetic event from the args. The topic uses a
	// "automation.invocation" prefix so observers can distinguish
	// procedural calls from real bus events; the payload IS the
	// args map.
	syntheticEvent := &events.Event{
		Topic:   "automation.invocation." + name,
		Kind:    events.KindUnspecified,
		Payload: args,
	}
	return s.eventExecutor.ExecuteWithEvent(ctx, automation, "sub-automation", syntheticEvent)
}

// TriggerAutomationWithEvent triggers an automation with a synthetic event.
// This allows HTTP endpoints to invoke event-triggered automations (like bootstrapUser)
// with the same event data structure as if the event had been published via the event bus.
func (s *Scheduler) TriggerAutomationWithEvent(ctx context.Context, name string, event *events.Event) (*AutomationExecution, error) {
	s.mu.RLock()
	automation, ok := s.automations[name]
	s.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("automation %q not found", name)
	}

	// Event-based triggers use eventExecutor (with dedup for idempotency)
	return s.eventExecutor.ExecuteWithEvent(ctx, automation, "http-trigger", event)
}

// GetAutomations returns all loaded automations.
func (s *Scheduler) GetAutomations() []*Automation {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]*Automation, 0, len(s.automations))
	for _, a := range s.automations {
		result = append(result, a)
	}
	return result
}

// ResumeAutomation resumes a failed automation from a checkpoint.
// It loads the checkpoint by executionId, validates it, and resumes execution
// from the specified step (or the failed step if fromStep is empty).
func (s *Scheduler) ResumeAutomation(ctx context.Context, executionId string, fromStep string) (*AutomationExecution, error) {
	// Load the checkpoint
	checkpoint, err := LoadCheckpoint(ctx, s.engine, executionId)
	if err != nil {
		return nil, err
	}

	// Load the automation by name
	s.mu.RLock()
	automation, ok := s.automations[checkpoint.AutomationName]
	s.mu.RUnlock()

	if !ok {
		// Try loading from disk in case it was added after scheduler started
		automation, err = s.loader.LoadByName(checkpoint.AutomationName)
		if err != nil {
			return nil, fmt.Errorf("automation %q not found: %w", checkpoint.AutomationName, err)
		}
	}

	// Resume using the schedule executor (no dedup for manual resume)
	opts := &ResumeOptions{
		FromStep: fromStep,
	}
	return s.scheduleExecutor.ResumeFrom(ctx, checkpoint, automation, opts)
}

func (s *Scheduler) run(ctx context.Context) {
	defer func() {
		s.mu.Lock()
		s.running = false
		s.cancel = nil
		close(s.done)
		s.done = nil
		s.mu.Unlock()
	}()

	s.logInfo("starting scheduler")

	// Load all automations
	automations, err := s.loader.LoadAll()
	if err != nil {
		s.logError("failed to load automations", "error", err)
		return
	}

	// Create cron with seconds support
	s.cron = cron.New(cron.WithSeconds())

	// Register all enabled scheduled and event-triggered automations
	scheduled := 0
	eventTriggered := 0
	for _, automation := range automations {
		if !automation.IsEnabled() {
			continue
		}

		s.mu.Lock()
		s.automations[automation.Name] = automation
		s.mu.Unlock()

		if automation.IsScheduled() {
			if err := s.scheduleAutomation(automation); err != nil {
				s.logError("failed to schedule automation", "automation", automation.Name, "error", err)
				continue
			}
			s.logInfo("automation scheduled", "automation", automation.Name, "schedule", automation.Schedule)
			scheduled++
		}

		if automation.IsEventTriggered() {
			if err := s.subscribeToEventTrigger(automation); err != nil {
				s.logError("failed to subscribe to event trigger", "automation", automation.Name, "error", err)
				continue
			}
			s.logInfo("automation event trigger registered", "automation", automation.Name, "event", automation.Trigger.Event)
			eventTriggered++
		}
	}

	// Start the cron scheduler
	s.cron.Start()

	s.logInfo("scheduler started", "automationCount", len(automations), "scheduledCount", scheduled, "eventTriggeredCount", eventTriggered)

	// Wait for shutdown
	<-ctx.Done()

	// Gracefully stop cron
	s.logInfo("stopping cron scheduler")
	cronCtx := s.cron.Stop()
	<-cronCtx.Done()

	// Unsubscribe from all event triggers
	s.mu.Lock()
	for _, unsub := range s.eventUnsubs {
		unsub()
	}
	s.eventUnsubs = nil
	s.mu.Unlock()

	// Close executors to release resources (dedup cleanup goroutines)
	if s.eventExecutor != nil {
		s.eventExecutor.Close()
	}
	if s.scheduleExecutor != nil {
		s.scheduleExecutor.Close()
	}

	s.logInfo("scheduler stopped")
}

// scheduleLeaderOK reports whether this node may execute a SCHEDULED
// automation firing right now: true when no leader gate is configured
// (single-node / dev) or when this node is the elected cron leader (#561).
func (s *Scheduler) scheduleLeaderOK() bool {
	return s.leaderGate == nil || s.leaderGate()
}

func (s *Scheduler) scheduleAutomation(automation *Automation) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Capture automation in closure
	a := automation
	entryId, err := s.cron.AddFunc(automation.Schedule, func() {
		// #561: only the elected cron leader runs scheduled automations, so
		// they fire once cluster-wide instead of once per node-type/replica.
		// Event-triggered automations are unaffected (each event reaches one
		// pod via the mesh). nil gate = ungated (single-node/dev).
		if !s.scheduleLeaderOK() {
			s.logDebug("skipping scheduled automation -- not the cron leader",
				"automation", a.Name, "schedule", a.Schedule)
			return
		}
		s.executeAutomation(a, "schedule", nil)
	})
	if err != nil {
		return fmt.Errorf("invalid cron schedule %q: %w", automation.Schedule, err)
	}

	s.entryIds[automation.Name] = entryId
	return nil
}

func (s *Scheduler) subscribeToEventTrigger(automation *Automation) error {
	if s.eventBus == nil {
		return fmt.Errorf("event bus not configured")
	}

	if automation.Trigger == nil || automation.Trigger.Event == "" {
		return fmt.Errorf("event trigger pattern is required")
	}

	// Capture automation in closure
	a := automation
	pattern := automation.Trigger.Event

	unsub := s.eventBus.Subscribe(pattern, func(event events.Event) {
		// Fire-time args validation (event-payload-binding ADR Decision 2,
		// memql#2363). When the automation declares an `args { }` contract,
		// bind + validate event.payload BEFORE the @filter and before any
		// step. A violation refuses the fire loudly (Warn + skip counter) and
		// never reaches the filter or the executor. Automations without an
		// args block bind nothing here -- behaviour is unchanged.
		bound, extras, argErr := bindEventArgs(a, &event)
		if argErr != nil {
			refuseFireForArgs(s.logger, a.Name, event.Topic, argErr)
			return
		}
		if len(extras) > 0 {
			s.logDebug("event trigger: undeclared payload fields ignored (tolerant-reader)",
				"automation", a.Name,
				"topic", event.Topic,
				"extraFields", extras,
			)
		}

		// Optionally evaluate filter condition
		if a.Trigger.Filter != "" {
			evaluator := NewEvaluator()
			// Shared envelope builder so @filter sees the same event shape
			// as step bodies (incl. event.actor / event.timestamp, G4).
			eventMap := buildEventEnvelope(&event, "", "")
			evaluator.SetCustom("event", eventMap)
			// The @filter evaluates with the validated `args` binding
			// available (ADR Decision 2) alongside the existing event root;
			// validation has already run above.
			if bound != nil {
				evaluator.SetCustom("args", bound)
				// G2 (memql#2364): see executor.go -- declared set enables
				// bare nil-resolution for absent optional fields in @filter.
				evaluator.SetCustom("argsDeclared", declaredArgsSet(a))
			}
			evaluator.SetCustom("ctx", map[string]any{
				"input":  eventMap,
				"output": nil,
				"error":  "",
			})

			shouldRun, err := evaluator.EvaluateCondition(a.Trigger.Filter)
			if err != nil {
				s.logWarn("event trigger filter evaluation failed",
					"automation", a.Name,
					"filter", a.Trigger.Filter,
					"error", err,
				)
				return
			}
			if !shouldRun {
				s.logDebug("event trigger filter not satisfied",
					"automation", a.Name,
					"topic", event.Topic,
				)
				return
			}
		}

		s.logInfo("event trigger fired",
			"automation", a.Name,
			"topic", event.Topic,
		)

		s.executeAutomation(a, "event:"+event.Topic, &event)
	}, events.WithSubscriberName("automation:"+a.Name))

	s.mu.Lock()
	s.eventUnsubs = append(s.eventUnsubs, unsub)
	s.mu.Unlock()

	return nil
}

func (s *Scheduler) executeAutomation(automation *Automation, triggeredBy string, event *events.Event) {
	ctx := context.Background()

	var execution *AutomationExecution
	var err error

	// Choose executor based on trigger type:
	// - Event-triggered: use eventExecutor (with dedup for idempotency)
	// - Scheduled/manual: use scheduleExecutor (no dedup)
	if event != nil {
		execution, err = s.eventExecutor.ExecuteWithEvent(ctx, automation, triggeredBy, event)
	} else {
		execution, err = s.scheduleExecutor.Execute(ctx, automation, triggeredBy)
	}

	if err != nil {
		s.logError("automation execution failed",
			"automation", automation.Name,
			"executionId", execution.ID,
			"triggeredBy", triggeredBy,
			"error", err,
		)
		return
	}

	logArgs := []any{
		"automation", automation.Name,
		"executionId", execution.ID,
		"status", execution.Status,
		"duration", formatDurationMs(execution.Duration),
		"stepCount", len(execution.Steps),
	}
	if execution.InitialChainHead != "" {
		logArgs = append(logArgs, "initialChainHead", execution.InitialChainHead[:16]+"...")
	}
	if execution.ChainHead != "" {
		logArgs = append(logArgs, "chainHead", execution.ChainHead[:16]+"...")
	}
	s.logInfo("automation execution completed", logArgs...)
}

// Logging helpers - component is already set by common.NewLogger

func (s *Scheduler) logDebug(msg string, args ...any) {
	if s.logger != nil {
		s.logger.Debug(msg, args...)
	}
}

func (s *Scheduler) logInfo(msg string, args ...any) {
	if s.logger != nil {
		s.logger.Info(msg, args...)
	}
}

func (s *Scheduler) logWarn(msg string, args ...any) {
	if s.logger != nil {
		s.logger.Warn(msg, args...)
	}
}

func (s *Scheduler) logError(msg string, args ...any) {
	if s.logger != nil {
		s.logger.Error(msg, args...)
	}
}

// formatDurationMs returns a human-readable duration string in milliseconds.
// Examples: "1.23ms", "456.78ms"
func formatDurationMs(d time.Duration) string {
	ms := float64(d.Nanoseconds()) / float64(time.Millisecond)
	return fmt.Sprintf("%.3fms", ms)
}
