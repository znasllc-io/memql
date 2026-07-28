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

	// clusterGuard, when set, extends dedup ACROSS replicas (#561): after the
	// per-process dedup passes, it claims the (automation, dedup-key) in the
	// DB so only one replica executes when an event reaches several. nil =
	// single-replica behaviour (no cross-replica claim).
	clusterGuard executionClaimer

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

	// SkipCache is INERT since memql#2899, which deleted the step cache: it is
	// written by the resume path and read by nothing. Kept so the resume path's
	// intent survives if a cache is ever reintroduced, but it guarantees nothing
	// today -- do not rely on it for freshness. Tracked with the rest of the
	// orphaned cache plumbing in memql#2941.
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

	// MaxConcurrentExecutions limits how many automations can run simultaneously.
	// Prevents database connection exhaustion during event storms.
	// Defaults to 10 if not set (0 means use default, not unlimited).
	MaxConcurrentExecutions int

	// ClusterGuard extends dedup across replicas (#561): after the per-process
	// dedup passes, the executor claims the (automation, dedup-key) in the DB
	// so an event reaching multiple replicas executes once cluster-wide. nil =
	// single-replica behaviour. Only set on the EVENT executor.
	ClusterGuard executionClaimer
}

// executionClaimer is the cross-replica execution gate (#561). Claim returns
// true when THIS node should run the automation, false when another replica
// already owns this (automation, event). Satisfied by *ClusterExecutionGuard.
type executionClaimer interface {
	Claim(ctx context.Context, automationName, dedupKey string) bool
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
		clusterGuard:         opts.ClusterGuard,
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

// buildEventEnvelope produces the object that gets seeded into the
// evaluator under the `event` global (and as ctx.input) for an
// automation run.
//
// For an EVENT-triggered run it carries the real triggering event's
// topic / kind / payload. For a SCHEDULE-, manual-, or startup-
// triggered run (triggeringEvent == nil) it returns a SYNTHETIC
// object envelope rather than leaving `event` unset.
//
// Why synthetic-but-non-nil matters (issue #418): scheduled
// automations whose step passes the conventional
// `logic xxx { event: event }` argument (e.g. expireGuestInvitations,
// magicLinkExpirySweep, rolloverDailySpace, feedbackTimeoutAutoPause)
// compile that argument to the runtime reference "event". If `event`
// is unseeded the reference fails to resolve, the function-arg
// renderer emits the unresolved literal token `event=event`, the
// engine coerces it to a STRING, and the receiving logic function's
// `event: object` validation trips with
// `argument "event": expected object, got string` on every cron tick.
// Returning an object here keeps BOTH trigger paths' `event` argument
// an object, so one dispatch-level fix covers every scheduled
// automation uniformly. The trigger source rides on the payload
// (`triggeredBy`) so logic bodies can branch on it if needed.
func buildEventEnvelope(triggeringEvent *events.Event, triggeredBy, trigger string) map[string]any {
	if triggeringEvent != nil {
		envelope := map[string]any{
			"topic":   triggeringEvent.Topic,
			"kind":    triggeringEvent.Kind.String(),
			"payload": triggeringEvent.Payload,
		}
		// G4 (memql#2366 / ADR Decision 4): expose the acting identity and
		// the event's occurrence time on the envelope. `event.actor` is only
		// present when the emitter stamped Metadata["actor"] -- an absent
		// actor stays ABSENT (no empty map), so exists(event.actor) checks
		// stay honest. `event.timestamp` (RFC3339) is distinct from the
		// reserved `now` captured at eval start.
		if actorId := triggeringEvent.Metadata["actor"]; actorId != "" {
			envelope["actor"] = map[string]any{"id": actorId}
		}
		if !triggeringEvent.Timestamp.IsZero() {
			envelope["timestamp"] = triggeringEvent.Timestamp.UTC().Format(time.RFC3339)
		}
		return envelope
	}
	return map[string]any{
		"topic":   trigger,
		"kind":    triggeredBy,
		"payload": map[string]any{"triggeredBy": triggeredBy},
	}
}

// ExecuteWithEvent runs an automation with an optional triggering event, from a
// SERVER-ORIGINATED trigger: a graph event, a cron tick, a sub-automation. The
// automation's source decides whether its steps reach the engine with internal
// origin.
func (e *Executor) ExecuteWithEvent(ctx context.Context, automation *Automation, triggeredBy string, triggeringEvent *events.Event) (*AutomationExecution, error) {
	return e.executeWithEvent(ctx, automation, triggeredBy, triggeringEvent, false)
}

// ExecuteWithClientEvent runs an automation whose TRIGGER PAYLOAD came from a
// caller, and therefore never grants internal origin however trusted the
// automation's source is (memql#2888).
//
// The #2800 rule -- "trust rides on the automation's SOURCE, not on which
// function does the dispatching" -- is right and is not sufficient. Its earlier
// phrasing carried the half that explains why:
//
//	a client can certainly cause an automation to fire, but it cannot choose
//	which constructs the authored body invokes.
//
// On the MCP run_automation path the client still cannot choose the CONSTRUCT,
// but it chooses the ARGUMENT -- and for a @serverOnly construct the argument
// IS the authorization decision, because dsl/conformance_test.go treats
// @serverOnly as the bucket that EXEMPTS a construct from carrying any
// caller-scope filter. Origin is the only gate, so an unchecked argument is an
// unchecked query.
//
// Concretely, before this existed: run_automation("killSwitchSuspendsRunningPlans",
// {node:{id: <any user>}}) reached runningPlansForUser (@serverOnly) with an
// attacker-chosen userId and then transitioned every plan it returned through
// updatePlanStatus, which stamps `id: args.planId` with no owner predicate --
// a cross-user write, not just a read leak. @filter does not help: the
// scheduler evaluates it on the event-bus path, not here, and the caller
// supplies the payload it would test.
//
// So the rule is: internal origin requires BOTH a trusted source AND a trigger
// payload the caller did not supply.
func (e *Executor) ExecuteWithClientEvent(ctx context.Context, automation *Automation, triggeredBy string, triggeringEvent *events.Event) (*AutomationExecution, error) {
	return e.executeWithEvent(ctx, automation, triggeredBy, triggeringEvent, true)
}

func (e *Executor) executeWithEvent(ctx context.Context, automation *Automation, triggeredBy string, triggeringEvent *events.Event, callerSuppliedPayload bool) (*AutomationExecution, error) {
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
	// Internal origin requires a trusted SOURCE and a trigger payload the
	// caller did not supply. See ExecuteWithClientEvent for why the source
	// alone is not enough (memql#2888).
	exec.SourceTrusted = automation.Trusted && !callerSuppliedPayload
	exec.CallerSuppliedPayload = callerSuppliedPayload

	// Global execution budget (memql#1142). The storm WARN above is a
	// SIGNAL; this is the STOP. A process-global, cross-executor ceiling
	// (total + per-automation executions/window) hard-skips the execution
	// once a storm blows past it, so a misfiring automation can't re-fire
	// hundreds of times a minute and drive unbounded plan/LLM churn. Checked
	// here (after the concurrency slot, before any step work) so a skipped
	// run is cheap; the deferred concurrency-slot release still fires.
	if allowed, reason := sharedAutomationBudget.admit(automation.Name); !allowed {
		if reason != "" && e.logger != nil {
			e.logger.Error("automation execution budget exceeded -- SKIPPING executions to stop a storm (memql#1142)",
				"component", ComponentName,
				"automation", automation.Name,
				"dimension", reason,
			)
		}
		exec.Status = "skipped"
		exec.Error = "automation execution budget exceeded (memql#1142)"
		exec.CompletedAt = time.Now()
		exec.Duration = exec.CompletedAt.Sub(exec.StartedAt)
		return exec, nil
	}

	evaluator := NewEvaluator()
	bindActorEnvelope(ctx, evaluator)

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
	eventEnvelope := buildEventEnvelope(triggeringEvent, triggeredBy, trigger)
	evaluator.SetCustom("event", eventEnvelope)
	evaluator.SetCustom("ctx", map[string]any{
		"input":  eventEnvelope,
		"output": nil,
		"error":  "",
	})

	// Fire-time args validation (event-payload-binding ADR Decision 2,
	// memql#2363). This is the universal entry gate for every trigger mode:
	// the event-bus path already validated in the scheduler (before its
	// @filter), but the invoke-by-reference (TriggerAutomationWithArgs) and
	// http-trigger (TriggerAutomationWithEvent) paths reach here directly.
	// Re-validating is idempotent for the event path and is the single point
	// of enforcement for the others. A violation REFUSES the run -- no steps
	// execute -- and the validated map is exposed to the step evaluator as
	// `args` (bodies in G1 may read `args.X`). Automations without an args
	// block bind nothing and behave exactly as before.
	boundArgs, extraArgs, argErr := bindEventArgs(automation, triggeringEvent)
	if argErr != nil {
		topic := trigger
		if triggeringEvent != nil {
			topic = triggeringEvent.Topic
		}
		refuseFireForArgs(e.logger, automation.Name, topic, argErr)
		exec.Status = "skipped"
		exec.Error = fmt.Sprintf("args contract violation: %v", argErr)
		exec.CompletedAt = time.Now()
		exec.Duration = exec.CompletedAt.Sub(exec.StartedAt)
		return exec, nil
	}
	if len(extraArgs) > 0 && e.logger != nil {
		e.logger.Debug("automation: undeclared payload fields ignored (tolerant-reader)",
			"component", ComponentName,
			"automation", automation.Name,
			"extraFields", extraArgs,
		)
	}
	if boundArgs != nil {
		evaluator.SetCustom("args", boundArgs)
		// G2 (memql#2364): the declared-field set lets a declared-but-absent
		// optional field resolve bare to nil instead of the literal fallback.
		evaluator.SetCustom("argsDeclared", declaredArgsSet(automation))
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
		// #2800: the input block reaches the engine exactly like a step, so
		// it gets the same trust rule -- stamped only when the automation's
		// SOURCE came from the registered tree.
		// exec.SourceTrusted, not automation.Trusted (memql#2888 review): the
		// `input:` block kept the OLD rule, so the caller-payload downgrade
		// never reached it and the two origin decisions could drift.
		//
		// UNTESTED, deliberately stated: executeInput returns early without an
		// engine and nothing populates Automation.Input today, so a mutation
		// here stays green. The alignment is for the drift, not for a
		// reachable exploit -- if `input:` ever becomes live, it inherits the
		// right rule instead of the one that was already wrong.
		inputResult, err := e.executeInput(originForSource(ctx, exec.SourceTrusted), automation.Input)
		if err != nil {
			exec.Fail(fmt.Errorf("input query failed: %w", err))
			e.handleAutomationError(ctx, automation, exec, triggeringEvent, err)
			return exec, err
		}
		exec.Input = inputResult
		evaluator.SetInput(inputResult)
	}

	// First-class precondition gate (Epic 4 / memql#2139). Evaluate every
	// precondition deterministically (no LLM) BEFORE any step runs. A miss
	// aborts the run cleanly -- no steps fire -- and emits the structured
	// healing.precondition.missed signal the self-healing repair loop
	// (E4.4) subscribes to. A miss is BOTH the clean repair trigger and the
	// cross-machine portability signal: a literal asserted here that does
	// not hold on this machine is a precondition that misses here.
	if missed, isMiss := EvaluatePreconditions(automation.Preconditions, evaluator); isMiss {
		e.emitPreconditionMiss(automation, exec, triggeringEvent, missed)
		exec.Status = "skipped"
		exec.Error = fmt.Sprintf("precondition %q missed", missed.ID)
		exec.CompletedAt = time.Now()
		exec.Duration = exec.CompletedAt.Sub(exec.StartedAt)
		if e.logger != nil {
			e.logger.Info("automation precondition missed -- run aborted (self-healing repair trigger emitted)",
				"component", ComponentName,
				"automation", automation.Name,
				"precondition", missed.ID,
				"check", missed.Check,
			)
		}
		return exec, nil
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

		// Cross-replica dedup (#561): the per-process check above only covers
		// THIS pod. When a node-type runs >=2 replicas an event can reach more
		// than one; the cluster guard claims the (automation, chain-head) in
		// the DB so exactly one replica executes. Only the event path carries
		// a guard (scheduled runs are gated by the cron leader instead).
		if e.clusterGuard != nil && exec.InitialChainHead != "" {
			if !e.clusterGuard.Claim(ctx, automation.Name, exec.InitialChainHead) {
				exec.Status = "skipped"
				exec.Error = "duplicate execution (cluster guard -- claimed by another replica)"
				exec.CompletedAt = time.Now()
				exec.Duration = exec.CompletedAt.Sub(exec.StartedAt)
				return exec, nil
			}
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
// originForSource applies the #2800 trust rule: an automation body reaches the
// engine with INTERNAL origin only when its source came from the registered
// tree. Caller-submitted source (a bundle dry-run, an inline automation) stays
// at client origin, so it cannot launder itself into reaching a @serverOnly
// construct.
//
// This is a named function rather than an inline `if` because the inline form
// was DELETABLE WITH A GREEN SUITE: the only end-to-end tests drive automations
// with no `input:` block, so the branch was never entered. The Executor holds a
// concrete *memql.MemQLEngine rather than an interface, so there is no seam to
// inject a capturing engine through -- extracting the decision is what makes it
// assertable at all. See TestOriginForSource.
// The untrusted branch stamps CLIENT explicitly rather than passing ctx
// through. Passing through looks equivalent -- OriginClient is the zero value
// -- but it is not: it INHERITS whatever the parent context carried. An
// untrusted body executed on a context descended from server-side Go (the MCP
// run_automation runner, a nested automation dispatched from an already-
// internal step) would then reach a @serverOnly construct on trust it was
// explicitly denied. That is the same laundering ContextWithClientOrigin
// exists to stop at the wire, and it was live here until a test asserted the
// inherited case.
func originForSource(ctx context.Context, trusted bool) context.Context {
	if trusted {
		return auth.ContextWithInternalOrigin(ctx)
	}
	return auth.ContextWithClientOrigin(ctx)
}

func (e *Executor) executeInput(ctx context.Context, input *AutomationInput) (any, error) {
	if e.engine == nil {
		return nil, fmt.Errorf("MemQL engine not configured")
	}

	query := input.Query
	if input.Limit > 0 {
		query = fmt.Sprintf("paginate(%s, %d)", query, input.Limit)
	}

	// #2800: same trust rule as executeStep -- an inline automation's
	// `input:` block is caller-supplied source too, so it cannot be stamped
	// unconditionally. The automation record is not threaded into this frame,
	// so the stamp is applied by ExecuteWithEvent (which has it) before
	// calling here; this frame deliberately does NOT stamp.
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

	// Publish step started event
	e.publishEvent(events.TopicAutomationStepStarted, events.KindAutomationStepStarted, map[string]any{
		"automationName": stepCtx.Execution.AutomationName,
		"executionId":    stepCtx.Execution.ID,
		"stepId":         step.ID,
		"stepType":       string(step.Type),
	})

	// Execute the step.
	//
	// #2800: a step whose automation came from the REGISTERED DSL TREE may
	// reach @serverOnly constructs; one whose body was supplied by a caller
	// may not. killSwitchSuspendsRunningPlans is the motivating case for the
	// first half -- it reads the affected USER's running plans, and the actor
	// is the automation's context rather than that user, so the construct
	// cannot be scoped to actor.userId and is barred from the wire instead.
	//
	// THE CONDITION IS THE SECURITY PROPERTY. Do not simplify it to an
	// unconditional stamp; two earlier attempts were wrong in opposite
	// directions and the reasoning for each looked sound at the time:
	//
	//   1. Stamped the `input:` block only. No step goes through that path --
	//      every step type dispatches via stepRegistry.Execute -- so the kill
	//      switch was refused as a client call and silently suspended
	//      nothing. Closed-looking but open, exactly as the issue's park
	//      comment predicted.
	//
	//   2. Stamped HERE unconditionally, justified by "executeStep is
	//      reachable only from automation execution and resume". That is
	//      TRUE and is NOT a security argument: automation execution includes
	//      automations whose body the caller supplied. RunBundleDryRun
	//      compiles submitted source and drives this exact frame, and its
	//      reads are not sandboxed -- so MCP run_inline_automation and the
	//      planner's LLM-emitted bundle could each wrap a @serverOnly read in
	//      a step and have it execute with internal origin.
	//
	// Trust therefore rides on the automation's SOURCE (Automation.Trusted,
	// granted only by the unified tree loader), not on which function does
	// the dispatching.
	//
	// It also must not go any deeper. LogicRunner is shared with the
	// client-callable `logic foo(...)` path, so stamping there would let a
	// caller launder client origin by wrapping a @serverOnly read in a logic.
	//
	// Routed through originForSource so the step and input: paths cannot
	// drift, and so the untrusted branch stamps CLIENT rather than inheriting
	// whatever the parent carried -- see that function's comment.
	trusted := stepCtx != nil && stepCtx.Execution != nil && stepCtx.Execution.SourceTrusted
	result, err := e.stepRegistry.Execute(originForSource(ctx, trusted), step, stepCtx)

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

	return result, err
}

// saveCheckpointOnFailure persists a checkpoint when an automation fails.
// newCheckpointFromExecution builds the checkpoint a failed run is resumed
// from.
//
// Extracted from saveCheckpointOnFailure so the FIELD MAPPING is assertable
// (memql#2888). The security-relevant field is CallerSuppliedPayload: resume
// reads it to decide whether internal origin may be restored, so a checkpoint
// that drops it hands a caller-parameterised run full trust on replay. Inside
// saveCheckpointOnFailure that mapping was unreachable from a test -- the
// function needs a live *memql.MemQLEngine and returns nothing -- and both
// mutations that dropped the flag left the suite GREEN. Same reason
// originForSource was extracted in #2800: a decision you cannot reach is a
// decision you cannot defend.
func newCheckpointFromExecution(
	automation *Automation,
	exec *AutomationExecution,
	failedStep *Step,
	stepIndex int,
	chainHead string,
	stepErr error,
	triggeringEvent *events.Event,
) *ExecutionCheckpoint {
	checkpoint := &ExecutionCheckpoint{
		ExecutionId:           exec.ID,
		AutomationName:        automation.Name,
		AutomationFingerprint: automation.DefinitionFingerprint(fingerprintEngine),
		StepIndex:             stepIndex,
		ChainHead:             chainHead,
		InitialChainHead:      exec.InitialChainHead,
		CallerSuppliedPayload: exec.CallerSuppliedPayload,
		StepResults:           ToMinimalStepResults(exec.Steps),
		StepOrder:             exec.StepOrder, // tracked order, not map iteration
		FailedAt: &StepFailure{
			StepId:    failedStep.ID,
			Error:     stepErr.Error(),
			Timestamp: time.Now().UTC(),
		},
		Input:            exec.Input,
		InputFingerprint: exec.InputFingerprint,
	}
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
	return checkpoint
}

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

	checkpoint := newCheckpointFromExecution(automation, exec, failedStep, stepIndex, chainHead, stepErr, triggeringEvent)

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
		bindActorEnvelope(ctx, evaluator)
		evaluator.SetCustom("error", err.Error())
		evaluator.SetCustom("timestamp", time.Now().UTC().Format(time.RFC3339))
		if triggeringEvent != nil {
			eventMap := buildEventEnvelope(triggeringEvent, "", "")
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

// emitPreconditionMiss publishes the structured self-healing repair-
// trigger signal (Epic 4 / memql#2139) when a first-class precondition
// misses. It rides the dedicated healing.precondition.missed topic (NOT
// automation.#, which the mesh blocks) so the repair loop (E4.4) hears it
// even on a different replica -- a healing.# forward routing rule
// (component/node/routing.go) carries it across the mesh.
//
// The payload carries everything the repair loop + typed-patch model
// (E4.3) need to propose a heal: the automation + precondition identity,
// the deterministic check that failed, the asserted machine-specific
// literal, and the triggering event payload (the concrete value that did
// not satisfy the check on THIS machine).
func (e *Executor) emitPreconditionMiss(automation *Automation, exec *AutomationExecution, triggeringEvent *events.Event, missed *Precondition) {
	if missed == nil {
		return
	}
	payload := map[string]any{
		"automationName":          automation.Name,
		"automationOrigin":        automation.Origin,
		"executionId":             exec.ID,
		"preconditionId":          missed.ID,
		"check":                   missed.Check,
		"literal":                 missed.Literal,
		"preconditionDescription": missed.Description,
	}
	if triggeringEvent != nil {
		payload["triggerTopic"] = triggeringEvent.Topic
		payload["triggerPayload"] = triggeringEvent.Payload
		if triggeringEvent.Partition != "" {
			payload["partition"] = triggeringEvent.Partition
		}
	}
	e.publishEvent(events.TopicPreconditionMissed, events.KindPreconditionMissed, payload)
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
// autoJoinAI's `concat("ga-", hash(canonicalId(ctx.actor,
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
