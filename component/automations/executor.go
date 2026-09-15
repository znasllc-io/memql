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
	"github.com/znasllc-io/memql/component/metrics"
	"github.com/znasllc-io/memql/component/provenance"
	"github.com/znasllc-io/memql/core/common"
)

// systemActorPrefix is the prefix for system-generated actor identifiers.
const systemActorPrefix = "system:automation:"

// contextWithSystemActor injects a system actor into the context for automation execution.
// This allows automations to execute mutations without requiring user authentication.
// The actor identifier includes the automation name for audit trail purposes.
//
// It stamps THREE surfaces, not two (memql#3620). Claims + TokenInfo are what
// `createdBy` and the engine's mutation-actor presence check read; the
// AccessContext is what `actor.userId` reads in a DSL `stamp { }` or filter.
// Setting only the first two is the trap auth.ContextWithUserActor's own doc
// comment describes: `actor.userId` then rendered as the EMPTY STRING, and
// dsl/forge's two audit automations wrote `v1:forge:requestEvent.actorUserId:
// ""` on every routed request and every status transition -- a required field,
// on a `@relationship(target=user)`, naming nobody. An automation IS a
// principal; writing its name is honest, writing "" is not.
//
// ONLY WHEN ABSENT, and that is load-bearing in both directions:
//
//   - AuthoredScheduler (authored_scheduler.go) runs an owner's automations
//     under AuthorContext, which stamps the AUTHOR's AccessContext precisely so
//     per-row authz confines the run to what the author may touch. Overwriting
//     it would hand every authored automation the engine's system principal
//     instead -- the opposite of what that helper exists to do.
//   - Conversely this must not INVENT one over an inherited caller: the
//     documented behaviour (component/server/server.go, memql#2888 / #2890) is
//     that this replaces claims + TokenInfo and leaves an AccessContext alone.
//
// RoleReader, not "system": Role is a closed enum (auth.AllRoles) and "system"
// is not in it, so stamping it would leave every rank comparison reading a role
// that ranks below nothing. Reader is the least-privileged valid role, which
// keeps actor.isClusterOwner FALSE -- the property memql#2801 made the envelope
// guarantee for an absent caller, and which must not weaken now that the caller
// is present.
//
// THE ONE EXCEPTION is the cluster's MAINTENANCE PRINCIPAL
// (component/auth/maintenance_actor.go, memql#4366 / memql#4406): a named,
// engine-owned, compiled-in list of automations whose reads span every owner by
// nature -- a retention sweep -- and which therefore run as a synthetic cluster
// owner. Read that file before adding to the list; the short version is that
// RoleReader makes a sweep over an owned-tier concept retire NOTHING, silently,
// with every gate green.
//
// The elevation applies only in the branch below where NO AccessContext was
// inherited, so the AuthoredScheduler invariant above is untouched: an authored
// run keeps the author's envelope, and a maintenance name it happened to share
// could not lift it.
func contextWithSystemActor(ctx context.Context, automationName string) context.Context {
	_, inherited := auth.AccessFromContext(ctx)

	actorId := systemActorPrefix + automationName
	role := "system"
	accessRole := auth.RoleReader
	// Gated on !inherited as well as on the list. Elevating the CLAIMS while
	// leaving an inherited AccessContext alone would leave the two surfaces
	// describing different principals -- and the surface that decides row
	// authz is the one we would not have touched.
	if !inherited {
		if ma := auth.MaintenanceActor(automationName); ma != nil {
			actorId = ma.UserId
			role = string(ma.Role)
			accessRole = ma.Role
		}
	}
	claims := map[string]any{
		"sub":   actorId,
		"email": actorId,
		"role":  role,
	}
	token := auth.BuildTokenInfo(claims)
	ctx = auth.ContextWithClaims(ctx, claims)
	ctx = auth.ContextWithToken(ctx, token)
	if !inherited {
		ctx = auth.ContextWithAccess(ctx, &auth.AccessContext{
			UserId: actorId,
			Role:   accessRole,
			// D4 (epic memql#4832): an automation's system actor is the
			// cluster acting, not a principal. accessRole above decides what
			// it may reach; it is not a rung, and the rank rules skip it.
			Unranked: true,
			// And SYNTHETIC: an automation is the cluster acting, so a row it
			// creates is not owned by the automation.
			Synthetic: true,
		})
	}
	return ctx
}

// Executor orchestrates automation execution.
type Executor struct {
	logger            *slog.Logger
	engine            *memql.MemQLEngine
	eventBus          *events.Bus
	stepRegistry      StepExecutorRegistry
	automationTrigger AutomationTrigger

	// sandboxRun suppresses journalling for Gate-2 dry-runs (memql#2932)
	sandboxRun bool

	// journal writes the run and step rows (journal.go). Nil under a
	// sandboxed dry-run and when no engine is configured; every method on a
	// nil journal is a no-op, so the loop below never branches on it.
	journal *workJournal

	// cancelPollInterval bounds how often a run asks whether it has been
	// cancelled (cancel.go). Zero means CancelPollInterval.
	cancelPollInterval time.Duration

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

	// PreviousChainHead is the chain state before this step executes. The
	// chain advances over the top-level statements only: a statement in a
	// `for` body or a parallel branch carries the chain position of the
	// statement that holds it.
	PreviousChainHead string

	// ChainTrackingEnabled indicates whether chain tracking is active.
	// Propagated from ExecutorOptions to child contexts.
	ChainTrackingEnabled bool
}

// ExecutorOptions configures the automation executor.
type ExecutorOptions struct {
	Logger            *slog.Logger
	Engine            *memql.MemQLEngine
	EventBus          *events.Bus
	StepRegistry      StepExecutorRegistry
	AutomationTrigger AutomationTrigger

	// SandboxRun marks this executor as driving a Gate-2 behavioural dry-run.
	//
	// A sandboxed run writes NO journal (memql#2932). A preview has nothing
	// to resume, and the record escaped the sandbox: the journal writer goes
	// straight to the engine, never through the interception layer, so a
	// "dry-run" left durable, RESUMABLE rows in the live graph.
	//
	// Since memql#2943 the sandbox registry refuses every step type it has not
	// classified, so the journal was the last write that escaped, and it is the
	// one that matters for #2890 / #2908, because it is the only escaping write
	// that is a RESUMABLE TOKEN.
	//
	// The failing preview was the one that wrote: the journal closes the run
	// at `failed` on step failure, so the refusal a preview exists to REPORT
	// was what produced the resumable row. NewExecutor holds no journal at all
	// when this is set, which is the mechanical form of that rule.
	SandboxRun bool

	// CancelPollInterval bounds how often a run re-reads its own
	// `cancelRequested` flag (memql#5066, cancel.go). Zero takes the default.
	//
	// It is an option only so a test can drive more than one boundary check
	// inside a run that finishes in microseconds. It is deliberately NOT
	// exposed per automation: a knob there would let one template opt out of
	// a control that exists to bound damage.
	CancelPollInterval time.Duration

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
		sandboxRun:           opts.SandboxRun,
		cancelPollInterval:   opts.CancelPollInterval,
		chainTrackingEnabled: opts.ChainTrackingEnabled,
		dedupEnabled:         opts.DedupEnabled,
		clusterGuard:         opts.ClusterGuard,
	}

	// The work journal (design record 2026-09-05-work-spine-design.md,
	// section D). opts.Engine is a *memql.MemQLEngine, and a nil pointer in
	// an interface is not a nil interface -- hence the explicit check rather
	// than letting newWorkJournal decide.
	if !opts.SandboxRun && opts.Engine != nil {
		e.journal = newWorkJournal(opts.Engine, opts.Logger)
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
	return e.executeWithEvent(ctx, automation, triggeredBy, triggeringEvent, false, nil)
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
// IS the authorization decision, because test/dslconformance/conformance_test.go treats
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
	return e.executeWithEvent(ctx, automation, triggeredBy, triggeringEvent, true, nil)
}

// adopt is nil for every trigger-driven run (the overwhelmingly common
// case) and non-nil only when a caller is executing an automation ONTO an
// existing v1:work:run row -- see adopt.go and memql#5054. Where it changes
// behaviour, the branch says why at the site.
func (e *Executor) executeWithEvent(ctx context.Context, automation *Automation, triggeredBy string, triggeringEvent *events.Event, callerSuppliedPayload bool, adopt *RunAdoption) (*AutomationExecution, error) {
	if automation == nil {
		return nil, fmt.Errorf("automation is nil")
	}
	if err := ensurePrepared(automation); err != nil {
		return nil, err
	}

	exec := NewExecution(automation.Name, triggeredBy)
	if adopt != nil {
		exec.ID = adopt.RunId
	}
	modeCtx, releaseMode, modeErr := sharedModeGate.acquire(ctx, automation, exec.ID)
	if modeErr != nil {
		if refusal, ok := modeErr.(*ModeRefusal); ok {
			if e.logger != nil {
				e.logger.Warn("automation mode refused fire", "automation", automation.Name, "error", refusal)
			}
			metrics.AutomationLoopStopped(automation.Name, metrics.LoopStopMode)
			exec.Status = "skipped"
			exec.Error = refusal.Error()
			exec.CompletedAt = time.Now()
			exec.Duration = exec.CompletedAt.Sub(exec.StartedAt)
			return exec, nil
		}
		return nil, modeErr
	}
	ctx = modeCtx
	defer releaseMode()
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

	// Internal origin requires a trusted SOURCE and a trigger payload the
	// caller did not supply. See ExecuteWithClientEvent for why the source
	// alone is not enough (memql#2888).
	exec.SourceTrusted = automation.Trusted && !callerSuppliedPayload
	exec.CallerSuppliedPayload = callerSuppliedPayload

	// THE EXECUTION *IS* THE EXISTING RUN (memql#5054). Taking the id here,
	// before anything reads exec.ID, is what makes every downstream write --
	// the journal's step rows, its heartbeats, its terminal close -- land on
	// the run the caller is already watching, with no second row.
	if adopt != nil {
		exec.ID = adopt.RunId
	}

	// The run's place in its causal chain (epic memql#5380): its parent -- the
	// triggering event's cause, else the calling run's for a sub-automation --
	// and its own, one deeper. The loop bound is decided here and acted on
	// after the dedup gates below; until then the fire runs under the cause
	// contextWithRunCause picks. See loop_runtime.go.
	parentCause, cause := runCause(ctx, automation, exec.ID, triggeringEvent)
	loopRefusal := loopBound(automation, cause, maxChainDepth())
	ctx = contextWithRunCause(ctx, parentCause, cause, loopRefusal)

	// Global execution budget (memql#1142). The storm WARN above is a
	// SIGNAL; this is the STOP. A process-global, cross-executor ceiling
	// (total + per-automation executions/window) hard-skips the execution
	// once a storm blows past it, so a misfiring automation can't re-fire
	// hundreds of times a minute and drive unbounded plan/LLM churn. Checked
	// here (after the concurrency slot, before any step work) so a skipped
	// run is cheap; the deferred concurrency-slot release still fires.
	if allowed, reason, alert := sharedAutomationBudget.admitRow(automation.Name, budgetRowId(triggeringEvent)); !allowed {
		if alert && e.logger != nil {
			e.logger.Error("automation execution budget exceeded -- SKIPPING executions to stop a storm (memql#1142)",
				"component", ComponentName,
				"automation", automation.Name,
				"dimension", reason,
			)
		}
		exec.Status = "skipped"
		exec.Error = "automation execution budget exceeded (memql#1142)"
		if reason == "per-row" {
			metrics.AutomationLoopStopped(automation.Name, metrics.LoopStopRowBudget)
			exec.Error = "automation execution budget exceeded for this row (per-row, memql#5382)"
		}
		exec.CompletedAt = time.Now()
		exec.Duration = exec.CompletedAt.Sub(exec.StartedAt)
		return exec, nil
	}

	evaluator := NewEvaluator()
	bindActorEnvelope(ctx, evaluator)

	// The resolvers the catalog's var(), systemVar(), secret() and
	// systemSecret() read.
	evaluator.SetVariableResolver(e.createVariableResolver())
	evaluator.SetSystemVariableResolver(e.createSystemVariableResolver())
	evaluator.SetSecretResolver(e.createSecretResolver())
	evaluator.SetSystemSecretResolver(e.createSystemSecretResolver())
	evaluator.SetCanonicalIdResolver(e.createCanonicalIdResolver())
	evaluator.SetLogger(e.logger)

	// The run's one clock, which `now` reads (runClock).
	evaluator.SetCustom("now", time.Now().UTC().Format(time.RFC3339))

	// The triggering event, under the `event` root.
	eventEnvelope := buildEventEnvelope(triggeringEvent, triggeredBy, trigger)
	if adopt != nil && adopt.Journal != nil && adopt.Journal.GoalId == "" && adopt.Journal.TriggerEvent != nil {
		eventEnvelope = adopt.Journal.TriggerEvent
	}
	evaluator.SetCustom("event", eventEnvelope)

	// Fire-time args validation (event-payload-binding ADR Decision 2,
	// memql#2363). This is the universal entry gate for every trigger mode:
	// the event-bus path already validated in the scheduler (before its
	// @filter), but the invoke-by-reference (TriggerAutomationWithArgs) and
	// http-trigger (TriggerAutomationWithEvent) paths reach here directly.
	// Re-validating is idempotent for the event path and is the single point
	// of enforcement for the others. A violation REFUSES the run -- no steps
	// execute -- and the validated map is exposed to the statements as
	// `args`. An automation without an args block binds nothing.
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
	}
	bindRunAmbient(ctx, e.engine, evaluator)

	if e.logger != nil {
		e.logger.Info("starting automation execution",
			"component", ComponentName,
			"automation", automation.Name,
			"executionId", exec.ID,
			"triggeredBy", triggeredBy,
		)
	}

	// Publish automation started event
	e.publishEvent(ctx, events.TopicAutomationStarted, events.KindAutomationStarted, map[string]any{
		"automationName": automation.Name,
		"executionId":    exec.ID,
		"triggeredBy":    triggeredBy,
	})

	// First-class precondition gate (Epic 4 / memql#2139). Evaluate every
	// precondition deterministically (no LLM) BEFORE any step runs. A miss
	// aborts the run cleanly -- no steps fire -- and emits the structured
	// healing.precondition.missed signal the self-healing repair loop
	// (E4.4) subscribes to. A miss is BOTH the clean repair trigger and the
	// cross-machine portability signal: a literal asserted here that does
	// not hold on this machine is a precondition that misses here.
	if missed, isMiss := EvaluatePreconditions(automation.Preconditions, evaluator); isMiss {
		e.emitPreconditionMiss(ctx, automation, exec, triggeringEvent, missed)
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
		eventData := chainEventData(triggeringEvent, automation, cause.CorrelationId)

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
		// AN ADOPTED RUN IS EXEMPT FROM BOTH DEDUP GATES, and this is not an
		// optimisation. Both key on the initial chain head, which is a hash of
		// (automation, triggeredBy, event, input) -- so two DIFFERENT goals
		// that compile to the same template with the same input produce the
		// SAME key. The per-process gate would skip the second as a duplicate
		// and the cluster guard's once-ever claim would skip every later one
		// cluster-wide, leaving those goals' runs at `running` with no
		// heartbeat until the abandoned sweep closed them. Two people asking
		// for the same thing is not a double-fire. An adopted run's identity
		// is its run id, and its claim is held by the caller (the work
		// integration's Postgres-backed ClusterExecutionGuard, keyed on that
		// id), which is both stronger and the right grain.
		duplicate, echo := false, false
		if adopt == nil && e.dedupEnabled && e.dedup != nil {
			identity := cause.CorrelationId
			if !parentCause.IsZero() {
				identity = rootCorrelation(triggeringEvent)
			}
			duplicate, echo = e.dedup.claimOrDuplicate(automation.Name, exec.InitialChainHead, identity, exec.ID)
			if !duplicate {
				defer func() {
					if exec.Status != "completed" {
						e.dedup.release(automation.Name, exec.InitialChainHead, exec.ID)
					}
				}()
			}
		}
		if duplicate {
			if echo {
				metrics.AutomationLoopStopped(automation.Name, metrics.LoopStopEcho)
			}
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
		if adopt == nil && e.clusterGuard != nil && exec.InitialChainHead != "" {
			if !e.clusterGuard.Claim(ctx, automation.Name, exec.InitialChainHead) {
				exec.Status = "skipped"
				exec.Error = "duplicate execution (cluster guard -- claimed by another replica)"
				exec.CompletedAt = time.Now()
				exec.Duration = exec.CompletedAt.Sub(exec.StartedAt)
				return exec, nil
			}
		}
	}

	// The loop bound (epic memql#5380), after the dedup gates and the cluster
	// guard's claim so one event refused on two replicas is recorded once.
	if loopRefusal != nil {
		return e.stopLoop(ctx, automation, exec, triggeringEvent, parentCause, loopRefusal, adopt != nil)
	}

	// Open the run row before the first step, so every later write has a
	// home. An automation whose trigger names a v1:work:* concept is NOT
	// journaled: its own step rows would re-fire it, and a feedback loop
	// through the graph is the failure an event-sourced substrate makes easy.
	journal := e.journal
	if journalSkipsAutomation(automation) {
		journal = nil
		if e.logger != nil {
			e.logger.Debug("work journal skipped: the automation reacts to work rows",
				"component", ComponentName, "automation", automation.Name)
		}
	}
	if adopt != nil {
		journal.adoptRun(ctx, automation, exec)
	} else {
		journal.openRun(ctx, automation, exec, triggeringEvent, parentCause)
	}
	// A logic a step calls journals its statements where this run's rows go
	// (logic_statements.go) -- nowhere, when the run is not journaled.
	ctx = withRunJournal(ctx, exec.ID, journal)

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

	// The statements run through runSequence (sequence.go), the one loop.
	return e.runStatementAutomation(ctx, automation, exec, triggeringEvent, journal, stepCtx, chainHead, nil)
}

// originForSource applies the #2800 trust rule: an automation body reaches the
// engine with INTERNAL origin only when its source came from the registered
// tree. Caller-submitted source (a bundle dry-run, an inline automation) stays
// at client origin, so it cannot launder itself into reaching a @serverOnly
// construct.
//
// This is a named function rather than an inline `if` because the inline form
// was DELETABLE WITH A GREEN SUITE: no end-to-end test entered the untrusted
// branch. The Executor holds a
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

// executeStep runs a single step using the registry.
func (e *Executor) executeStep(ctx context.Context, step *Step, stepCtx *StepContext) (*StepResult, error) {
	if e.stepRegistry == nil {
		return nil, fmt.Errorf("step registry not configured")
	}

	// Publish step started event
	e.publishEvent(ctx, events.TopicAutomationStepStarted, events.KindAutomationStepStarted, map[string]any{
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
	//   1. Stamped the automation's input evaluation only. No step went
	//      through that path -- every step type dispatches via
	//      stepRegistry.Execute -- so the kill switch was refused as a client
	//      call and silently suspended nothing. Closed-looking but open,
	//      exactly as the issue's park comment predicted.
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
	// Routed through originForSource so the untrusted branch stamps CLIENT
	// rather than inheriting whatever the parent carried -- see that
	// function's comment.
	trusted := stepCtx != nil && stepCtx.Execution != nil && stepCtx.Execution.SourceTrusted
	stepExecCtx := e.withRunContext(originForSource(ctx, trusted), stepCtx, step)
	result, err := e.stepRegistry.Execute(stepExecCtx, step, stepCtx)

	// A cancelled executor may return no result. Lifecycle reporting must not
	// turn cooperative cancellation into a nil-pointer panic.
	var stepDuration time.Duration
	if result != nil {
		stepDuration = result.Duration
	}
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
	e.publishEvent(ctx, stepTopic, stepKind, map[string]any{
		"automationName": stepCtx.Execution.AutomationName,
		"executionId":    stepCtx.Execution.ID,
		"stepId":         step.ID,
		"stepType":       string(step.Type),
		"duration":       stepDuration.Milliseconds(),
	})

	return result, err
}

// withRunContext stamps the work run this step belongs to (memql#4999), so a
// model call the step makes is journaled against the run and can be served
// back on a replay.
//
// IT IS THE SAME RUN THE JOURNAL ALREADY OPENS. component/automations/journal.go
// writes v1:work:run at exec.ID and v1:work:step at the step's key; this puts
// those two ids where the engine's model seam can read them, because the path
// between here and a provider -- the step registry, the shape evaluator, the
// prompt renderer -- has no business carrying a run id in its signatures.
//
// A SANDBOXED RUN IS NOT STAMPED. A dry-run writes no journal at all
// (memql#2932), so stamping one would have its model calls looked up against a
// run whose rows do not exist and, worse, recorded into a run the preview was
// never supposed to leave behind.
//
// The mode is always live here. A replay is not driven from this executor: it
// is a derived run whose context integrations/work stamps at the point it
// dispatches the compile.
func (e *Executor) withRunContext(ctx context.Context, stepCtx *StepContext, step *Step) context.Context {
	if e == nil || e.sandboxRun || stepCtx == nil || stepCtx.Execution == nil || step == nil {
		return ctx
	}
	// Keep inherited lineage and replay policy, but identify the current
	// step when this executor owns that run. Nested executions keep their
	// parent's association rather than attributing a call to another run.
	// A step in a statement body's nested list is keyed by its list's path
	// (stepKeyIn, sequence.go); every other step's key is its id.
	if run, ok := common.RunFromContext(ctx); ok {
		if memql.BareShortId(run.RunId) == memql.BareShortId(stepCtx.Execution.ID) {
			run.StepKey = stepKeyIn(ctx, step.ID)
			return common.ContextWithRun(ctx, run)
		}
		return ctx
	}
	return common.ContextWithRun(ctx, common.RunContext{
		RunId:   stepCtx.Execution.ID,
		StepKey: stepKeyIn(ctx, step.ID),
		Mode:    common.RunModeLive,
		// No owner: an automation's run is the DEPLOYMENT's, which is what
		// journalContext's Synthetic actor already makes true of its run and
		// step rows. The journal reads a blank-owner run through the
		// cluster-owner query for exactly this reason.
	})
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
// This enables resuming the automation from the failed step later.
// handleAutomationError logs a failed run and publishes the failure event.
func (e *Executor) handleAutomationError(ctx context.Context, automation *Automation, exec *AutomationExecution, triggeringEvent *events.Event, err error) {
	if e.logger != nil {
		e.logger.Error("automation execution failed",
			"component", ComponentName,
			"automation", automation.Name,
			"executionId", exec.ID,
			"error", err,
		)
	}

	// Publish automation failed event
	e.publishEvent(ctx, events.TopicAutomationFailed, events.KindAutomationFailed, map[string]any{
		"automationName": automation.Name,
		"executionId":    exec.ID,
		"error":          err.Error(),
		"duration":       exec.Duration.Milliseconds(),
	})
}

// publishEvent publishes an automation event to the event bus, stamping the
// run's cause (component/events/cause.go, epic memql#5380) onto it when ctx
// carries one. These are the executor's own lifecycle events
// (automation.started/completed/failed, the per-step started/completed/failed
// pair) and the precondition-missed signal -- every one of them a downstream
// consequence of the run in ctx, so they belong on its chain exactly as a
// step's own writes and publishes do.
func (e *Executor) publishEvent(ctx context.Context, topic string, kind events.Kind, payload map[string]any) {
	if e.eventBus == nil {
		return
	}
	event := events.NewEvent(topic, kind, payload)
	if cause, ok := events.CauseFromContext(ctx); ok {
		event = event.WithCause(cause)
	}
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
func (e *Executor) emitPreconditionMiss(ctx context.Context, automation *Automation, exec *AutomationExecution, triggeringEvent *events.Event, missed *Precondition) {
	if missed == nil {
		return
	}
	// The same fact the event carries, kept on the execution so the failure
	// path's symptom table can read it as a value (epic memql#5127). The event
	// reaches the healer on whichever replica subscribed; this reaches the
	// journal on THIS one, and neither substitutes for the other.
	if exec != nil {
		exec.PreconditionMissed = true
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
	e.publishEvent(ctx, events.TopicPreconditionMissed, events.KindPreconditionMissed, payload)
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
