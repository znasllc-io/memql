// fairness.go
//
// Fairness / anti-starvation for the tenant task queue (memql#908, epic
// #902). Tasks may run for days or months -- the constraint is concurrency,
// not duration -- so a long-running plan can sit on a slot indefinitely
// while other plans wait. The admission core (#904) + FIFO queue (#905)
// admit the next waiter when a slot FREES, but nothing frees a slot from a
// long holder. This sweep does: when an account's waiting queue is
// non-empty and a running plan has held its slot past a hysteresis floor,
// it PASSES (preempts) the longest-held holder -- marks it `paused`, which
// #905's handleSlotFreed turns into admit-next for the oldest waiter. The
// passed plan resumes later (memql#907) when a slot is free again.
//
// Safe on main without the cross-node pass envelope (Session B's
// feat/906-planner-pass): marking the holder `paused` frees the slot via
// the DB-backed status transition, and the markPlanSucceeded/markPlanFailed
// guard keeps a still-in-flight turn from overwriting `paused` with a
// terminal status. Best-effort early agent-stop (RequestPause, #906)
// composes on top once that envelope lands -- until then the holder's turn
// finishes on its own wallclock and its result is discarded by the guard.
//
// DEFAULT OFF: enable via MEMQL_TASK_FAIRNESS_ENABLED. Unconfigured /
// unlimited-tier accounts never have a waiting queue, so even when enabled
// the sweep is a no-op until a finite cap produces real contention.
package planner

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/core/env"
)

const (
	// defaultFairnessMinRunSeconds is the hysteresis floor: a running plan
	// must have held its slot at least this long before it can be passed for
	// fairness. Prevents thrash (passing a plan moments after it started) and
	// gives short tasks room to just finish. 30 min default.
	defaultFairnessMinRunSeconds = 1800
	// defaultFairnessSweepSeconds is how often the sweep wakes.
	defaultFairnessSweepSeconds = 300
)

// fairnessConfig is the tunable policy, resolved from env once at construction.
type fairnessConfig struct {
	enabled bool
	minRun  time.Duration
	sweep   time.Duration
}

func loadFairnessConfig() fairnessConfig {
	reader := env.NewEnvReader("")
	cfg := fairnessConfig{
		enabled: false,
		minRun:  time.Duration(defaultFairnessMinRunSeconds) * time.Second,
		sweep:   time.Duration(defaultFairnessSweepSeconds) * time.Second,
	}
	if b, err := reader.OptionalBool("MEMQL_TASK_FAIRNESS_ENABLED"); err == nil && b != nil {
		cfg.enabled = *b
	}
	if ptr, err := reader.OptionalInt("MEMQL_TASK_FAIRNESS_MIN_RUN_SECONDS"); err == nil && ptr != nil && *ptr > 0 {
		cfg.minRun = time.Duration(*ptr) * time.Second
	}
	if ptr, err := reader.OptionalInt("MEMQL_TASK_FAIRNESS_SWEEP_SECONDS"); err == nil && ptr != nil && *ptr > 0 {
		cfg.sweep = time.Duration(*ptr) * time.Second
	}
	return cfg
}

// runningSlot is a running plan holding a concurrency slot, with when it
// started running (for slot-hold age).
type runningSlot struct {
	PlanId    string
	StartedAt time.Time
}

// selectFairnessVictim picks the running plan to pass for fairness, or
// ("", false) when no pass is warranted. A pass is warranted only when the
// account has waiters AND at least one running plan has held its slot beyond
// minRun; among those, the LONGEST-held (oldest StartedAt) is chosen so the
// biggest hog yields first. At most one victim per account per sweep
// (returned here) bounds churn. Pure + deterministic for unit testing.
func selectFairnessVictim(running []runningSlot, waitingCount int, now time.Time, minRun time.Duration) (string, bool) {
	if waitingCount <= 0 || len(running) == 0 {
		return "", false
	}
	var victim string
	var oldest time.Time
	for _, r := range running {
		if r.PlanId == "" || r.StartedAt.IsZero() {
			continue
		}
		if now.Sub(r.StartedAt) < minRun {
			continue // still within the hysteresis floor
		}
		if victim == "" || r.StartedAt.Before(oldest) {
			victim, oldest = r.PlanId, r.StartedAt
		}
	}
	return victim, victim != ""
}

// FairnessCron is the periodic sweep. Mirrors RefreshCron's ticker shape.
type FairnessCron struct {
	engine      Engine
	dbGetter    func() *bun.DB
	logger      *slog.Logger
	cfg         fairnessConfig
	now         func() time.Time
	cancel      context.CancelFunc
	startedOnce sync.Once
}

// NewFairnessCron builds the sweep over the engine (for the pause mutation)
// and the lazy *bun.DB getter (for the across-account read SQL).
func NewFairnessCron(engine Engine, dbGetter func() *bun.DB, logger *slog.Logger) *FairnessCron {
	if logger == nil {
		logger = slog.Default()
	}
	return &FairnessCron{
		engine:   engine,
		dbGetter: dbGetter,
		logger:   logger,
		cfg:      loadFairnessConfig(),
		now:      time.Now,
	}
}

// Start launches the sweep loop unless fairness is disabled or deps are
// missing. Idempotent.
func (c *FairnessCron) Start(ctx context.Context) {
	if !c.cfg.enabled {
		c.logger.Info("planner fairness cron: disabled (MEMQL_TASK_FAIRNESS_ENABLED unset)")
		return
	}
	if c.engine == nil || c.dbGetter == nil {
		c.logger.Warn("planner fairness cron: engine or dbGetter not configured; not starting")
		return
	}
	c.startedOnce.Do(func() {
		runCtx, cancel := context.WithCancel(context.Background())
		c.cancel = cancel
		_ = ctx
		go c.run(runCtx)
		c.logger.Info("planner fairness cron: started",
			"sweepInterval", c.cfg.sweep.String(),
			"minRunBeforePass", c.cfg.minRun.String(),
		)
	})
}

// Stop cancels the sweep loop.
func (c *FairnessCron) Stop() {
	if c.cancel != nil {
		c.cancel()
	}
}

func (c *FairnessCron) run(ctx context.Context) {
	ticker := time.NewTicker(c.cfg.sweep)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			c.logger.Info("planner fairness cron: stopped")
			return
		case <-ticker.C:
			c.sweep(ctx)
		}
	}
}

// sweep runs one fairness pass across every account that currently has a
// non-empty waiting queue.
func (c *FairnessCron) sweep(ctx context.Context) {
	waiters, err := c.accountsWithWaiters(ctx)
	if err != nil {
		c.logger.Debug("planner fairness cron: accountsWithWaiters failed (db likely not ready)", "error", err)
		return
	}
	now := c.now()
	for acct, waitingCount := range waiters {
		running, err := c.runningSlotsForAccount(ctx, acct)
		if err != nil {
			c.logger.Warn("planner fairness cron: runningSlotsForAccount failed", "account_id", acct, "error", err)
			continue
		}
		victim, ok := selectFairnessVictim(running, waitingCount, now, c.cfg.minRun)
		if !ok {
			continue
		}
		c.passForFairness(ctx, acct, victim, waitingCount)
	}
}

// passForFairness marks the victim plan paused, which frees its slot:
// #905's handleSlotFreed observes the `paused` transition and admits the
// oldest waiter. Best-effort -- a failed pause just means we try again next
// sweep.
func (c *FairnessCron) passForFairness(ctx context.Context, accountId, planId string, waitingCount int) {
	nowStr := c.now().UTC().Format(time.RFC3339)
	q := fmt.Sprintf(`mutationUpdatePlanStatus({planId:%q, status:"paused", pausedAt:%q})`, planId, nowStr)
	if _, err := c.engine.Execute(contextWithSystemActor(ctx), q); err != nil {
		c.logger.Warn("planner fairness cron: pass (mark paused) failed", "plan_id", planId, "account_id", accountId, "error", err)
		return
	}
	c.logger.Info("planner fairness cron: passed a long-held plan to free a slot for the queue",
		"plan_id", planId,
		"account_id", accountId,
		"waiting", waitingCount,
		"minRunBeforePass", c.cfg.minRun.String(),
	)
}

// --- across-account read SQL (mirrors admission.go's latest-version CTE) ---

const fairnessAccountsWithWaitersQuery = `WITH latest AS (
    SELECT DISTINCT ON (id) id, payload
    FROM "MemoryNodes"
    WHERE concept = 'v1:planner:plan'
    ORDER BY id, "createdAt" DESC
)
SELECT payload->>'requestedBy' AS acct, count(*) AS n FROM latest
WHERE payload->>'status' = 'waitingForSlot'
  AND coalesce(payload->>'requestedBy', '') <> ''
GROUP BY acct`

const fairnessRunningSlotsQuery = `WITH latest AS (
    SELECT DISTINCT ON (id) id, payload
    FROM "MemoryNodes"
    WHERE concept = 'v1:planner:plan'
    ORDER BY id, "createdAt" DESC
)
SELECT id, payload->>'startedAt' AS started FROM latest
WHERE payload->>'status' = 'running'
  AND payload->>'requestedBy' = $1`

// accountsWithWaiters returns accountId -> waiting-plan count for every
// account that currently has at least one waitingForSlot plan.
func (c *FairnessCron) accountsWithWaiters(ctx context.Context) (map[string]int, error) {
	db := c.dbGetter()
	if db == nil {
		return nil, fmt.Errorf("fairness: db unavailable")
	}
	rows, err := db.DB.QueryContext(ctx, fairnessAccountsWithWaitersQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]int)
	for rows.Next() {
		var acct string
		var n int
		if err := rows.Scan(&acct, &n); err != nil {
			return nil, err
		}
		out[acct] = n
	}
	return out, rows.Err()
}

// runningSlotsForAccount returns the account's currently-running plans with
// their startedAt parsed. Rows with a missing / unparseable startedAt get a
// zero StartedAt and are skipped by selectFairnessVictim.
func (c *FairnessCron) runningSlotsForAccount(ctx context.Context, accountId string) ([]runningSlot, error) {
	db := c.dbGetter()
	if db == nil {
		return nil, fmt.Errorf("fairness: db unavailable")
	}
	rows, err := db.DB.QueryContext(ctx, fairnessRunningSlotsQuery, accountId)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []runningSlot
	for rows.Next() {
		var id, started string
		if err := rows.Scan(&id, &started); err != nil {
			return nil, err
		}
		slot := runningSlot{PlanId: id}
		if t, perr := time.Parse(time.RFC3339, started); perr == nil {
			slot.StartedAt = t
		}
		out = append(out, slot)
	}
	return out, rows.Err()
}
