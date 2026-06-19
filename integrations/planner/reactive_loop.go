// reactive_loop.go
//
// The reactive planner loop (epic #632 / program #629). This is where
// the harness stops waiting and starts pursuing: a periodic heartbeat
// on the planner node that scans the cluster's active responsibilities,
// routes each due one to an agent, honors it according to its
// archetype, and -- as the capstone -- reasons across the user's space
// goals + responsibilities + recent memory together to act proactively.
//
// The four cohesive pieces (one loop, built incrementally):
//
//   C1 (#638) TICK -- a time.Ticker poll mirroring refresh_cron.go /
//      retry.go: spawned once at Start(), clean ctx shutdown. Each tick
//      sweeps queryActiveResponsibilitiesAcrossUsers (cross-user; the
//      planner node has no single user context), does the Go-side
//      due-check (cron for recurring, condition for reactive -- MemQL
//      can't evaluate either in a filter), records lastEvaluatedAt via
//      mutationRecordResponsibilityEvaluation, and dedups: a
//      responsibility with a live Plan already in flight is skipped
//      (continuity).
//
//   C2 (#639) ROUTING -- resolve the executing agent. assistant -> the
//      user's General Assistant. specialist / unassigned -> reuse the
//      ensureAgentForGoal factory (agentFactoryAnalyze +
//      createSpecialist/extendSpecialist) to match / extend / mint, then
//      persist the assignment back onto the responsibility
//      (mutationAssignResponsibility). Idempotent -- a row that already
//      names a concrete agent skips the factory.
//
//   C3 (#640) HONOR -- reactive / recurring -> create a Plan
//      (kind=agentProactive) via mutationCreatePlan owned by the
//      assigned agent; standing / behavioral -> inject the directive
//      into the assigned specialist's lineage.extensionGoals[] (the
//      agent-context-assembly path) so it applies whenever that
//      specialist works, with no Plan created. lastResult is stamped
//      either way.
//
//   C4 (#641) CONVERGENCE -- per user, assemble the active space goals
//      + the user's responsibilities + recall() and run the
//      reactiveConductor prompt: "given these, what should happen now?"
//      It emits zero or more proactive actions (create Plan / surface
//      nudge / extend specialist) and stays silent when nothing is
//      actionable, under a quiet/dedup guard so it pursues without
//      spamming.
//
// Authorization. The responsibility read/write surface is owned-tier
// (filters + stamps bind to actor.userId). The poller has no user
// context, so it reads the cross-user sweep under the SYSTEM actor,
// then -- for every owned-tier per-user write -- IMPERSONATES the row's
// owner by attaching an AccessContext{UserId: ownerUserId}. A row's
// owner can never touch another owner's responsibility: the sweep is
// read-only and each write goes back through the owned-tier mutation
// gated on actor.userId == the impersonated owner.

package planner

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	cron "github.com/robfig/cron/v3"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

const (
	// reactivePollInterval is how often the reactive loop wakes to scan
	// responsibilities. ~45s sits in the 30-60s band the issue calls for:
	// frequent enough that a reactive condition or a just-due recurring
	// schedule fires within a minute, infrequent enough that the
	// cross-user sweep + per-user convergence stay cheap.
	reactivePollInterval = 45 * time.Second

	// reactiveStartupDelay lets the engine + DSL finish loading before
	// the first pass so a long-overdue responsibility doesn't wait a full
	// interval, without racing engine readiness on a cold boot.
	reactiveStartupDelay = 90 * time.Second

	// recurringDueSlack is the window after a cron occurrence within
	// which we still treat a recurring responsibility as "due now". The
	// poll cadence means we observe an occurrence up to one interval
	// late; the slack (generously > the interval) absorbs that jitter so
	// a 09:00 Monday schedule fires on the first tick at/after 09:00 and
	// not a tick early.
	recurringDueSlack = 2 * time.Minute

	// honorRespawnGuard suppresses re-honoring the same responsibility
	// within this window after we spawn work for it -- the in-process
	// backstop to the DB-level live-Plan dedup. Covers the gap between
	// spawning a Plan and that Plan being observable as live, and
	// throttles standing/convergence re-injection so a behavioral
	// directive isn't re-appended every tick.
	honorRespawnGuard = 30 * time.Minute

	// convergenceGuard throttles the per-user convergence reasoning step
	// (C4). The conductor is an LLM call; running it every 45s per user
	// would be wasteful and spammy. Hourly per user is timely without
	// hammering the model or the user.
	convergenceGuard = time.Hour

	// reactiveProactivePlanKind is the Plan kind the loop spawns when it
	// honors a reactive / recurring responsibility (and what the
	// convergence step's createPlan action lands as). 'agentProactive'
	// is an existing v1:planner:plan.kind value (see dsl/planner/
	// concepts.memql) -- the agent-proactive surface from Q4.
	reactiveProactivePlanKind = "agentProactive"

	// reactiveSystemActor is the synthetic subject the loop stamps on
	// cross-user sweep reads (no single user). Per-user writes impersonate
	// the row owner instead (see ownerActorContext).
	reactiveSystemActor = "system:reactive-loop"

	// recallTopK / recallWindowConcept bound the convergence memory pull.
	recallTopK = 8
)

// ReactiveLoop is the planner-node heartbeat that scans active
// responsibilities and pursues them (epic #632). It owns its own poll
// goroutine (like RefreshCron) rather than depending on the engine's
// automation scheduler reaching this node-type binary.
type ReactiveLoop struct {
	engine Engine
	logger plannerLogger

	cancel      context.CancelFunc
	startedOnce sync.Once

	mu sync.Mutex
	// lastHonored tracks when we last spawned work / injected for a
	// responsibility id, for the in-process respawn guard.
	lastHonored map[string]time.Time
	// lastConverged tracks when we last ran the convergence step per
	// userId, for the per-user convergence throttle.
	lastConverged map[string]time.Time
}

// plannerLogger is the slog-shaped subset the loop logs through.
// Matches the structural interface RefreshCron uses so either a
// *slog.Logger or a test double satisfies it.
type plannerLogger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Debug(msg string, args ...any)
	Error(msg string, args ...any)
}

// NewReactiveLoop constructs the loop pinned to the planner
// integration's engine + logger.
func NewReactiveLoop(engine Engine, logger plannerLogger) *ReactiveLoop {
	return &ReactiveLoop{
		engine:        engine,
		logger:        logger,
		lastHonored:   make(map[string]time.Time),
		lastConverged: make(map[string]time.Time),
	}
}

// Start kicks off the poll goroutine. Idempotent -- the integration's
// Start can fire more than once; only the first call spawns the loop,
// which then runs for the process lifetime or until Stop.
func (r *ReactiveLoop) Start(ctx context.Context) {
	if r == nil {
		return
	}
	r.startedOnce.Do(func() {
		runCtx, cancel := context.WithCancel(context.Background())
		r.mu.Lock()
		r.cancel = cancel
		r.mu.Unlock()
		_ = ctx // parent ctx is informational; the loop runs for the
		// process lifetime like the training retry loop + refresh cron.
		go r.loop(runCtx)
	})
}

// Stop cancels the poll goroutine.
func (r *ReactiveLoop) Stop() {
	if r == nil {
		return
	}
	r.mu.Lock()
	cancel := r.cancel
	r.cancel = nil
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (r *ReactiveLoop) loop(ctx context.Context) {
	if r.logger != nil {
		r.logger.Info("planner reactive loop: started",
			"pollInterval", reactivePollInterval.String())
	}
	ticker := time.NewTicker(reactivePollInterval)
	defer ticker.Stop()
	startup := time.NewTimer(reactiveStartupDelay)
	defer startup.Stop()
	for {
		select {
		case <-ctx.Done():
			if r.logger != nil {
				r.logger.Info("planner reactive loop: stopped")
			}
			return
		case <-startup.C:
			r.tick(ctx)
		case <-ticker.C:
			r.tick(ctx)
		}
	}
}

// tick is one heartbeat: sweep active responsibilities cluster-wide,
// group them by owner, and process each user's set (C1->C3) followed by
// the per-user convergence step (C4). A no-op when nothing is due.
func (r *ReactiveLoop) tick(ctx context.Context) {
	if r.engine == nil {
		return
	}
	rows, err := r.sweepResponsibilities(ctx)
	if err != nil {
		if r.logger != nil {
			r.logger.Debug("planner reactive loop: sweep failed (engine likely not ready)",
				"error", err)
		}
		return
	}

	// Group by owner so per-user reasoning (convergence) sees the whole
	// set and the impersonation context is built once per user.
	byUser := map[string][]map[string]any{}
	order := []string{}
	for _, row := range rows {
		owner := getString(row, "ownerUserId")
		if owner == "" {
			continue
		}
		if _, seen := byUser[owner]; !seen {
			order = append(order, owner)
		}
		byUser[owner] = append(byUser[owner], row)
	}

	now := time.Now().UTC()
	honored := 0
	for _, userId := range order {
		userCtx := ownerActorContext(ctx, userId)
		for _, row := range byUser[userId] {
			if r.processResponsibility(userCtx, userId, row, now) {
				honored++
			}
		}
		// C4 convergence: reason across this user's goals +
		// responsibilities + memory once the per-row honoring is done.
		r.maybeConverge(userCtx, userId, byUser[userId], now)
	}
	if honored > 0 && r.logger != nil {
		r.logger.Info("planner reactive loop: honored responsibilities",
			"count", honored, "users", len(order), "candidates", len(rows))
	}
}

// sweepResponsibilities reads the cross-user candidate set under the
// system actor. Returns active + enabled rows of every trigger
// archetype (the Go-side code below filters by due-ness per archetype).
func (r *ReactiveLoop) sweepResponsibilities(ctx context.Context) ([]map[string]any, error) {
	res, err := r.engine.Execute(reactiveSystemActorContext(ctx),
		`queryActiveResponsibilitiesAcrossUsers({})`)
	if err != nil {
		return nil, err
	}
	return memql.MaterializeRows(res), nil
}

// processResponsibility runs the C1->C3 pipeline for one responsibility
// under its owner's impersonated context. Returns true when it honored
// the row (spawned a Plan or injected context). standing rows are NOT
// honored here on a schedule -- they're always-on context the routing
// step ensures is injected; the reactive/recurring rows are the
// due-driven ones.
func (r *ReactiveLoop) processResponsibility(ctx context.Context, userId string, row map[string]any, now time.Time) bool {
	respId := getString(row, "id")
	if respId == "" {
		return false
	}
	trigger := getString(row, "trigger")

	// standing / behavioral directives are never "due" on a schedule --
	// they're always-on context. Their honoring (#640) is making sure the
	// directive is present in the assigned specialist's context, which the
	// routing step does via maybeInjectStanding (itself throttled by the
	// respawn guard so it doesn't re-write the agent every tick). Route +
	// inject and stamp lastResult; no Plan, no due-check, no dedup query.
	if trigger == "standing" {
		agentId, err := r.routeResponsibility(ctx, userId, row)
		if err != nil {
			r.logger.Warn("planner reactive loop: standing routing failed",
				"responsibilityId", respId, "error", err)
			return false
		}
		// Only count + record when the routing produced a concrete agent
		// the directive could attach to.
		if agentId == "" {
			return false
		}
		// Respawn guard keys on the inject key inside maybeInjectStanding;
		// avoid stamping lastResult every tick by gating on the same key.
		if !r.shouldHonor("standing-eval:"+respId, now) {
			return false
		}
		r.recordEvaluation(ctx, respId, "standing directive active on agent "+agentId)
		r.markHonored("standing-eval:"+respId, now)
		return true
	}

	// C1 due-check (Go-side; MemQL can't evaluate cron or conditions).
	due := r.isDue(trigger, row, now)
	if !due {
		return false
	}

	// C1 dedup: skip if a live Plan already exists for this
	// responsibility (continuity) -- a recurring tick still spawns per
	// occurrence, but a reactive / in-flight one doesn't double-start.
	// We also apply the in-process respawn guard so a Plan we just
	// spawned but can't yet observe as live doesn't get re-spawned.
	if trigger != "recurring" && r.hasLivePlan(ctx, respId) {
		r.logger.Debug("planner reactive loop: live plan exists; skipping",
			"responsibilityId", respId)
		return false
	}
	if !r.shouldHonor(respId, now) {
		return false
	}

	// C2 routing: resolve + persist the executing agent.
	agentId, err := r.routeResponsibility(ctx, userId, row)
	if err != nil {
		r.logger.Warn("planner reactive loop: routing failed",
			"responsibilityId", respId, "error", err)
		return false
	}

	// C3 honor: archetype-specific. reactive / recurring -> Plan;
	// standing -> context injection (handled in routeResponsibility's
	// assignment for standing; here we only Plan the due archetypes).
	result, err := r.honorResponsibility(ctx, userId, row, agentId)
	if err != nil {
		r.logger.Warn("planner reactive loop: honor failed",
			"responsibilityId", respId, "error", err)
		// Still record the evaluation so lastEvaluatedAt advances and a
		// hard-failing row doesn't re-fire every tick.
		r.recordEvaluation(ctx, respId, "honor failed: "+err.Error())
		return false
	}

	r.recordEvaluation(ctx, respId, result)
	r.markHonored(respId, now)
	r.logger.Info("planner reactive loop: honored responsibility",
		"responsibilityId", respId, "trigger", trigger, "agentId", agentId,
		"result", result)
	return true
}

// --- C1: due-check ---------------------------------------------------------

// isDue applies the Go-side due-check per archetype.
//
//	recurring -> the cron `schedule` has an occurrence in the window
//	             (lastEvaluatedAt, now]; never-evaluated rows are due on
//	             the first occurrence at/before now.
//	reactive  -> a pending condition. The signal-matching evaluator
//	             (matching incoming events against payload.condition) is
//	             a deeper build-out; for the loop's first cut a reactive
//	             row is treated as due for re-evaluation on the cadence
//	             (the conductor + recall ground whether it should act),
//	             rate-limited by the respawn guard so it doesn't fire
//	             every tick.
//	standing  -> never due here; always-on context handled by routing.
func (r *ReactiveLoop) isDue(trigger string, row map[string]any, now time.Time) bool {
	switch trigger {
	case "recurring":
		return r.recurringIsDue(row, now)
	case "reactive":
		// Cadence re-evaluation: due unless we honored it recently (the
		// shouldHonor guard downstream enforces the actual rate limit).
		return true
	default:
		// standing (or unknown): not due on the heartbeat.
		return false
	}
}

// recurringIsDue parses the cron schedule and returns true when its most
// recent occurrence is in (lastEvaluatedAt, now] -- i.e. a scheduled
// tick came due since we last evaluated. A never-evaluated row fires on
// its first occurrence at/before now.
func (r *ReactiveLoop) recurringIsDue(row map[string]any, now time.Time) bool {
	schedule := strings.TrimSpace(getString(row, "schedule"))
	if schedule == "" {
		return false
	}
	sched, err := cron.ParseStandard(schedule)
	if err != nil {
		// Unparseable cron -- don't wedge or spam. Log-once-ish via Debug
		// and treat as not-due (a malformed schedule shouldn't fire every
		// tick).
		r.logger.Debug("planner reactive loop: bad cron schedule",
			"schedule", schedule, "error", err)
		return false
	}

	// Anchor the "since" point. Look back from one occurrence before now
	// to find the previous fire time, then check it's after when we last
	// evaluated (and recent enough to count as "now").
	last := parseTimeOrZero(getString(row, "lastEvaluatedAt"))
	// cron.Schedule only exposes Next(); to find the latest occurrence
	// <= now we step Next() forward from a point safely before now and
	// keep the last one that's still <= now.
	prev := latestOccurrenceBefore(sched, now)
	if prev.IsZero() {
		return false
	}
	if now.Sub(prev) > recurringDueSlack {
		// The occurrence is older than the slack window -- we already had
		// ticks since it passed; only fire if we never evaluated since.
		if last.IsZero() || last.Before(prev) {
			return true
		}
		return false
	}
	// Occurrence is within the slack window (just came due). Fire unless
	// we already evaluated at/after it.
	return last.IsZero() || last.Before(prev)
}

// --- C1: dedup -------------------------------------------------------------

// hasLivePlan returns true when a non-terminal Plan already exists for
// the responsibility (matched on input.responsibilityId). Terminal
// statuses are succeeded / failed / cancelled; anything else is live.
func (r *ReactiveLoop) hasLivePlan(ctx context.Context, respId string) bool {
	q := fmt.Sprintf(`queryPlansForResponsibility({responsibilityId:%q})`, respId)
	res, err := r.engine.Execute(ctx, q)
	if err != nil {
		// Fail open toward NOT-spawning would wedge the responsibility
		// forever on a transient read error; fail open toward spawning
		// risks a duplicate. The in-process respawn guard covers the
		// duplicate case, so treat a read error as "no live plan" and let
		// the guard prevent a storm.
		r.logger.Debug("planner reactive loop: live-plan check failed",
			"responsibilityId", respId, "error", err)
		return false
	}
	for _, p := range memql.MaterializeRows(res) {
		if !isTerminalPlanStatus(getString(p, "status")) {
			return true
		}
	}
	return false
}

// --- C2: routing -----------------------------------------------------------

// routeResponsibility resolves the executing agent for a responsibility
// and persists the assignment. Returns the resolved agentId (may be
// empty for an assistant-targeted row whose GA we couldn't resolve --
// honoring still proceeds, the assistant relays at work time).
//
//	assistant            -> the user's General Assistant.
//	specialist / unassigned, already bound -> reuse assignedAgentId
//	                        (idempotent; no factory re-run).
//	specialist / unassigned, unbound -> ensureAgentForGoal factory
//	                        (agentFactoryAnalyze + createSpecialist /
//	                        extendSpecialist), then persist + flip
//	                        targetKind off 'unassigned'.
func (r *ReactiveLoop) routeResponsibility(ctx context.Context, userId string, row map[string]any) (string, error) {
	respId := getString(row, "id")
	targetKind := getString(row, "targetKind")
	existing := getString(row, "assignedAgentId")

	switch targetKind {
	case "assistant":
		ga := r.resolveAssistant(ctx, userId)
		// Persist the assignment so the row carries the concrete GA id
		// (idempotent re-stamp).
		if ga != "" && ga != existing {
			r.assign(ctx, respId, "assistant", ga, "")
		}
		return ga, nil

	case "specialist":
		if existing != "" {
			// Already bound to a concrete specialist -- idempotent, no
			// factory re-run. For standing rows this also re-asserts the
			// directive injection (below) is current.
			r.maybeInjectStanding(ctx, row, existing)
			return existing, nil
		}
		fallthrough

	default: // unassigned (or specialist with no agent yet)
		agentId, err := r.mintOrExtendSpecialist(ctx, userId, row)
		if err != nil {
			return "", err
		}
		if agentId == "" {
			return "", fmt.Errorf("factory resolved no agent for responsibility %s", respId)
		}
		r.assign(ctx, respId, "specialist", agentId, getString(row, "assignedRoleSlug"))
		r.maybeInjectStanding(ctx, row, agentId)
		return agentId, nil
	}
}

// resolveAssistant resolves the user's active General Assistant id.
// Returns empty when none is found (the loop proceeds; the GA relays at
// work time once available).
func (r *ReactiveLoop) resolveAssistant(ctx context.Context, userId string) string {
	q := fmt.Sprintf(`queryAssistantAgentForUser({ownerUserId:%q})`, userId)
	res, err := r.engine.Execute(ctx, q)
	if err != nil {
		r.logger.Debug("planner reactive loop: resolveAssistant failed",
			"userId", userId, "error", err)
		return ""
	}
	rows := memql.MaterializeRows(res)
	if len(rows) == 0 {
		return ""
	}
	return getString(rows[0], "id")
}

// mintOrExtendSpecialist reuses the ensureAgentForGoal factory builtin
// (the same path the Planner Agent loop's createSpecialist /
// extendSpecialist actions use) to match / extend / mint a specialist
// for the responsibility's statement. The factory runs agentFactoryAnalyze
// and issues mutationCreateAgent / mutationUpdateAgent itself; we just
// pluck the resulting agentId.
func (r *ReactiveLoop) mintOrExtendSpecialist(ctx context.Context, userId string, row map[string]any) (string, error) {
	goal := getString(row, "statement")
	if goal == "" {
		return "", fmt.Errorf("responsibility has no statement to route on")
	}
	spaceId := getString(row, "scopeSpaceId")
	call := fmt.Sprintf(
		`ensureAgentForGoal({goal:%q, ownerUserId:%q, spaceId:%q})`,
		goal, userId, spaceId,
	)
	res, err := r.engine.Execute(ctx, call)
	if err != nil {
		return "", fmt.Errorf("ensureAgentForGoal: %w", err)
	}
	agentId, action := extractAgentFactoryResult(res)
	r.logger.Info("planner reactive loop: factory resolved specialist",
		"responsibilityId", getString(row, "id"), "agentId", agentId,
		"factoryAction", action)
	return agentId, nil
}

// assign persists the routing decision onto the responsibility via the
// owned-tier mutationAssignResponsibility. Best-effort: a failed assign
// doesn't block honoring (the routing result is already in hand for this
// tick), it just means the row's binding isn't durable until the next
// successful tick.
//
// Only non-empty optional fields (assignedAgentId, assignedRoleSlug) are
// included in the mutation call. Passing an empty string would write "" onto
// the row, clearing any existing binding. Omitting the field lets the
// partial-update merge keep the responsibility's previous value -- so a
// role-only assignment does not clear an existing agent binding, and an
// assistant-route call does not blank an existing roleSlug. (#1680)
func (r *ReactiveLoop) assign(ctx context.Context, respId, targetKind, agentId, roleSlug string) {
	args := map[string]any{
		"responsibilityId": respId,
		"targetKind":       targetKind,
	}
	if agentId != "" {
		args["assignedAgentId"] = agentId
	}
	if roleSlug != "" {
		args["assignedRoleSlug"] = roleSlug
	}
	q := fmt.Sprintf(`mutationAssignResponsibility(%s)`, encodeArgs(args))
	if _, err := r.engine.Execute(ctx, q); err != nil {
		r.logger.Warn("planner reactive loop: assign failed",
			"responsibilityId", respId, "agentId", agentId, "error", err)
	}
}

// --- C3: honor -------------------------------------------------------------

// honorResponsibility carries out the directive per archetype. reactive
// and recurring create a Plan (the assistant relays the result);
// standing injects the directive into the specialist's context and
// creates NO Plan. Returns the lastResult headline.
func (r *ReactiveLoop) honorResponsibility(ctx context.Context, userId string, row map[string]any, agentId string) (string, error) {
	trigger := getString(row, "trigger")
	switch trigger {
	case "reactive", "recurring":
		planId, err := r.createResponsibilityPlan(ctx, userId, row, agentId)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("spawned %s Plan %s (agent %s)", trigger, planId, agentId), nil
	case "standing":
		// Standing directives carry no Plan -- the injection happened in
		// routing (maybeInjectStanding). This branch is reached only when
		// a standing row is somehow due; re-assert the injection.
		r.maybeInjectStanding(ctx, row, agentId)
		return "injected standing directive into agent " + agentId, nil
	default:
		return "", fmt.Errorf("unknown trigger %q", trigger)
	}
}

// createResponsibilityPlan inserts an agentProactive Plan owned by the
// assigned agent, carrying the input.responsibilityId back-pointer the
// C1 dedup query (queryPlansForResponsibility) matches on. requestedBy
// is the responsibility owner; triggerSource='system' marks the
// provenance (no person asked).
func (r *ReactiveLoop) createResponsibilityPlan(ctx context.Context, userId string, row map[string]any, agentId string) (string, error) {
	respId := getString(row, "id")
	statement := getString(row, "statement")
	spaceId := getString(row, "scopeSpaceId")
	if spaceId == "" {
		// A responsibility without a pinned space still needs a Plan
		// spaceId (Plan concept @required). Use the synthetic reactive
		// space sentinel -- the assistant relays into the user's surfaces
		// regardless of this bookkeeping space.
		spaceId = "system:reactive-loop"
	}
	planId := id.NewSystemNodeShortId("proactive", respId)
	goal := statement
	if goal == "" {
		goal = "Honor responsibility " + respId
	}
	input := map[string]any{
		"responsibilityId": respId,
		"trigger":          getString(row, "trigger"),
		"statement":        statement,
		"successCriteria":  getString(row, "successCriteria"),
	}
	call := fmt.Sprintf(
		`mutationCreatePlan({planId:%q, spaceId:%q, kind:%q, goal:%q, requestedBy:%q, authorizedBy:%q, triggerSource:"system", input:%s})`,
		planId, spaceId, reactiveProactivePlanKind, goal, userId, userId, mustJSONObject(input),
	)
	if _, err := r.engine.Execute(ctx, call); err != nil {
		return "", fmt.Errorf("mutationCreatePlan: %w", err)
	}
	// Stamp the owning agent (best-effort) so downstream dispatch knows
	// who runs it. Reuse the plan-status update path.
	if agentId != "" {
		upd := fmt.Sprintf(
			`mutationUpdatePlanStatus({planId:%q, status:"routing", ownerAgentId:%q})`,
			planId, agentId,
		)
		if _, err := r.engine.Execute(ctx, upd); err != nil {
			r.logger.Debug("planner reactive loop: ownerAgent stamp failed",
				"planId", planId, "agentId", agentId, "error", err)
		}
	}
	return planId, nil
}

// maybeInjectStanding appends a standing / behavioral directive into the
// assigned specialist's lineage.extensionGoals[] so it's present in the
// agent's working context on its next task (the agent-context-assembly
// path reads extensionGoals). No-op for non-standing rows. Idempotent:
// the directive is appended only when not already present. Throttled by
// the respawn guard at the caller so a behavioral directive isn't
// re-read-merge-written every tick.
func (r *ReactiveLoop) maybeInjectStanding(ctx context.Context, row map[string]any, agentId string) {
	if getString(row, "trigger") != "standing" || agentId == "" {
		return
	}
	directive := strings.TrimSpace(getString(row, "statement"))
	if directive == "" {
		return
	}
	respId := getString(row, "id")
	// Throttle: only re-assert injection past the respawn guard so we
	// don't read-merge-write the agent every tick for a standing row.
	if !r.shouldHonor("standing-inject:"+respId, time.Now().UTC()) {
		return
	}

	agent := r.loadAgent(ctx, agentId)
	if agent == nil {
		r.logger.Debug("planner reactive loop: standing inject -- agent not found",
			"agentId", agentId, "responsibilityId", respId)
		return
	}
	lineage, _ := agent["lineage"].(map[string]any)
	goals := toStringList(mapGet(lineage, "extensionGoals"))
	for _, g := range goals {
		if strings.EqualFold(strings.TrimSpace(g), directive) {
			// Already injected -- nothing to do.
			r.markHonored("standing-inject:"+respId, time.Now().UTC())
			return
		}
	}
	goals = append(goals, directive)
	// Partial-update only the lineage.extensionGoals path. mutationUpdateAgent
	// merges the payload object onto the prior row.
	payload := map[string]any{
		"lineage": map[string]any{
			"extensionGoals": goals,
		},
	}
	call := fmt.Sprintf(`mutationUpdateAgent({agentId:%q, payload:%s})`,
		agentId, mustJSONObject(payload))
	if _, err := r.engine.Execute(ctx, call); err != nil {
		r.logger.Warn("planner reactive loop: standing inject failed",
			"agentId", agentId, "responsibilityId", respId, "error", err)
		return
	}
	r.markHonored("standing-inject:"+respId, time.Now().UTC())
	r.logger.Info("planner reactive loop: injected standing directive",
		"agentId", agentId, "responsibilityId", respId)
}

// recordEvaluation stamps lastEvaluatedAt + lastResult on the
// responsibility (owned-tier, under the impersonated owner ctx).
func (r *ReactiveLoop) recordEvaluation(ctx context.Context, respId, result string) {
	q := fmt.Sprintf(
		`mutationRecordResponsibilityEvaluation({responsibilityId:%q, lastResult:%q})`,
		respId, truncate(result, 280),
	)
	if _, err := r.engine.Execute(ctx, q); err != nil {
		r.logger.Warn("planner reactive loop: recordEvaluation failed",
			"responsibilityId", respId, "error", err)
	}
}

// --- C4: convergence -------------------------------------------------------

// maybeConverge runs the convergence reasoning step for one user, gated
// by the per-user convergence throttle. Assembles the user's space goals
// + active responsibilities + recent memory and runs the
// reactiveConductor prompt, then dispatches each emitted proactive
// action (under the quiet/dedup guard). Stays silent when the conductor
// returns no actions.
func (r *ReactiveLoop) maybeConverge(ctx context.Context, userId string, responsibilities []map[string]any, now time.Time) {
	if !r.shouldConverge(userId, now) {
		return
	}
	r.markConverged(userId, now)

	goals := r.loadSpaceGoals(ctx, userId)
	memory := r.loadRecentMemory(ctx)

	// Nothing to reason over -> stay silent without an LLM call.
	if len(goals) == 0 && len(responsibilities) == 0 {
		return
	}

	resp, err := r.engine.InvokeSI(ctx, "reactiveConductor", map[string]any{
		"user":             map[string]any{"userId": userId},
		"spaceGoals":       goals,
		"responsibilities": compactResponsibilities(responsibilities),
		"recentMemory":     memory,
		"now":              now.Format(time.RFC3339),
	})
	if err != nil {
		r.logger.Debug("planner reactive loop: convergence invoke failed",
			"userId", userId, "error", err)
		return
	}
	decision, err := parseConvergence(resp)
	if err != nil {
		r.logger.Debug("planner reactive loop: convergence parse failed",
			"userId", userId, "error", err)
		return
	}
	if len(decision.Actions) == 0 {
		r.logger.Debug("planner reactive loop: convergence silent",
			"userId", userId, "silentReason", decision.SilentReason)
		return
	}
	for _, a := range decision.Actions {
		r.dispatchConvergenceAction(ctx, userId, a, now)
	}
}

// dispatchConvergenceAction applies one proactive action emitted by the
// conductor, under the confidence + dedup guards. Low-confidence or
// recently-acted actions are dropped silently (no spam).
func (r *ReactiveLoop) dispatchConvergenceAction(ctx context.Context, userId string, a convergenceAction, now time.Time) {
	if a.Confidence < 0.6 {
		return
	}
	// Dedup key: prefer the responsibility id when the action targets
	// one, else the statement, so a repeated nudge for the same thing is
	// throttled by the respawn guard.
	dedupKey := "converge:" + userId + ":" + a.ResponsibilityId
	if a.ResponsibilityId == "" {
		dedupKey = "converge:" + userId + ":" + truncate(a.Statement, 64)
	}
	if !r.shouldHonor(dedupKey, now) {
		return
	}

	switch a.Kind {
	case "createPlan":
		row := map[string]any{
			"id":           a.ResponsibilityId,
			"statement":    a.Statement,
			"trigger":      "reactive",
			"scopeSpaceId": "",
		}
		// Route through the same Plan path; resolve the agent loosely (GA
		// or the responsibility's agent). For a goal-driven plan with no
		// responsibility we hand it to the GA.
		agentId := r.resolveAssistant(ctx, userId)
		if _, err := r.createResponsibilityPlan(ctx, userId, row, agentId); err != nil {
			r.logger.Warn("planner reactive loop: convergence createPlan failed",
				"userId", userId, "error", err)
			return
		}
	case "surfaceNudge":
		// A nudge is a lightweight surface; we record it as a Plan-free
		// signal by stamping lastResult on the responsibility when one is
		// named (so the UI's last-output widget shows it), else log it.
		// (A canvas-card surface is the richer path; kept minimal here --
		// see deliberate simplifications in the PR.)
		if a.ResponsibilityId != "" {
			r.recordEvaluation(ctx, a.ResponsibilityId, "nudge: "+a.Statement)
		} else {
			r.logger.Info("planner reactive loop: convergence nudge",
				"userId", userId, "statement", a.Statement)
		}
	case "extendSpecialist":
		// Treat as a routing re-run for the named responsibility: a
		// minimal specialist row whose factory pass extends the existing
		// agent toward the capability gap in the statement.
		if a.ResponsibilityId != "" {
			row := map[string]any{
				"id":          a.ResponsibilityId,
				"statement":   a.Statement,
				"targetKind":  "unassigned",
				"trigger":     "standing",
			}
			if _, err := r.routeResponsibility(ctx, userId, row); err != nil {
				r.logger.Warn("planner reactive loop: convergence extendSpecialist failed",
					"userId", userId, "responsibilityId", a.ResponsibilityId, "error", err)
				return
			}
		}
	default:
		r.logger.Debug("planner reactive loop: convergence unknown action",
			"userId", userId, "kind", a.Kind)
		return
	}
	r.markHonored(dedupKey, now)
	r.logger.Info("planner reactive loop: convergence action dispatched",
		"userId", userId, "kind", a.Kind, "responsibilityId", a.ResponsibilityId,
		"confidence", a.Confidence)
}

// loadSpaceGoals pulls the user's active spaces and projects each one's
// goal sub-object (#635 lives as v1:cognition:space.goal). Empty for a
// user with no goal-bearing spaces.
func (r *ReactiveLoop) loadSpaceGoals(ctx context.Context, userId string) []map[string]any {
	q := fmt.Sprintf(`queryActiveSpaces({userId:%q})`, userId)
	res, err := r.engine.Execute(ctx, q)
	if err != nil {
		r.logger.Debug("planner reactive loop: loadSpaceGoals failed",
			"userId", userId, "error", err)
		return nil
	}
	out := []map[string]any{}
	for _, sp := range memql.MaterializeRows(res) {
		goal, _ := sp["goal"].(map[string]any)
		statement := strings.TrimSpace(getString(goal, "statement"))
		if statement == "" {
			continue
		}
		out = append(out, map[string]any{
			"spaceId":   getString(sp, "id"),
			"name":      getString(sp, "name"),
			"statement": statement,
			"timeframe": getString(goal, "timeframe"),
		})
	}
	return out
}

// loadRecentMemory pulls top-k recent + relevant memories via recall().
// Owner-scoped inside the builtin (it reads the actor from ctx), so the
// impersonated owner ctx is what makes this the right user's memory.
// Best-effort -- recall lives on the harness integration which may not
// be wired on every planner binary; a failure yields empty memory and
// the conductor reasons without it.
func (r *ReactiveLoop) loadRecentMemory(ctx context.Context) []map[string]any {
	q := fmt.Sprintf(`recall({text:%q, k:%d})`,
		"active goals and standing responsibilities I am pursuing", recallTopK)
	res, err := r.engine.Execute(ctx, q)
	if err != nil {
		r.logger.Debug("planner reactive loop: recall failed (harness likely unwired)",
			"error", err)
		return nil
	}
	rows := memql.MaterializeRows(res)
	out := make([]map[string]any, 0, len(rows))
	for _, m := range rows {
		entry := map[string]any{
			"content":   getString(m, "content"),
			"createdAt": getString(m, "createdAt"),
		}
		out = append(out, entry)
	}
	return out
}

// --- guards ----------------------------------------------------------------

func (r *ReactiveLoop) shouldHonor(key string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	last, ok := r.lastHonored[key]
	if !ok {
		return true
	}
	return now.Sub(last) > honorRespawnGuard
}

func (r *ReactiveLoop) markHonored(key string, now time.Time) {
	r.mu.Lock()
	r.lastHonored[key] = now
	r.mu.Unlock()
}

func (r *ReactiveLoop) shouldConverge(userId string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	last, ok := r.lastConverged[userId]
	if !ok {
		return true
	}
	return now.Sub(last) > convergenceGuard
}

func (r *ReactiveLoop) markConverged(userId string, now time.Time) {
	r.mu.Lock()
	r.lastConverged[userId] = now
	r.mu.Unlock()
}

// --- loaders ---------------------------------------------------------------

func (r *ReactiveLoop) loadAgent(ctx context.Context, agentId string) map[string]any {
	q := fmt.Sprintf(`queryAgentById({agentId:%q})`, agentId)
	res, err := r.engine.Execute(ctx, q)
	if err != nil {
		return nil
	}
	rows := memql.MaterializeRows(res)
	if len(rows) == 0 {
		return nil
	}
	return rows[0]
}
