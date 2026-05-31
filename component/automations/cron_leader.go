package automations

import (
	"context"
	"database/sql"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component"
	"github.com/znasllc-io/memql/core/common"
)

// CronLeader elects a single cluster-wide owner of SCHEDULED (cron)
// automations via a Postgres session-level advisory lock (znasllc-io/memql#561).
//
// Why: every memQL node runs the automation scheduler, and a cron entry
// fires independently on each one -- so the daily purges + the */5min
// feedback sweep run once PER NODE TYPE today, and would run once per
// REPLICA too. Event-triggered automations are unaffected (the mesh routes
// each event to one pod), so only the schedule path needs gating.
//
// Mechanism: each node holds one dedicated DB connection and calls
// pg_try_advisory_lock(key) on it. Exactly one node acquires the lock and
// becomes the leader; the scheduler only executes cron firings on the
// leader (see Scheduler.LeaderGate). The lock is session-scoped, so if the
// leader pod dies its connection drops and Postgres releases the lock
// automatically -- another node's poll then takes over (failover). When no
// DB is reachable the node is simply not leader (fail-closed: better to
// skip a maintenance cron than double-run a non-idempotent one).
const (
	CronLeaderComponentName = common.ComponentName("automationCronLeader")

	// cronLeaderLockKey is an arbitrary fixed 64-bit advisory-lock id for the
	// "owner of scheduled automations" lease. Must not collide with any other
	// advisory lock the app takes (the bun migrator uses its own keyspace).
	cronLeaderLockKey int64 = 7756010113207010561

	cronLeaderPollInterval = 10 * time.Second
)

// CronLeader is a Dependency; it acquires/holds the lease over its lifetime.
type CronLeader struct {
	*component.Component

	dbGetter func() *bun.DB
	logger   *slog.Logger

	leader atomic.Bool

	mu   sync.Mutex // guards conn
	conn *sql.Conn
}

// NewCronLeader builds the leader. dbGetter is resolved lazily (the DB may
// not be Ready at construction). A nil logger is tolerated.
func NewCronLeader(dbGetter func() *bun.DB, logger *slog.Logger) *CronLeader {
	comp, _ := component.New(CronLeaderComponentName)
	cl := &CronLeader{Component: comp, dbGetter: dbGetter, logger: logger}
	_ = cl.ConfigureLifecycle(
		component.WithRunHook(cl.run),
		component.WithOnStopHook(cl.cleanup),
	)
	return cl
}

// IsLeader reports whether this node currently owns the scheduled-automation
// lease. Wire it as Scheduler.LeaderGate.
func (cl *CronLeader) IsLeader() bool { return cl.leader.Load() }

func (cl *CronLeader) run(ctx context.Context, markStarted func()) error {
	cl.poll(ctx) // first attempt before signalling Ready so a single-node
	markStarted() // cluster is already leader by the time crons can fire
	t := time.NewTicker(cronLeaderPollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			cl.poll(ctx)
		}
	}
}

// poll acquires the lease (non-leader) or keepalives it (leader), dropping
// leadership if the connection dies.
func (cl *CronLeader) poll(ctx context.Context) {
	conn, err := cl.ensureConn(ctx)
	if err != nil {
		cl.demote("no database connection", err)
		return
	}

	if cl.leader.Load() {
		// Keepalive: if the lock-holding connection is dead, drop the lease
		// so a healthy node can take over.
		if err := conn.PingContext(ctx); err != nil {
			cl.demote("lease connection lost", err)
		}
		return
	}

	var acquired bool
	if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", cronLeaderLockKey).Scan(&acquired); err != nil {
		cl.demote("advisory-lock query failed", err)
		return
	}
	if acquired {
		cl.leader.Store(true)
		if cl.logger != nil {
			cl.logger.Info("cron leader acquired -- this node runs scheduled automations")
		}
	}
}

func (cl *CronLeader) ensureConn(ctx context.Context) (*sql.Conn, error) {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	if cl.conn != nil {
		return cl.conn, nil
	}
	db := cl.dbGetter()
	if db == nil || db.DB == nil {
		return nil, sql.ErrConnDone
	}
	conn, err := db.DB.Conn(ctx)
	if err != nil {
		return nil, err
	}
	cl.conn = conn
	return conn, nil
}

// demote drops leadership + the (likely dead) connection so the next poll
// re-acquires from scratch.
func (cl *CronLeader) demote(reason string, err error) {
	wasLeader := cl.leader.Swap(false)
	cl.closeConn()
	if wasLeader && cl.logger != nil {
		cl.logger.Warn("cron leadership released", "reason", reason, "error", err)
	}
}

func (cl *CronLeader) closeConn() {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	if cl.conn != nil {
		_ = cl.conn.Close()
		cl.conn = nil
	}
}

func (cl *CronLeader) cleanup() {
	cl.mu.Lock()
	conn := cl.conn
	cl.conn = nil
	cl.mu.Unlock()

	if conn != nil {
		if cl.leader.Load() {
			// Best-effort explicit unlock; closing the conn would release it
			// anyway, but unlock first so a co-located node takes over faster.
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_, _ = conn.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", cronLeaderLockKey)
			cancel()
		}
		_ = conn.Close()
	}
	cl.leader.Store(false)
}
