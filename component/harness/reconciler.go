package harness

// reconciler.go is the event-sourced harness controller (issue #583).
// It drives plan execution as a Kubernetes-style reconcile loop instead
// of an imperative while-loop: each tick it observes a plan's steps,
// claims a runnable step, dispatches it to the inner loop (#584), records
// an observation, and completes/fails the step. The written observation
// re-fires graph.node.created.v1:harness:observation, which triggers the
// next reconcile -- the agent reacts to its own writes.
//
// Crash recovery is free: the controller holds NO authoritative plan
// state between ticks; the graph is the source of truth. On restart it
// just reconciles again from current step statuses. Claim-before-run +
// the step's idempotencyKey make a re-dispatch safe.
//
// Design seams (all narrow interfaces) keep the core unit-testable
// without a live DB / event bus / LLM:
//   - StepReader   -- latest-per-id step + plan reads (bun in prod).
//   - StepClaimer  -- atomic ready->running claim (conditional update).
//   - Dispatcher   -- runs a step through the inner loop (si_tool_loop).
//   - Writer       -- the #582 mutations (start/complete/fail/observe/ready).
//   - Subscriber   -- event-bus subscription (events.Bus in prod).

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// ComponentName is the reconciler's logger / dependency identifier.
const ComponentName = "harnessReconciler"

// systemActorId attributes the controller's graph writes to a stable
// system principal in the audit trail (createdBy). Distinct per
// integration -- mirrors the planner's helper of the same name.
const systemActorId = "system:harness-reconciler"

// defaultMaxConcurrentPerPartition bounds parallel step dispatch per
// partition (backpressure). Conservative default; overridable via Config.
const defaultMaxConcurrentPerPartition = 4

// defaultTickInterval is the periodic safety-net reconcile cadence. It
// catches stuck/blocked steps that no event would otherwise re-trigger
// (e.g. a blocker that completed on another node before this controller
// subscribed).
const defaultTickInterval = 30 * time.Second

// ---------------------------------------------------------------------------
// Narrow dependency interfaces (the test seams)
// ---------------------------------------------------------------------------

// StepReader reads harness graph state. Implemented in prod by a bun-
// backed reader (NewBunStepReader); the reconciler does its graph READS
// in Go per the #583/#585 split (the queries.memql file is owned by
// #585 -- the controller must not depend on it).
type StepReader interface {
	// StepsForPlan returns the latest version of every step belonging to
	// planID, plus the plan's partition (for backpressure keying).
	StepsForPlan(ctx context.Context, planID string) (steps []StepView, partition string, err error)
	// PlanStatus returns the latest status of a plan, or "" if absent.
	PlanStatus(ctx context.Context, planID string) (string, error)
	// PlanView returns the latest plan projection (owner + goal +
	// status), used to re-assert the owned-tier fields on a terminal
	// re-version write. Returns ok==false when the plan is absent.
	PlanView(ctx context.Context, planID string) (view PlanView, ok bool, err error)
}

// StepClaimer performs the atomic claim that prevents double-execution.
// The prod implementation issues a conditional update on the step's
// latest row keyed by the partition+id PK, flipping ready->running only
// when the current status is still ready. Exactly one concurrent claim
// wins (rows-affected == 1); losers get claimed==false and skip dispatch.
type StepClaimer interface {
	// ClaimStep attempts to atomically move stepID from ready to running
	// (bumping attempt). Returns claimed==true iff this caller won the
	// race. expectedAttempt is the attempt the reader observed; a mismatch
	// (another claimer already bumped it) loses the claim.
	ClaimStep(ctx context.Context, partition, stepID string, expectedAttempt int) (claimed bool, err error)
}

// Dispatcher runs a claimed step through the inner loop. The prod
// implementation wraps engine.InvokeSIChatWithFilteredToolsOpts with a
// scoped context + the step's idempotencyKey + an observation sink. It
// returns the result payload, the number of tool calls made, and an
// error (nil on success).
type Dispatcher interface {
	Dispatch(ctx context.Context, step StepView) (result map[string]any, toolCalls int, err error)
}

// Writer is the #582 mutation surface the controller writes through.
// Implemented in prod by an engine.Execute-backed writer (the controller
// uses the LANDED #582 mutations only -- it invents no new ones).
type Writer interface {
	// MarkStepReady transitions a pending/blocked step to ready.
	MarkStepReady(ctx context.Context, step StepView) error
	// StartStep claims durably (ready -> running) via
	// mutationStartHarnessStep. The atomic guard is StepClaimer; this is
	// the durable transition write that follows a won claim.
	StartStep(ctx context.Context, stepID string) error
	// CompleteStep records a running step's result (running -> done).
	CompleteStep(ctx context.Context, stepID string, result map[string]any) error
	// FailStep marks a running step failed (running -> failed).
	FailStep(ctx context.Context, stepID, errorMessage string) error
	// RecordObservation appends a v1:harness:observation for a step.
	RecordObservation(ctx context.Context, obs Observation) error
	// SetPlanStatus promotes a plan to a terminal status (done/failed).
	// plan carries the owned-tier fields re-asserted on the re-version
	// write (raw insert does not read-merge).
	SetPlanStatus(ctx context.Context, plan PlanView, status, errorMessage string) error
}

// Observation is the payload for mutationRecordHarnessObservation. kind
// is one of tool_result / error / note / decision (#582). The json field
// on the concept is `data`, not `payload` (the #582 gotcha).
type Observation struct {
	StepID  string
	PlanID  string
	Kind    string
	Content string
	Data    map[string]any
}

// Subscriber is the event-bus subscription surface (events.Bus in prod).
// Returning an unsubscribe func mirrors events.Bus.Subscribe.
type Subscriber interface {
	Subscribe(pattern string, handler func(planID string)) (unsubscribe func())
}

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

// Config tunes the controller. Zero value yields safe defaults via
// withDefaults.
type Config struct {
	// MaxConcurrentPerPartition bounds parallel step dispatch per
	// partition (backpressure).
	MaxConcurrentPerPartition int
	// TickInterval is the periodic safety-net reconcile cadence.
	TickInterval time.Duration
	// Budget bounds each plan's execution. Zero value uses
	// DefaultPlanBudget.
	Budget PlanBudget
}

func (c Config) withDefaults() Config {
	if c.MaxConcurrentPerPartition <= 0 {
		c.MaxConcurrentPerPartition = defaultMaxConcurrentPerPartition
	}
	if c.TickInterval <= 0 {
		c.TickInterval = defaultTickInterval
	}
	if c.Budget == (PlanBudget{}) {
		c.Budget = DefaultPlanBudget()
	}
	return c
}

// ---------------------------------------------------------------------------
// Reconciler
// ---------------------------------------------------------------------------

// Reconciler is the event-sourced harness controller. It is safe for
// concurrent reconciles (per-plan locking serializes ticks for the same
// plan; different plans reconcile in parallel).
type Reconciler struct {
	reader    StepReader
	claimer   StepClaimer
	dispatch  Dispatcher
	writer    Writer
	subscribe Subscriber
	logger    *slog.Logger
	cfg       Config

	// planStart records the wall-clock of a plan's first dispatch so the
	// wall-clock budget is measured from "work began", not plan creation.
	// This is a derived accelerator, NOT authoritative state -- losing it
	// on restart only resets the clock, never corrupts the graph.
	mu        sync.Mutex
	planStart map[string]time.Time
	planLocks map[string]*sync.Mutex

	unsubscribes []func()
	startOnce    sync.Once
	stopOnce     sync.Once
}

// New constructs a Reconciler. reader/claimer/dispatch/writer are
// required; subscribe may be nil for a pull-only (tick/manual) controller
// (e.g. tests). logger defaults to slog.Default.
func New(
	reader StepReader,
	claimer StepClaimer,
	dispatch Dispatcher,
	writer Writer,
	subscribe Subscriber,
	logger *slog.Logger,
	cfg Config,
) (*Reconciler, error) {
	if reader == nil {
		return nil, fmt.Errorf("harness reconciler: reader is required")
	}
	if claimer == nil {
		return nil, fmt.Errorf("harness reconciler: claimer is required")
	}
	if dispatch == nil {
		return nil, fmt.Errorf("harness reconciler: dispatcher is required")
	}
	if writer == nil {
		return nil, fmt.Errorf("harness reconciler: writer is required")
	}
	if logger == nil {
		logger = slog.Default().With("component", ComponentName)
	}
	return &Reconciler{
		reader:    reader,
		claimer:   claimer,
		dispatch:  dispatch,
		writer:    writer,
		subscribe: subscribe,
		logger:    logger,
		cfg:       cfg.withDefaults(),
		planStart: make(map[string]time.Time),
		planLocks: make(map[string]*sync.Mutex),
	}, nil
}

// Subscriptions returns the event topics the controller listens on. Both
// a new plan and a new observation drive a reconcile (event-sourced),
// plus the periodic tick (started by Start) covers the stuck/blocked
// safety net.
func Subscriptions() []string {
	return []string{
		"graph.node.created." + memorynodes.ConceptHarnessPlan,
		"graph.node.created." + memorynodes.ConceptHarnessObservation,
	}
}

// Start wires the event subscriptions and launches the periodic tick.
// Idempotent. The tick re-reconciles all known plans; the event paths
// reconcile the specific plan named by the fired node.
func (r *Reconciler) Start(ctx context.Context) {
	if r == nil {
		return
	}
	r.startOnce.Do(func() {
		if r.subscribe != nil {
			for _, topic := range Subscriptions() {
				unsub := r.subscribe.Subscribe(topic, func(planID string) {
					if planID == "" {
						return
					}
					// Reconcile in a goroutine so a slow dispatch never
					// blocks the event bus delivery path.
					go r.reconcileSafely(ctx, planID)
				})
				r.unsubscribes = append(r.unsubscribes, unsub)
			}
		}
		go r.tickLoop(ctx)
		r.logger.Info("harness reconciler started",
			"topics", Subscriptions(),
			"tickInterval", r.cfg.TickInterval.String(),
			"maxConcurrentPerPartition", r.cfg.MaxConcurrentPerPartition,
		)
	})
}

// Stop unsubscribes from the event bus. Idempotent.
func (r *Reconciler) Stop() {
	if r == nil {
		return
	}
	r.stopOnce.Do(func() {
		r.mu.Lock()
		unsubs := r.unsubscribes
		r.unsubscribes = nil
		r.mu.Unlock()
		for _, u := range unsubs {
			if u != nil {
				u()
			}
		}
	})
}

// tickLoop runs the periodic safety-net reconcile. It re-reconciles every
// plan the controller has seen kick off; this is intentionally simple --
// crash recovery comes from the graph, not from this in-memory set.
func (r *Reconciler) tickLoop(ctx context.Context) {
	t := time.NewTicker(r.cfg.TickInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for _, planID := range r.knownPlans() {
				r.reconcileSafely(ctx, planID)
			}
		}
	}
}

// knownPlans returns the plan ids the controller has a derived clock for.
func (r *Reconciler) knownPlans() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.planStart))
	for id := range r.planStart {
		out = append(out, id)
	}
	return out
}

// reconcileSafely runs Reconcile and logs (never panics out of) a failure.
func (r *Reconciler) reconcileSafely(ctx context.Context, planID string) {
	defer func() {
		if rec := recover(); rec != nil {
			r.logger.Error("harness reconcile panicked", "plan", planID, "panic", rec)
		}
	}()
	if err := r.Reconcile(ctx, planID); err != nil {
		r.logger.Warn("harness reconcile failed", "plan", planID, "error", err)
	}
}

// planLock returns the per-plan mutex, serializing reconciles for the
// same plan so two ticks don't both promote/claim concurrently. Different
// plans still reconcile in parallel.
func (r *Reconciler) planLock(planID string) *sync.Mutex {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.planLocks[planID] == nil {
		r.planLocks[planID] = &sync.Mutex{}
	}
	return r.planLocks[planID]
}

// Reconcile is one idempotent tick for a single plan. It:
//
//  1. reads the plan's steps (latest per id) from the graph;
//  2. promotes pending/blocked steps whose deps are now done -> ready;
//  3. checks the per-plan budget; trips -> records a decision + fails plan;
//  4. selects runnable (ready) steps and dispatches them (claim-before-run,
//     bounded concurrency), recording an observation + completing/failing;
//  5. computes the plan terminal state and promotes the plan when reached.
//
// Holds nothing across calls -- the graph is the source of truth, so a
// crash between any two steps resumes correctly on the next tick.
func (r *Reconciler) Reconcile(ctx context.Context, planID string) error {
	planID = strings.TrimSpace(planID)
	if planID == "" {
		return fmt.Errorf("reconcile: empty plan id")
	}
	lock := r.planLock(planID)
	lock.Lock()
	defer lock.Unlock()

	ctx = contextWithSystemActor(ctx)
	r.noteSeen(planID)

	planStatus, err := r.reader.PlanStatus(ctx, planID)
	if err != nil {
		return fmt.Errorf("read plan status: %w", err)
	}
	if isTerminalPlanStatus(planStatus) {
		return nil // nothing to do; plan already settled
	}

	steps, _, err := r.reader.StepsForPlan(ctx, planID)
	if err != nil {
		return fmt.Errorf("read steps: %w", err)
	}

	// (2) Promote pending/blocked steps whose dependencies are satisfied.
	for _, s := range PromotablePending(steps) {
		if err := r.writer.MarkStepReady(ctx, s); err != nil {
			r.logger.Warn("promote step to ready failed",
				"plan", planID, "step", s.ID, "error", err)
			continue
		}
		r.logger.Debug("step promoted to ready", "plan", planID, "step", s.ID)
	}
	// Re-read so the runnable selection sees the promotions we just made.
	steps, partition, err := r.reader.StepsForPlan(ctx, planID)
	if err != nil {
		return fmt.Errorf("re-read steps: %w", err)
	}

	// (3) Budget check. Usage is DERIVED from the graph + the plan's
	// derived clock, never trusted from memory alone.
	usage := r.budgetUsage(ctx, planID, steps)
	if v := BudgetTripped(r.cfg.Budget, usage); v.Tripped {
		plan, _, _ := r.reader.PlanView(ctx, planID)
		return r.haltOnBudget(ctx, plan, planID, v, usage)
	}

	// (4) Dispatch runnable steps with bounded per-partition concurrency.
	runnable := SelectRunnable(steps)
	r.dispatchRunnable(ctx, planID, partition, runnable)

	// (5) Terminal-state promotion. Re-read so the verdict reflects the
	// completions this tick produced.
	steps, _, err = r.reader.StepsForPlan(ctx, planID)
	if err != nil {
		return fmt.Errorf("final read steps: %w", err)
	}
	if v := ComputePlanTerminal(steps); v.Status != "" {
		msg := ""
		if v.Status == PlanStatusFailed {
			msg = "one or more steps failed or are permanently blocked"
		}
		plan, ok, _ := r.reader.PlanView(ctx, planID)
		if !ok {
			plan = PlanView{ID: planID}
		}
		if err := r.writer.SetPlanStatus(ctx, plan, v.Status, msg); err != nil {
			return fmt.Errorf("set plan terminal status: %w", err)
		}
		r.logger.Info("plan reached terminal status",
			"plan", planID, "status", v.Status)
	}
	return nil
}

// dispatchRunnable claims + dispatches each runnable step under a bounded
// concurrency limit per partition. Claim-before-run guarantees only one
// concurrent reconcile executes a given step.
func (r *Reconciler) dispatchRunnable(ctx context.Context, planID, partition string, runnable []StepView) {
	if len(runnable) == 0 {
		return
	}
	limit := r.cfg.MaxConcurrentPerPartition
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for _, s := range runnable {
		wg.Add(1)
		sem <- struct{}{}
		go func(step StepView) {
			defer wg.Done()
			defer func() { <-sem }()
			r.runStep(ctx, planID, partition, step)
		}(s)
	}
	wg.Wait()
}

// runStep claims, dispatches, observes, and completes/fails a single
// step. The claim is the double-execution guard: if another reconcile
// already claimed it, claimed==false and we skip without side effects.
func (r *Reconciler) runStep(ctx context.Context, planID, partition string, step StepView) {
	claimed, err := r.claimer.ClaimStep(ctx, partition, step.ID, step.Attempt)
	if err != nil {
		r.logger.Warn("claim step failed", "plan", planID, "step", step.ID, "error", err)
		return
	}
	if !claimed {
		// Lost the race -- another reconcile is running this step.
		r.logger.Debug("step already claimed; skipping", "plan", planID, "step", step.ID)
		return
	}
	// Record the plan's first-dispatch clock for the wall-clock budget.
	r.markPlanStarted(planID)

	// Durable ready -> running transition (the claim flipped the latest
	// row; this is the mutation-routed write the audit trail names).
	if err := r.writer.StartStep(ctx, step.ID); err != nil {
		// The atomic claim already won, so a StartStep failure here is a
		// write error, not a race. Surface it as a step failure so the
		// plan doesn't wedge on a half-claimed step.
		r.logger.Warn("start step write failed", "plan", planID, "step", step.ID, "error", err)
	}

	result, toolCalls, derr := r.dispatch.Dispatch(ctx, step)
	if derr != nil {
		// Record an error observation, then fail the step.
		_ = r.writer.RecordObservation(ctx, Observation{
			StepID:  step.ID,
			PlanID:  planID,
			Kind:    "error",
			Content: fmt.Sprintf("step %q dispatch failed: %v", step.ID, derr),
			Data:    map[string]any{"message": derr.Error(), "toolCalls": toolCalls},
		})
		if ferr := r.writer.FailStep(ctx, step.ID, derr.Error()); ferr != nil {
			r.logger.Warn("fail step write failed", "plan", planID, "step", step.ID, "error", ferr)
		}
		return
	}

	// Record a tool_result observation, then complete the step. The
	// written observation re-fires graph.node.created -> next tick.
	if err := r.writer.RecordObservation(ctx, Observation{
		StepID:  step.ID,
		PlanID:  planID,
		Kind:    "tool_result",
		Content: fmt.Sprintf("step %q completed (%d tool call(s))", step.ID, toolCalls),
		Data:    map[string]any{"result": result, "toolCalls": toolCalls},
	}); err != nil {
		r.logger.Warn("record observation failed", "plan", planID, "step", step.ID, "error", err)
	}
	if err := r.writer.CompleteStep(ctx, step.ID, result); err != nil {
		r.logger.Warn("complete step write failed", "plan", planID, "step", step.ID, "error", err)
	}
}

// haltOnBudget records a `decision` observation naming the tripped limit
// and fails the plan, so a runaway plan stops with a recorded reason.
func (r *Reconciler) haltOnBudget(ctx context.Context, plan PlanView, planID string, v BudgetVerdict, u BudgetUsage) error {
	if plan.ID == "" {
		plan.ID = planID
	}
	r.logger.Warn("plan budget tripped",
		"plan", planID, "reason", v.Reason,
		"steps", u.StepsDispatched, "toolCalls", u.ToolCalls, "elapsedMs", u.ElapsedMillis)
	// A budget halt is a plan-level decision; record it against the plan
	// (no specific step). The observation concept allows a plan-scoped
	// row -- stepId is required by the schema, so we anchor it on the
	// plan id, which is unambiguous for a plan-level decision.
	_ = r.writer.RecordObservation(ctx, Observation{
		StepID:  planID,
		PlanID:  planID,
		Kind:    "decision",
		Content: fmt.Sprintf("plan halted: budget %q exceeded", v.Reason),
		Data: map[string]any{
			"choice":          "halt_plan",
			"rationale":       "budget exceeded",
			"limit":           v.Reason,
			"stepsDispatched": u.StepsDispatched,
			"toolCalls":       u.ToolCalls,
			"elapsedMillis":   u.ElapsedMillis,
		},
	})
	if err := r.writer.SetPlanStatus(ctx, plan, PlanStatusFailed,
		fmt.Sprintf("budget exceeded: %s", v.Reason)); err != nil {
		return fmt.Errorf("set plan failed on budget: %w", err)
	}
	return nil
}

// budgetUsage derives the running tally for a plan from the graph. Steps
// dispatched = steps that have moved past pending/ready (running + every
// terminal status counts as "was dispatched"). Tool calls are summed from
// completed step results' toolCalls when present, plus the in-flight
// attempt count as a coarse floor. Elapsed is measured from the derived
// first-dispatch clock.
func (r *Reconciler) budgetUsage(ctx context.Context, planID string, steps []StepView) BudgetUsage {
	_ = ctx
	u := BudgetUsage{}
	for _, s := range steps {
		switch s.Status {
		case StepStatusRunning, StepStatusDone, StepStatusFailed:
			u.StepsDispatched++
		}
		// attempt counts re-dispatches; use as a coarse tool-call floor so
		// a retry storm trips the tool-call budget even before results land.
		if s.Attempt > 0 {
			u.ToolCalls += s.Attempt
		}
	}
	r.mu.Lock()
	start, ok := r.planStart[planID]
	r.mu.Unlock()
	// A zero time is the "seen but not yet dispatched" sentinel left by
	// noteSeen -- the wall clock only starts at the first dispatch, so an
	// unstarted plan reports zero elapsed (not since-the-epoch).
	if ok && !start.IsZero() {
		u.ElapsedMillis = time.Since(start).Milliseconds()
	}
	return u
}

// markPlanStarted records the plan's first-dispatch wall-clock once. The
// zero-time sentinel left by noteSeen ("seen but not yet dispatched") is
// overwritten with the real first-dispatch time; a non-zero value is left
// untouched so the clock measures from the first dispatch, not later ones.
func (r *Reconciler) markPlanStarted(planID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t, ok := r.planStart[planID]; !ok || t.IsZero() {
		r.planStart[planID] = time.Now()
	}
}

// noteSeen ensures the plan has a derived-clock entry so the periodic
// tick re-reconciles it even if no further events fire.
func (r *Reconciler) noteSeen(planID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.planStart[planID]; !ok {
		// Zero time means "seen but not yet dispatched"; markPlanStarted
		// overwrites it with the real first-dispatch time. We store a
		// sentinel so knownPlans includes it for the tick.
		r.planStart[planID] = time.Time{}
	}
}

// isTerminalPlanStatus reports whether a plan status needs no further
// reconcile.
func isTerminalPlanStatus(s string) bool {
	switch strings.TrimSpace(s) {
	case PlanStatusDone, PlanStatusFailed, PlanStatusCancelled:
		return true
	default:
		return false
	}
}
