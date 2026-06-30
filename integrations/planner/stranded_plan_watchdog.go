// stranded_plan_watchdog.go
//
// The stranded-plan watchdog (memql#1389).
//
// A v1:planner:plan only starts via the graph.node.created event
// (PlannerAgentLoop.HandlePlanCreated). If that single event is lost --
// a cross-node mesh drop (cf. the peer-table decay incident, memql#1388)
// or a planner replica crash between receive and dispatch -- the Plan
// sits in planning/queued FOREVER with no retry and no user-visible
// failure. Tonight's staging incident stranded a produceArtifact plan
// exactly this way after Sofia had acknowledged it.
//
// This watchdog is the safety net. It runs as a ticker poller inside the
// planner integration (the same node that owns the agent loop), mirroring
// RefreshCron / FairnessCron:
//
//   - Polls strandedCandidatePlans (status in [planning, queued]).
//   - Applies the precise "older than the strand threshold" check Go-side
//     (the MemQL comparison evaluator can't do createdAt-vs-now()
//     arithmetic; cf. dueTrainAgentRetryPlans).
//   - Skips the kinds the agent loop never owns (trainSpecialist /
//     embedDomainItems / adHocAction / scopeElevation / agentInvocation):
//     those have their own dispatchers, and re-driving them through
//     invokeAndDispatch would be wrong.
//   - Re-enters PlannerAgentLoop.RecoverStranded for the rest, which
//     routes through the SAME per-plan once-guard (memql#1155) the
//     created-event path uses -- so a late created event AND the watchdog
//     never double-run on one replica.
//
// Effectively: plan dispatch becomes AT-LEAST-ONCE -- the created event is
// the fast path, the watchdog is the safety net.
//
// MULTI-NODE SAFETY (the cluster is the default runtime). The candidate
// query result reaches EVERY planner replica, so without a cross-replica
// gate each replica would re-drive the same stranded Plan and feed the
// exact duplicate-dispatch storm memql#1363 fixed for the approved-plan
// path. The watchdog therefore claims each recovery through the SAME
// ClusterExecutionGuard (the clusterClaimer wired in app/, memql#1363)
// BEFORE calling RecoverStranded -- exactly one replica recovers a given
// Plan in a given strand window. When no claimer is wired (dev /
// single-binary / unit tests) the in-process once-guard still bounds it
// to one recovery per process, the pre-#1363 behavior.

package planner

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/env"
)

const (
	// defaultStrandThresholdSeconds is how old (by createdAt) a Plan in
	// planning/queued must be before the watchdog treats it as stranded
	// and re-drives it. The created-event fast path dispatches within
	// seconds; a healthy plan transitions out of planning/queued long
	// before this. 5 minutes is comfortably past any normal decompose
	// latency (the per-plan LLM budget + iteration cap bound a live
	// dispatch well under a minute) while still recovering a dropped
	// event promptly. Tunable via MEMQL_PLANNER_WATCHDOG_STRAND_SECONDS.
	defaultStrandThresholdSeconds = 300

	// defaultWatchdogPollSeconds is how often the watchdog wakes to scan
	// for stranded Plans. 60s matches the training retry poller; frequent
	// enough that a dropped event is recovered within ~1 strand-threshold
	// window without hammering the query. Tunable via
	// MEMQL_PLANNER_WATCHDOG_POLL_SECONDS.
	defaultWatchdogPollSeconds = 60

	// watchdogStartupDelay defers the first scan so the engine + DSL
	// finish loading and any in-flight created events get a chance to
	// dispatch normally before the safety net considers anything stranded.
	watchdogStartupDelay = 90 * time.Second

	// strandedRecoveryClaimName is the cross-replica claim namespace for
	// watchdog recoveries (memql#1389). Rows land in the same
	// automation_execution_claims ledger the automation cluster guard +
	// the approved-plan executor (memql#1363) use. Distinct from
	// plannerPlanExecution so a watchdog recovery and a later legitimate
	// approved-plan dispatch of the same Plan don't collide on one key.
	strandedRecoveryClaimName = "plannerStrandedRecovery"

	// strandedRespawnGuard suppresses re-recovering the same Plan id for
	// this window after a recovery attempt on THIS replica, covering the
	// gap between re-driving the Plan and it transitioning out of
	// planning/queued (or to a terminal/parked status). Generous so a
	// slow decompose run doesn't get a duplicate recovery queued behind
	// it. The cross-replica claim + the once-guard are the hard dedup;
	// this just trims redundant work + log noise on one replica.
	strandedRespawnGuard = 15 * time.Minute
)

// watchdogRecoverableKind reports whether the agent loop owns dispatch for
// a Plan of this kind -- i.e. whether the watchdog should re-drive it
// through RecoverStranded. Mirrors the kind exclusions in
// PlannerAgentLoop.HandlePlanCreated: the dispatcher-owned kinds
// (trainSpecialist / embedDomainItems) and the non-agent-loop kinds
// (adHocAction / scopeElevation / agentInvocation) are handled elsewhere
// and must NOT be re-entered into invokeAndDispatch. Everything else
// (userGoal, produceArtifact, refineAnalysis, ...) flows through the loop.
func watchdogRecoverableKind(kind string) bool {
	switch kind {
	case "trainSpecialist", "embedDomainItems", "adHocAction", "scopeElevation", "agentInvocation":
		return false
	default:
		return true
	}
}

// watchdogConfig is the tunable policy, resolved from env once at
// construction. Default-ON: this is a data-loss safety net, not an
// optional feature, so it runs unless explicitly disabled.
type watchdogConfig struct {
	enabled   bool
	threshold time.Duration
	poll      time.Duration
}

func loadWatchdogConfig() watchdogConfig {
	reader := env.NewEnvReader("")
	cfg := watchdogConfig{
		enabled:   true,
		threshold: time.Duration(defaultStrandThresholdSeconds) * time.Second,
		poll:      time.Duration(defaultWatchdogPollSeconds) * time.Second,
	}
	if b, err := reader.OptionalBool("MEMQL_PLANNER_WATCHDOG_ENABLED"); err == nil && b != nil {
		cfg.enabled = *b
	}
	if ptr, err := reader.OptionalInt("MEMQL_PLANNER_WATCHDOG_STRAND_SECONDS"); err == nil && ptr != nil && *ptr > 0 {
		cfg.threshold = time.Duration(*ptr) * time.Second
	}
	if ptr, err := reader.OptionalInt("MEMQL_PLANNER_WATCHDOG_POLL_SECONDS"); err == nil && ptr != nil && *ptr > 0 {
		cfg.poll = time.Duration(*ptr) * time.Second
	}
	return cfg
}

// StrandedPlanWatchdog polls strandedCandidatePlans and re-drives
// Plans whose plan-created event was lost (memql#1389).
type StrandedPlanWatchdog struct {
	engine    Engine
	agentLoop *PlannerAgentLoop
	// claimer is the cross-replica recovery gate (memql#1363 guard). When
	// nil only the in-process once-guard + respawn guard apply.
	claimer ExecutionClaimer
	logger  *slog.Logger
	cfg     watchdogConfig

	cancel context.CancelFunc

	mu          sync.Mutex
	lastRecover map[string]time.Time // planId -> when we last attempted recovery (this replica)
	startedOnce sync.Once
}

// NewStrandedPlanWatchdog constructs the watchdog pinned to the planner
// integration's engine + agent loop. claimer is the cross-replica claim
// (optional; nil leaves only in-process dedup).
func NewStrandedPlanWatchdog(engine Engine, agentLoop *PlannerAgentLoop, claimer ExecutionClaimer, logger *slog.Logger) *StrandedPlanWatchdog {
	if logger == nil {
		logger = slog.Default()
	}
	return &StrandedPlanWatchdog{
		engine:      engine,
		agentLoop:   agentLoop,
		claimer:     claimer,
		logger:      logger,
		cfg:         loadWatchdogConfig(),
		lastRecover: make(map[string]time.Time),
	}
}

// Start kicks off the poll goroutine. Idempotent -- repeated calls are
// no-ops after the first (the integration's Start can fire more than
// once). A no-op when disabled or when its dependencies are missing.
func (w *StrandedPlanWatchdog) Start(ctx context.Context) {
	if w == nil {
		return
	}
	if !w.cfg.enabled {
		w.logger.Info("planner stranded-plan watchdog: disabled (MEMQL_PLANNER_WATCHDOG_ENABLED=false)")
		return
	}
	if w.engine == nil || w.agentLoop == nil {
		w.logger.Warn("planner stranded-plan watchdog: missing engine or agent loop; not starting")
		return
	}
	w.startedOnce.Do(func() {
		runCtx, cancel := context.WithCancel(context.Background())
		w.mu.Lock()
		w.cancel = cancel
		w.mu.Unlock()
		_ = ctx // parent ctx is informational; the watchdog runs for the
		// process lifetime like the refresh / fairness pollers.
		go w.loop(runCtx)
	})
}

// Stop cancels the poll goroutine.
func (w *StrandedPlanWatchdog) Stop() {
	if w == nil {
		return
	}
	w.mu.Lock()
	cancel := w.cancel
	w.cancel = nil
	w.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (w *StrandedPlanWatchdog) loop(ctx context.Context) {
	w.logger.Info("planner stranded-plan watchdog: started",
		"pollInterval", w.cfg.poll.String(),
		"strandThreshold", w.cfg.threshold.String())
	ticker := time.NewTicker(w.cfg.poll)
	defer ticker.Stop()
	startup := time.NewTimer(watchdogStartupDelay)
	defer startup.Stop()
	for {
		select {
		case <-ctx.Done():
			w.logger.Info("planner stranded-plan watchdog: stopped")
			return
		case <-startup.C:
			w.pollOnce(ctx)
		case <-ticker.C:
			w.pollOnce(ctx)
		}
	}
}

// pollOnce scans for stranded Plans, applies the Go-side age + dedup
// checks, claims each recovery cross-replica, and re-drives it.
func (w *StrandedPlanWatchdog) pollOnce(ctx context.Context) {
	if w.engine == nil || w.agentLoop == nil {
		return
	}
	res, err := w.engine.Execute(systemActorContext(ctx), `query strandedCandidatePlans()`)
	if err != nil {
		w.logger.Debug("planner stranded-plan watchdog: query failed (engine likely not ready)", "error", err)
		return
	}
	rows := memql.MaterializeRows(res)
	if len(rows) == 0 {
		return
	}
	now := time.Now().UTC()
	recovered := 0
	for _, row := range rows {
		planId := getString(row, "id")
		if planId == "" {
			continue
		}
		kind := getString(row, "kind")
		if !watchdogRecoverableKind(kind) {
			// Owned by a dedicated dispatcher / the scope-elevation path;
			// the watchdog never re-drives these through the agent loop.
			continue
		}
		if !w.isStranded(row, now) {
			continue
		}
		if !w.shouldRecover(planId, now) {
			continue
		}
		// Cross-replica claim BEFORE any recovery side effect (memql#1363).
		// The candidate query result reaches every replica; the claim makes
		// exactly one replica recover this Plan in this strand window. Key on
		// the bare planId: a stranded Plan has no startedAt yet, and we want
		// at most one recovery for it. A later legitimate approved-plan
		// dispatch claims under the distinct plannerPlanExecution name, so
		// this recovery claim never blocks it.
		if w.claimer != nil {
			if !w.claimer.Claim(ctx, strandedRecoveryClaimName, planId) {
				w.logger.Debug("planner stranded-plan watchdog: another replica is recovering this plan; skipping",
					"planId", planId, "node", claimNodeName())
				// Mark locally so we don't re-probe the claim every poll while
				// the winning replica drives it.
				w.markRecovered(planId, now)
				continue
			}
		}
		w.markRecovered(planId, now)
		if w.agentLoop.RecoverStranded(ctx, planId, kind) {
			recovered++
		}
	}
	if recovered > 0 {
		w.logger.Info("planner stranded-plan watchdog: recovered stranded plans",
			"count", recovered, "candidates", len(rows))
	}
}

// isStranded reports whether a planning/queued Plan is old enough (by
// createdAt) to be considered stranded. A Plan with an unparseable /
// missing createdAt is treated as NOT stranded -- we'd rather leave it
// for the next poll than risk re-driving a brand-new row whose stamp we
// can't read.
func (w *StrandedPlanWatchdog) isStranded(row map[string]any, now time.Time) bool {
	created := parseTimeOrZero(getString(row, "createdAt"))
	if created.IsZero() {
		return false
	}
	return now.Sub(created) >= w.cfg.threshold
}

// shouldRecover returns true unless we attempted recovery for this Plan
// within the respawn-guard window on this replica. Trims redundant work +
// log noise between re-driving a Plan and it leaving planning/queued.
func (w *StrandedPlanWatchdog) shouldRecover(planId string, now time.Time) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	last, ok := w.lastRecover[planId]
	if !ok {
		return true
	}
	return now.Sub(last) > strandedRespawnGuard
}

func (w *StrandedPlanWatchdog) markRecovered(planId string, now time.Time) {
	w.mu.Lock()
	w.lastRecover[planId] = now
	// Opportunistic sweep so the map can't grow unbounded across a long
	// process lifetime: drop entries older than 2x the respawn guard.
	if len(w.lastRecover) > 256 {
		for k, v := range w.lastRecover {
			if now.Sub(v) > 2*strandedRespawnGuard {
				delete(w.lastRecover, k)
			}
		}
	}
	w.mu.Unlock()
}
