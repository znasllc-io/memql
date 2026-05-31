package automations

import (
	"context"
	"os"
	"sync/atomic"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component"
	"github.com/znasllc-io/memql/core/common"
)

// ClusterExecutionGuard makes EVENT-triggered automations exactly-once
// across the cluster, and makes any double-fire observable (#561).
//
// When a memQL node-type runs >=2 replicas, the same triggering event can
// reach more than one replica (the EventBridge dedup is per-process). Each
// replica calls Claim(automation, dedupKey) before executing; the claim
// inserts a row keyed by (automation_name, dedup_key) whose PRIMARY KEY lets
// exactly one replica win -- the others skip. dedupKey is the automation's
// InitialChainHead (a deterministic fingerprint of the triggering event), so
// the same event yields the same key on every replica.
//
// OBSERVABILITY -- the whole point of the safety net:
//   - every cross-replica duplicate that gets PREVENTED is WARN-logged with
//     the automation + key + node, and counted (DuplicatesPrevented). A
//     rising count proves events ARE reaching multiple replicas AND that we
//     are correctly collapsing them to one execution.
//   - a claim that can't reach the DB executes UNGUARDED (fail-open: never
//     drop legitimate work) but is WARN-logged + counted (ClaimErrors) so the
//     double-fire WINDOW is never silent.
//   - the claim rows themselves (claimed_by + claimed_at) are an audit trail:
//     a true double-fire is impossible at the DB (the PK rejects the second
//     insert), so any suspected double-fire is investigable from the table.
//   - a periodic summary line logs the running counts.
//
// Started as a Dependency: a background loop prunes expired claim rows and
// emits the summary.
const (
	ClusterGuardComponentName = common.ComponentName("automationClusterGuard")

	clusterGuardDefaultRetention = time.Hour
	clusterGuardPruneInterval    = 10 * time.Minute
)

// ClusterExecutionGuard implements executionClaimer.
type ClusterExecutionGuard struct {
	*component.Component

	dbGetter  func() *bun.DB
	nodeId    string
	logger    Logger
	retention time.Duration

	claimed   atomic.Int64
	prevented atomic.Int64
	errors    atomic.Int64
}

// Logger is the minimal logging surface the guard needs (satisfied by
// *slog.Logger). Kept as an interface so tests can inject a recorder.
type Logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
}

// NewClusterExecutionGuard builds the guard. dbGetter is resolved lazily.
func NewClusterExecutionGuard(dbGetter func() *bun.DB, logger Logger) *ClusterExecutionGuard {
	comp, _ := component.New(ClusterGuardComponentName)
	host, _ := os.Hostname()
	if host == "" {
		host = "unknown"
	}
	g := &ClusterExecutionGuard{
		Component: comp,
		dbGetter:  dbGetter,
		nodeId:    host,
		logger:    logger,
		retention: clusterGuardDefaultRetention,
	}
	_ = g.ConfigureLifecycle(component.WithRunHook(g.run))
	return g
}

// Claim returns true when THIS node should execute the automation (it won the
// claim, or the guard is degraded and fails open) and false when another
// replica already claimed it (a prevented cross-replica duplicate).
func (g *ClusterExecutionGuard) Claim(ctx context.Context, automationName, dedupKey string) bool {
	db := g.dbGetter()
	if db == nil || db.DB == nil {
		g.errors.Add(1)
		g.warn("automation execution claim skipped -- no database; executing UNGUARDED (possible double-fire window)",
			"automation", automationName)
		return true // fail-open: never drop legitimate work
	}

	res, err := db.DB.ExecContext(ctx,
		`INSERT INTO automation_execution_claims (automation_name, dedup_key, claimed_by)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (automation_name, dedup_key) DO NOTHING`,
		automationName, dedupKey, g.nodeId)
	if err != nil {
		g.errors.Add(1)
		g.warn("automation execution claim failed -- executing UNGUARDED (possible double-fire window)",
			"automation", automationName, "dedupKey", dedupKey, "error", err)
		return true // fail-open
	}

	n, _ := res.RowsAffected()
	if n == 0 {
		// Another replica already owns this (automation, event) -> duplicate.
		g.prevented.Add(1)
		g.warn("duplicate automation execution prevented by cluster guard",
			"automation", automationName, "dedupKey", dedupKey, "node", g.nodeId)
		return false
	}

	g.claimed.Add(1)
	g.debug("automation execution claimed", "automation", automationName, "node", g.nodeId)
	return true
}

// DuplicatesPrevented / ClaimErrors / Claimed expose the running counts for
// tests + health surfaces.
func (g *ClusterExecutionGuard) DuplicatesPrevented() int64 { return g.prevented.Load() }
func (g *ClusterExecutionGuard) ClaimErrors() int64         { return g.errors.Load() }
func (g *ClusterExecutionGuard) Claimed() int64             { return g.claimed.Load() }

func (g *ClusterExecutionGuard) run(ctx context.Context, markStarted func()) error {
	markStarted()
	t := time.NewTicker(clusterGuardPruneInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			g.prune(ctx)
			g.logSummary()
		}
	}
}

// prune deletes claim rows older than the retention window so the table stays
// small (the dedup window only needs to span concurrent deliveries).
func (g *ClusterExecutionGuard) prune(ctx context.Context) {
	db := g.dbGetter()
	if db == nil || db.DB == nil {
		return
	}
	cutoff := time.Now().Add(-g.retention)
	if _, err := db.DB.ExecContext(ctx,
		`DELETE FROM automation_execution_claims WHERE claimed_at < $1`, cutoff); err != nil {
		g.warn("automation cluster guard: prune failed", "error", err)
	}
}

// logSummary emits the running counts -- the at-a-glance double-fire signal.
func (g *ClusterExecutionGuard) logSummary() {
	prevented := g.prevented.Load()
	level := g.info
	if prevented > 0 {
		// Duplicates being prevented is healthy (the guard working), but worth
		// surfacing at WARN so it's visible that multi-replica delivery is live.
		level = g.warn
	}
	level("automation cluster guard summary",
		"node", g.nodeId,
		"claimed", g.claimed.Load(),
		"duplicatesPrevented", prevented,
		"claimErrors", g.errors.Load())
}

func (g *ClusterExecutionGuard) debug(msg string, args ...any) {
	if g.logger != nil {
		g.logger.Debug(msg, args...)
	}
}
func (g *ClusterExecutionGuard) info(msg string, args ...any) {
	if g.logger != nil {
		g.logger.Info(msg, args...)
	}
}
func (g *ClusterExecutionGuard) warn(msg string, args ...any) {
	if g.logger != nil {
		g.logger.Warn(msg, args...)
	}
}
