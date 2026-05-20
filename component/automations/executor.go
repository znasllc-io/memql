package automations

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/provenance"
	"github.com/znasllc-io/memql/core/id"
)

// systemActorPrefix is the prefix for system-generated actor identifiers.
const systemActorPrefix = "system:automation:"

// contextWithSystemActor injects a system actor into the context for automation execution.
// This allows automations to execute mutations without requiring user authentication.
// The actor identifier includes the automation name for audit trail purposes.
func contextWithSystemActor(ctx context.Context, automationName string) context.Context {
	actorId := systemActorPrefix + automationName
	claims := map[string]any{
		"sub":   actorId,
		"email": actorId,
		"role":  "system",
	}
	token := auth.BuildTokenInfo(claims)
	ctx = auth.ContextWithClaims(ctx, claims)
	return auth.ContextWithToken(ctx, token)
}

// Executor orchestrates automation execution.
type Executor struct {
	logger            *slog.Logger
	engine            *memql.MemQLEngine
	eventBus          *events.Bus
	stepRegistry      StepExecutorRegistry
	automationTrigger AutomationTrigger

	// Chain tracking (enabled via ExecutorOptions.ChainTrackingEnabled)
	chainTrackingEnabled bool

	// Dedup tracking (enabled via ExecutorOptions.DedupEnabled)
	// Separate from chain tracking - dedup blocks duplicate executions,
	// chain tracking just provides verification data
	dedupEnabled bool
	dedup        *executionDedup

	// Step-level caching (enabled via ExecutorOptions.StepCacheEnabled)
	stepCacheEnabled    bool
	stepCache           *StepCache
	stepCacheDefaultTTL time.Duration

	// Concurrency control (limits concurrent automation executions)
	concurrencySem chan struct{}

	// Execution tracking for storm detection
	executionTracker *executionTracker
}

// Close releases executor resources (e.g., deduplication cleanup goroutines).
// Safe to call multiple times.
func (e *Executor) Close() {
	if e == nil {
		return
	}
	if e.dedup != nil {
		e.dedup.stop()
	}
}

// StepExecutorRegistry provides step executors.
type StepExecutorRegistry interface {
	Execute(ctx context.Context, step *Step, stepCtx *StepContext) (*StepResult, error)
}

// AutomationTrigger allows steps to invoke other automations.
type AutomationTrigger interface {
	// TriggerAutomation starts an automation by name.
	// Returns the execution result and any error.
	TriggerAutomation(ctx context.Context, name string) (*AutomationExecution, error)

	// TriggerAutomationWithArgs starts an automation by name and
	// passes the supplied args as the sub-automation's input
	// envelope. The args appear under `event.X` from the
	// sub-automation's perspective, the same way an event-triggered
	// invocation would see the event payload. Used by the
	// automation-within-automation step kind for procedural
	// composition.
	TriggerAutomationWithArgs(ctx context.Context, name string, args map[string]any) (*AutomationExecution, error)
}

// StepContext holds the context for step execution.
type StepContext struct {
	Logger            *slog.Logger
	Engine            *memql.MemQLEngine
	EventBus          *events.Bus
	Evaluator         *Evaluator
	Execution         *AutomationExecution
	AutomationTrigger AutomationTrigger
	TriggeringEvent   *events.Event // The event that triggered this automation (if any)

	// PreviousChainHead is the chain state before this step executes.
	// For child steps in forEach/parallel, this links to the parent's chain position
	// (parallel) or the previous sibling's chain head (forEach sequential).
	PreviousChainHead string

	// ChainTrackingEnabled indicates whether chain tracking is active.
	// Propagated from ExecutorOptions to child contexts.
	ChainTrackingEnabled bool

	// SkipCache bypasses step-level caching when true.
	// Used during resume to ensure fresh data is fetched instead of stale cached results.
	SkipCache bool
}

// ExecutorOptions configures the automation executor.
type ExecutorOptions struct {
	Logger            *slog.Logger
	Engine            *memql.MemQLEngine
	EventBus          *events.Bus
	StepRegistry      StepExecutorRegistry
	AutomationTrigger AutomationTrigger

	// ChainTrackingEnabled enables content-addressed chain tracking for executions.
	// When enabled, each step produces a deterministic fingerprint that chains to
	// the previous state, enabling replay verification.
	// This is separate from deduplication - chain tracking provides verification
	// without necessarily blocking duplicate executions.
	ChainTrackingEnabled bool

	// DedupEnabled enables execution deduplication based on initial chain head.
	// When true, executions with the same automation + trigger context + input
	// will be skipped within the DedupTTL window.
	// Should only be enabled for event-triggered automations where idempotency matters.
	// Scheduled and manual runs should NOT use dedup (same schedule = same fingerprint).
	DedupEnabled bool

	// DedupTTL configures how long to remember executions for deduplication.
	// Defaults to 10 minutes if DedupEnabled is true and this is zero.
	DedupTTL time.Duration

	// StepCacheEnabled enables step-level memoization.
	// When true, pure steps (query, shape, function) cache their results.
	StepCacheEnabled bool

	// StepCacheMaxBytes limits cache memory. Defaults to 256MB.
	StepCacheMaxBytes int64

	// StepCacheDefaultTTL is the default TTL when not specified per-step.
	// Defaults to 5 minutes if StepCacheEnabled is true and this is zero.
	StepCacheDefaultTTL time.Duration

	// MaxConcurrentExecutions limits how many automations can run simultaneously.
	// Prevents database connection exhaustion during event storms.
	// Defaults to 10 if not set (0 means use default, not unlimited).
	MaxConcurrentExecutions int
}

// NewExecutor creates a new automation executor.
func NewExecutor(opts ExecutorOptions) *Executor {
	e := &Executor{
		logger:               opts.Logger,
		engine:               opts.Engine,
		eventBus:             opts.EventBus,
		stepRegistry:         opts.StepRegistry,
		automationTrigger:    opts.AutomationTrigger,
		chainTrackingEnabled: opts.ChainTrackingEnabled,
		dedupEnabled:         opts.DedupEnabled,
		stepCacheEnabled:     opts.StepCacheEnabled,
		stepCacheDefaultTTL:  opts.StepCacheDefaultTTL,
	}

	// Initialize dedup tracker if dedup is enabled
	// Note: dedup requires chain tracking to compute fingerprints,
	// but chain tracking can be used without dedup
	if opts.DedupEnabled {
		ttl := opts.DedupTTL
		if ttl == 0 {
			ttl = 10 * time.Minute // default TTL
		}
		e.dedup = newExecutionDedup(ttl)
	}

	// Initialize step cache if enabled
	if opts.StepCacheEnabled {
		maxBytes := opts.StepCacheMaxBytes
		if maxBytes == 0 {
			maxBytes = 256 * 1024 * 1024 // 256MB default
		}
		e.stepCache = NewStepCache(maxBytes)

		if e.stepCacheDefaultTTL == 0 {
			e.stepCacheDefaultTTL = 5 * time.Minute // default TTL
		}
	}

	// Initialize concurrency limiter
	maxConcurrent := opts.MaxConcurrentExecutions
	if maxConcurrent <= 0 {
		maxConcurrent = 10 // Default to 10 concurrent automations
	}
	e.concurrencySem = make(chan struct{}, maxConcurrent)

	// Initialize execution tracker for storm detection
	e.executionTracker = newExecutionTracker(1 * time.Minute)

	return e
}

// Execute runs an automation and returns the execution result.
func (e *Executor) Execute(ctx context.Context, automation *Automation, triggeredBy string) (*AutomationExecution, error) {
	return e.ExecuteWithEvent(ctx, automation, triggeredBy, nil)
}

// ExecuteWithEvent runs an automation with an optional triggering event.
func (e *Executor) ExecuteWithEvent(ctx context.Context, automation *Automation, triggeredBy string, triggeringEvent *events.Event) (*AutomationExecution, error) {
	if automation == nil {
		return nil, fmt.Errorf("automation is nil")
	}

	// Acquire concurrency slot (blocks if limit reached)
	// This prevents database connection exhaustion during event storms
	select {
	case e.concurrencySem <- struct{}{}:
		defer func() { <-e.concurrencySem }() // Release on completion
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	// Track execution count for storm detection
	if e.executionTracker != nil {
		executionCount := e.executionTracker.record(automation.Name)
		if executionCount > 20 && e.logger != nil {
			e.logger.Warn("automation storm detected",
				"component", ComponentName,
				"automation", automation.Name,
				"executionsInWindow", executionCount,
				"window", "1m",
			)
		}
	}

	// Inject system actor for automation execution
	// This allows automations to execute mutations without user authentication
	ctx = contextWithSystemActor(ctx, automation.Name)

	// Stamp Automation provenance on every row this run writes. The
	// engine reads this off ctx and stores it on the row intrinsic
	// (see component/provenance). Trigger = the event topic that
	// fired the automation, or "cron"/"manual" when no event.
	trigger := "manual"
	if triggeringEvent != nil {
		trigger = triggeringEvent.Topic
	} else if triggeredBy != "" {
		trigger = triggeredBy
	}
	ctx = provenance.ContextWithProvenance(ctx, provenance.Automation(automation.Name, trigger))

	exec := NewExecution(automation.Name, triggeredBy)
	evaluator := NewEvaluator()

	// Wire variable resolver for $var.X expressions
	evaluator.SetVariableResolver(e.createVariableResolver())
	evaluator.SetSystemVariableResolver(e.createSystemVariableResolver())
	evaluator.SetSecretResolver(e.createSecretResolver())
	evaluator.SetSystemSecretResolver(e.createSystemSecretResolver())
	evaluator.SetCanonicalIdResolver(e.createCanonicalIdResolver())
	evaluator.SetLogger(e.logger)

	// Make timestamp available to the evaluator
	evaluator.SetCustom("timestamp", time.Now().UTC().Format(time.RFC3339))

	// Make triggering event data available to the evaluator under
	// the legacy `event` global AND the canonical ctx envelope. The
	// two share the same backing map — body authors should write the
	// `ctx.input.<...>` form going forward; `event.<...>` is kept
	// for transition and parse-equivalent through Phase B/D.
	if triggeringEvent != nil {
		eventMap := map[string]any{
			"topic":   triggeringEvent.Topic,
			"kind":    triggeringEvent.Kind.String(),
			"payload": triggeringEvent.Payload,
		}
		evaluator.SetCustom("event", eventMap)
		evaluator.SetCustom("ctx", map[string]any{
			"input":  eventMap,
			"output": nil,
			"error":  "",
		})
	} else {
		// Cron / startup-triggered automations don't have a trigger
		// event; their ctx.input is an empty object. We still expose
		// ctx so `ctx.output = ...` / `ctx.error` reads in cron bodies
		// have a place to land.
		evaluator.SetCustom("ctx", map[string]any{
			"input":  map[string]any{},
			"output": nil,
			"error":  "",
		})
	}

	if e.logger != nil {
		e.logger.Info("starting automation execution",
			"component", ComponentName,
			"automation", automation.Name,
			"executionId", exec.ID,
			"triggeredBy", triggeredBy,
		)
	}

	// Publish automation started event
	e.publishEvent(events.TopicAutomationStarted, events.KindAutomationStarted, map[string]any{
		"automationName": automation.Name,
		"executionId":    exec.ID,
		"triggeredBy":    triggeredBy,
	})

	// Execute input query if defined
	if automation.Input != nil && automation.Input.Query != "" {
		inputResult, err := e.executeInput(ctx, automation.Input)
		if err != nil {
			exec.Fail(fmt.Errorf("input query failed: %w", err))
			e.handleAutomationError(ctx, automation, exec, triggeringEvent, err)
			return exec, err
		}
		exec.Input = inputResult
		evaluator.SetInput(inputResult)
	}

	// Chain tracking initialization (when enabled)
	var chainHead string
	if e.chainTrackingEnabled {
		// Initialize step order tracking
		exec.StepOrder = make([]string, 0, len(automation.Steps))

		// Build event data for fingerprinting
		var eventData map[string]any
		if triggeringEvent != nil {
			eventData = map[string]any{
				"topic":   triggeringEvent.Topic,
				"kind":    triggeringEvent.Kind.String(),
				"payload": triggeringEvent.Payload,
			}
		}

		// Fingerprint input if present
		if exec.Input != nil {
			exec.InputFingerprint = FingerprintInput(exec.Input)
		}

		// Compute initial chain head
		exec.InitialChainHead = ComputeInitialChainHead(
			automation.Name,
			triggeredBy,
			eventData,
			exec.InputFingerprint,
		)
		chainHead = exec.InitialChainHead

		// Check for duplicate execution (only when dedup is explicitly enabled)
		// Dedup should only be used for event-triggered automations where
		// idempotency matters. Scheduled/manual runs should NOT use dedup.
		if e.dedupEnabled && e.dedup != nil && e.dedup.isDuplicate(automation.Name, exec.InitialChainHead) {
			if e.logger != nil {
				e.logger.Info("skipping duplicate execution",
					"component", ComponentName,
					"automation", automation.Name,
					"initialChainHead", exec.InitialChainHead,
				)
			}
			exec.Status = "skipped"
			exec.Error = "duplicate execution detected"
			exec.CompletedAt = time.Now()
			exec.Duration = exec.CompletedAt.Sub(exec.StartedAt)
			return exec, nil
		}
	}

	// Execute steps
	stepCtx := &StepContext{
		Logger:               e.logger,
		Engine:               e.engine,
		EventBus:             e.eventBus,
		Evaluator:            evaluator,
		Execution:            exec,
		AutomationTrigger:    e.automationTrigger,
		TriggeringEvent:      triggeringEvent,
		ChainTrackingEnabled: e.chainTrackingEnabled,
	}

	for stepIndex, step := range automation.Steps {
		// Track step order for chain verification
		if e.chainTrackingEnabled {
			exec.StepOrder = append(exec.StepOrder, step.ID)
		}

		// Check for cancellation
		select {
		case <-ctx.Done():
			exec.Cancel()
			return exec, ctx.Err()
		default:
		}

		// Evaluate step condition if present
		if step.Condition != "" {
			if e.logger != nil {
				e.logger.Debug("evaluating step condition",
					"step", step.ID,
					"condition", step.Condition,
				)
			}
			shouldRun, err := evaluator.EvaluateCondition(step.Condition)
			if err != nil {
				if e.logger != nil {
					e.logger.Warn("step condition evaluation failed",
						"step", step.ID,
						"condition", step.Condition,
						"error", err,
					)
				}
				shouldRun = false
			}
			if e.logger != nil {
				e.logger.Debug("step condition evaluated",
					"step", step.ID,
					"condition", step.Condition,
					"result", shouldRun,
				)
			}
			if !shouldRun {
				// Record skipped step
				skipResult := &StepResult{
					StepId:      step.ID,
					Status:      "skipped",
					StartedAt:   time.Now(),
					CompletedAt: time.Now(),
				}
				if e.logger != nil {
					e.logger.Debug("step skipped due to condition",
						"step", step.ID,
						"condition", step.Condition,
					)
				}
				exec.AddStepResult(skipResult)
				// Make skipped steps visible to later step conditions ($steps.<id>.status).
				evaluator.SetStepResult(step.ID, skipResult)
				continue
			}
		}

		// Set current chain head in context for child steps (forEach/parallel)
		if e.chainTrackingEnabled {
			stepCtx.PreviousChainHead = chainHead
		}

		// Execute the step
		result, err := e.executeStep(ctx, step, stepCtx)
		if result != nil {
			// Compute chain linkage if tracking enabled
			if e.chainTrackingEnabled {
				result.PreviousChainHead = chainHead
				result.ContentId = StepDeterministicFingerprint(step, result)
				// Advance chain head
				chainHead = string(fingerprintEngine.Combine(
					id.ID(chainHead),
					id.ID(result.ContentId),
				))
			}

			exec.AddStepResult(result)
			evaluator.SetStepResult(step.ID, result)
		}

		if err != nil {
			// Handle error based on strategy
			switch step.OnError {
			case ErrorStrategyContinue:
				if e.logger != nil {
					e.logger.Warn("step failed, continuing",
						"component", ComponentName,
						"step", step.ID,
						"error", err,
					)
				}
				continue
			case ErrorStrategyRetry:
				// Retry logic
				retried := false
				for attempt := 1; attempt <= step.RetryCount && !retried; attempt++ {
					if e.logger != nil {
						e.logger.Info("retrying step",
							"component", ComponentName,
							"step", step.ID,
							"attempt", attempt,
						)
					}
					result, err = e.executeStep(ctx, step, stepCtx)
					if err == nil {
						// Compute chain linkage if tracking enabled
						if e.chainTrackingEnabled && result != nil {
							result.PreviousChainHead = chainHead
							result.ContentId = StepDeterministicFingerprint(step, result)
							chainHead = string(fingerprintEngine.Combine(
								id.ID(chainHead),
								id.ID(result.ContentId),
							))
						}
						exec.AddStepResult(result)
						evaluator.SetStepResult(step.ID, result)
						retried = true
					}
				}
				if !retried {
					exec.Fail(err)
					e.saveCheckpointOnFailure(ctx, automation, exec, step, stepIndex, chainHead, err, triggeringEvent)
					e.handleAutomationError(ctx, automation, exec, triggeringEvent, err)
					return exec, err
				}
			default: // ErrorStrategyStop
				exec.Fail(err)
				e.saveCheckpointOnFailure(ctx, automation, exec, step, stepIndex, chainHead, err, triggeringEvent)
				e.handleAutomationError(ctx, automation, exec, triggeringEvent, err)
				return exec, err
			}
		}
	}

	// Execute onComplete hook if defined
	if automation.OnComplete != nil {
		_, err := e.executeStep(ctx, automation.OnComplete, stepCtx)
		if err != nil && e.logger != nil {
			e.logger.Warn("onComplete hook failed",
				"component", ComponentName,
				"error", err,
			)
		}
	}

	exec.Complete()

	// Finalize chain tracking
	if e.chainTrackingEnabled {
		exec.ChainHead = chainHead

		// Register execution for dedup
		if e.dedup != nil {
			e.dedup.register(automation.Name, exec.InitialChainHead, exec.ID)
		}
	}

	// Publish automation completed event
	completedPayload := map[string]any{
		"automationName": automation.Name,
		"executionId":    exec.ID,
		"duration":       exec.Duration.Milliseconds(),
		"stepCount":      len(exec.Steps),
	}
	if e.chainTrackingEnabled && exec.ChainHead != "" {
		completedPayload["chainHead"] = exec.ChainHead
	}
	e.publishEvent(events.TopicAutomationCompleted, events.KindAutomationCompleted, completedPayload)

	return exec, nil
}

// executeInput runs the input query.
func (e *Executor) executeInput(ctx context.Context, input *AutomationInput) (any, error) {
	if e.engine == nil {
		return nil, fmt.Errorf("MemQL engine not configured")
	}

	query := input.Query
	if input.Limit > 0 {
		query = fmt.Sprintf("paginate(%s, %d)", query, input.Limit)
	}

	result, err := e.engine.Execute(ctx, query)
	if err != nil {
		return nil, err
	}

	return result, nil
}

// executeStep runs a single step using the registry.
func (e *Executor) executeStep(ctx context.Context, step *Step, stepCtx *StepContext) (*StepResult, error) {
	if e.stepRegistry == nil {
		return nil, fmt.Errorf("step registry not configured")
	}

	// Compute cache key if caching enabled for this step
	// Skip cache entirely when SkipCache is set (e.g., during resume to ensure fresh data)
	var cacheKey id.ID
	var cacheEnabled bool
	if e.stepCacheEnabled && e.stepCache != nil && step.IsCacheable() && !stepCtx.SkipCache {
		cacheEnabled = true
		actor := auth.ActorFromContext(ctx)
		scopeId := e.stepCache.Engine().FromString(actor)
		stepDefId := step.DefinitionId(e.stepCache.Engine())
		inputId := FingerprintStepInput(step, stepCtx.Evaluator)
		cacheKey = e.stepCache.Key(scopeId, stepDefId, inputId)
	}

	// Publish step started event (always, even for cache hits)
	e.publishEvent(events.TopicAutomationStepStarted, events.KindAutomationStepStarted, map[string]any{
		"automationName": stepCtx.Execution.AutomationName,
		"executionId":    stepCtx.Execution.ID,
		"stepId":         step.ID,
		"stepType":       string(step.Type),
	})

	// Check cache for existing result
	if cacheEnabled {
		if cached, ok := e.stepCache.Get(cacheKey); ok {
			// Update timing to reflect this execution (instant cache hit)
			now := time.Now()
			cached.StartedAt = now
			cached.CompletedAt = now
			cached.Duration = 0

			if e.logger != nil {
				e.logger.Debug("step cache hit",
					"component", ComponentName,
					"step", step.ID,
					"cacheKey", string(cacheKey),
				)
			}

			// Publish completed event for cache hit
			e.publishEvent(events.TopicAutomationStepCompleted, events.KindAutomationStepCompleted, map[string]any{
				"automationName": stepCtx.Execution.AutomationName,
				"executionId":    stepCtx.Execution.ID,
				"stepId":         step.ID,
				"stepType":       string(step.Type),
				"duration":       int64(0),
				"cached":         true,
			})

			return cached, nil
		}
	}

	// Execute the step
	result, err := e.stepRegistry.Execute(ctx, step, stepCtx)

	// Publish step completed/failed event
	var stepTopic string
	var stepKind events.Kind
	if err != nil {
		stepTopic = events.TopicAutomationStepFailed
		stepKind = events.KindAutomationStepFailed
	} else {
		stepTopic = events.TopicAutomationStepCompleted
		stepKind = events.KindAutomationStepCompleted
	}
	e.publishEvent(stepTopic, stepKind, map[string]any{
		"automationName": stepCtx.Execution.AutomationName,
		"executionId":    stepCtx.Execution.ID,
		"stepId":         step.ID,
		"stepType":       string(step.Type),
		"duration":       result.Duration.Milliseconds(),
	})

	// Cache successful result
	if cacheEnabled && err == nil {
		ttl := step.CacheTTL()
		if ttl == 0 {
			ttl = e.stepCacheDefaultTTL
		}
		e.stepCache.Put(cacheKey, result, ttl)

		if e.logger != nil {
			e.logger.Debug("step result cached",
				"component", ComponentName,
				"step", step.ID,
				"cacheKey", string(cacheKey),
				"ttl", ttl.String(),
			)
		}
	}

	return result, err
}

// saveCheckpointOnFailure persists a checkpoint when an automation fails.
// This enables resuming the automation from the failed step later.
func (e *Executor) saveCheckpointOnFailure(
	ctx context.Context,
	automation *Automation,
	exec *AutomationExecution,
	failedStep *Step,
	stepIndex int,
	chainHead string,
	stepErr error,
	triggeringEvent *events.Event,
) {
	if e.engine == nil {
		return
	}

	checkpoint := &ExecutionCheckpoint{
		ExecutionId:           exec.ID,
		AutomationName:        automation.Name,
		AutomationFingerprint: automation.DefinitionFingerprint(fingerprintEngine),
		StepIndex:             stepIndex,
		ChainHead:             chainHead,
		InitialChainHead:      exec.InitialChainHead,
		StepResults:           ToMinimalStepResults(exec.Steps),
		StepOrder:             exec.StepOrder, // Use tracked order, not map iteration
		FailedAt: &StepFailure{
			StepId:    failedStep.ID,
			Error:     stepErr.Error(),
			Timestamp: time.Now().UTC(),
		},
		Input:            exec.Input,
		InputFingerprint: exec.InputFingerprint,
	}

	// Capture trigger context for event-driven automations
	if triggeringEvent != nil {
		checkpoint.TriggerContext = &TriggerContext{
			TriggeredBy: exec.TriggeredBy,
			Event: map[string]any{
				"topic":   triggeringEvent.Topic,
				"kind":    triggeringEvent.Kind.String(),
				"payload": triggeringEvent.Payload,
			},
		}
	}

	if err := SaveCheckpoint(ctx, e.engine, checkpoint); err != nil {
		if e.logger != nil {
			e.logger.Warn("failed to save checkpoint",
				"component", ComponentName,
				"automation", automation.Name,
				"executionId", exec.ID,
				"error", err,
			)
		}
		return
	}

	if e.logger != nil {
		e.logger.Info("checkpoint saved for failed automation",
			"component", ComponentName,
			"automation", automation.Name,
			"executionId", exec.ID,
			"failedStep", failedStep.ID,
			"stepIndex", stepIndex,
		)
	}
}

// handleAutomationError runs the onError hook and publishes failure event.
func (e *Executor) handleAutomationError(ctx context.Context, automation *Automation, exec *AutomationExecution, triggeringEvent *events.Event, err error) {
	// Execute onError hook if defined
	if automation.OnError != nil {
		evaluator := NewEvaluator()
		evaluator.SetCustom("error", err.Error())
		evaluator.SetCustom("timestamp", time.Now().UTC().Format(time.RFC3339))
		if triggeringEvent != nil {
			eventMap := map[string]any{
				"topic":   triggeringEvent.Topic,
				"kind":    triggeringEvent.Kind.String(),
				"payload": triggeringEvent.Payload,
			}
			evaluator.SetCustom("event", eventMap)
			evaluator.SetCustom("ctx", map[string]any{
				"input":  eventMap,
				"output": nil,
				"error":  err.Error(),
			})
		} else {
			evaluator.SetCustom("ctx", map[string]any{
				"input":  map[string]any{},
				"output": nil,
				"error":  err.Error(),
			})
		}
		evaluator.SetVariableResolver(e.createVariableResolver())
		evaluator.SetSystemVariableResolver(e.createSystemVariableResolver())
		evaluator.SetSecretResolver(e.createSecretResolver())
		evaluator.SetSystemSecretResolver(e.createSystemSecretResolver())
		evaluator.SetCanonicalIdResolver(e.createCanonicalIdResolver())
		evaluator.SetLogger(e.logger)

		stepCtx := &StepContext{
			Logger:    e.logger,
			Engine:    e.engine,
			EventBus:  e.eventBus,
			Evaluator: evaluator,
			Execution: exec,
		}

		_, hookErr := e.executeStep(ctx, automation.OnError, stepCtx)
		if hookErr != nil && e.logger != nil {
			e.logger.Warn("onError hook failed",
				"component", ComponentName,
				"error", hookErr,
			)
		}
	}

	if e.logger != nil {
		e.logger.Error("automation execution failed",
			"component", ComponentName,
			"automation", automation.Name,
			"executionId", exec.ID,
			"error", err,
		)
	}

	// Publish automation failed event
	e.publishEvent(events.TopicAutomationFailed, events.KindAutomationFailed, map[string]any{
		"automationName": automation.Name,
		"executionId":    exec.ID,
		"error":          err.Error(),
		"duration":       exec.Duration.Milliseconds(),
	})
}

// publishEvent publishes an automation event to the event bus.
func (e *Executor) publishEvent(topic string, kind events.Kind, payload map[string]any) {
	if e.eventBus == nil {
		return
	}
	event := events.NewEvent(topic, kind, payload)
	e.eventBus.Publish(event)
}

// navigatePath navigates a dot-separated path in a value.
func navigatePath(value any, path string) any {
	if path == "" || value == nil {
		return value
	}

	parts := strings.Split(path, ".")
	current := value

	for _, part := range parts {
		switch v := current.(type) {
		case map[string]any:
			current = v[part]
		default:
			return nil
		}
	}

	return current
}

// createVariableResolver returns a resolver that delegates to
// engine.ResolveVariable (v1:platform:partitionVariable with fallback to
// v1:platform:globalVariable).
func (e *Executor) createVariableResolver() VariableResolver {
	return func(ctx context.Context, name string) (string, error) {
		if e.engine == nil {
			return "", fmt.Errorf("MemQL engine not configured")
		}
		return e.engine.ResolveVariable(ctx, name)
	}
}

// createSystemVariableResolver returns a resolver for $systemVar.X
// expressions (v1:platform:globalVariable, global plaintext).
func (e *Executor) createSystemVariableResolver() VariableResolver {
	return func(ctx context.Context, name string) (string, error) {
		if e.engine == nil {
			return "", fmt.Errorf("MemQL engine not configured")
		}
		return e.engine.ResolveSystemVariable(ctx, name)
	}
}

// createSecretResolver returns a resolver for $secret.X expressions
// (v1:platform:partitionSecret with fallback to v1:platform:globalSecret, decrypted
// under MEMQL_MASTER_KEY).
func (e *Executor) createSecretResolver() VariableResolver {
	return func(ctx context.Context, name string) (string, error) {
		if e.engine == nil {
			return "", fmt.Errorf("MemQL engine not configured")
		}
		return e.engine.ResolveSecret(ctx, name)
	}
}

// createSystemSecretResolver returns a resolver for $systemSecret.X
// expressions (v1:platform:globalSecret, global encrypted).
func (e *Executor) createSystemSecretResolver() VariableResolver {
	return func(ctx context.Context, name string) (string, error) {
		if e.engine == nil {
			return "", fmt.Errorf("MemQL engine not configured")
		}
		return e.engine.ResolveSystemSecret(ctx, name)
	}
}

// createCanonicalIdResolver returns a resolver for canonicalId(value,
// "<conceptType>") expressions in automation step bodies. Delegates
// to the engine's id-canonicalization helper, which reads the
// concept's @scope to pick the right partition prefix.
//
// Required wiring: without this, automation-derived ids (like
// autoJoinSI's `concat("ga-", hash(canonicalId(ctx.actor,
// "v1:identity:user")))`) would fall back to identity-mapping the
// value, producing different hashes than the mutation-side path
// when the input is a bare slug -- defeating the whole point of
// canonicalId.
func (e *Executor) createCanonicalIdResolver() CanonicalIdResolver {
	return func(ctx context.Context, value, conceptType string) (string, error) {
		if e.engine == nil {
			return value, fmt.Errorf("MemQL engine not configured")
		}
		return e.engine.CanonicalizeIdValue(ctx, value, conceptType)
	}
}

// executionTracker tracks execution counts per automation for storm detection.
// It uses a sliding window approach to detect when an automation fires excessively.
type executionTracker struct {
	mu         sync.Mutex
	counts     map[string]*executionCount
	windowSize time.Duration
}

// executionCount tracks executions within a time window.
type executionCount struct {
	count       int
	windowStart time.Time
}

// newExecutionTracker creates a new execution tracker with the specified window size.
func newExecutionTracker(windowSize time.Duration) *executionTracker {
	return &executionTracker{
		counts:     make(map[string]*executionCount),
		windowSize: windowSize,
	}
}

// record increments the execution count for an automation and returns the current count.
// If the window has expired, it resets the count.
func (t *executionTracker) record(automationName string) int {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()

	if ec, ok := t.counts[automationName]; ok {
		// Check if window has expired
		if now.Sub(ec.windowStart) > t.windowSize {
			// Reset window
			ec.count = 1
			ec.windowStart = now
		} else {
			ec.count++
		}
		return ec.count
	}

	// First execution for this automation
	t.counts[automationName] = &executionCount{
		count:       1,
		windowStart: now,
	}
	return 1
}
