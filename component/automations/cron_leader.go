package automations

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/binary"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/component"
)

// CronLeader elects a single cluster-wide owner of SCHEDULED (cron)
// automations via a Postgres session-level advisory lock (znasllc-io/memql#561).
//
// Why: every MemQL node runs the automation scheduler, and a cron entry
// fires independently on each one -- so the daily purges + the */5min
// feedback sweep run once PER NODE TYPE today, and would run once per
// REPLICA too. Event-triggered automations are unaffected (the mesh routes
// each event to one pod), so only the schedule path needs gating.
//
// Mechanism: each node holds one dedicated DB connection (resolved via the
// injected dbGetter -- wired to the DIRECT, non-pooled endpoint in app/, since
// the lock is held for the leader's lifetime and a transaction-mode pooler
// would recycle the backend out from under it, memql#1930) and calls
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
	cronLeaderPollTimeout  = 2 * time.Second
)

// CronLeader is a Dependency; it acquires/holds the lease over its lifetime.
type CronLeader struct {
	*component.Component

	dbGetter func() *bun.DB
	logger   *slog.Logger
	lockKey  int64

	leader atomic.Bool

	mu     sync.Mutex // serializes polling, scope registration and shutdown
	conn   *sql.Conn
	leases map[int64]*atomic.Bool
}

// NewCronLeader builds the leader. dbGetter is resolved lazily (the DB may
// not be Ready at construction). A nil logger is tolerated.
func NewCronLeader(dbGetter func() *bun.DB, logger *slog.Logger) *CronLeader {
	return newCronLeader(dbGetter, logger, CronLeaderComponentName, cronLeaderLockKey)
}

// NewScopedCronLeader elects one runner independently of the general cron
// lease. The composition layer decides which nodes may compete for a scope.
// Replicas using the same scope share a lease; distinct scopes do not block
// one another. It uses the same direct connection and failover contract.
func NewScopedCronLeader(scope string, dbGetter func() *bun.DB, logger *slog.Logger) *CronLeader {
	return newCronLeader(dbGetter, logger, common.ComponentName(string(CronLeaderComponentName)+"-"+scope), scopedCronKey(scope))
}

func scopedCronKey(scope string) int64 {
	digest := sha256.Sum256([]byte("memql:scheduled-automation:" + scope))
	return int64(binary.BigEndian.Uint64(digest[:8]))
}

func newCronLeader(dbGetter func() *bun.DB, logger *slog.Logger, name common.ComponentName, key int64) *CronLeader {
	comp, _ := component.New(name)
	cl := &CronLeader{Component: comp, dbGetter: dbGetter, logger: logger, lockKey: key}
	cl.leases = map[int64]*atomic.Bool{key: &cl.leader}
	_ = cl.ConfigureLifecycle(
		component.WithRunHook(cl.run),
		component.WithOnStopHook(cl.cleanup),
	)
	return cl
}

// IsLeader reports whether this node currently owns the scheduled-automation
// lease. Wire it as Scheduler.LeaderGate.
func (cl *CronLeader) IsLeader() bool { return cl.leader.Load() }

// ScopedGate registers an independent advisory lock on this leader's ONE
// session. Adding schedules must not consume another lifetime connection:
// enough scopes otherwise exhaust the bounded direct pool before Ready closes.
// Call during composition; each scope retains its existing lock key and gate.
func (cl *CronLeader) ScopedGate(scope string) func() bool {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	key := scopedCronKey(scope)
	state := cl.leases[key]
	if state == nil {
		state = &atomic.Bool{}
		cl.leases[key] = state
	}
	return state.Load
}

func (cl *CronLeader) run(ctx context.Context, markStarted func()) error {
	cl.poll(ctx)  // first attempt before signalling Ready so a single-node
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
	ctx, cancel := context.WithTimeout(ctx, cronLeaderPollTimeout)
	defer cancel()
	cl.mu.Lock()
	defer cl.mu.Unlock()
	conn, err := cl.ensureConn(ctx)
	if err != nil {
		cl.demote("no database connection", err)
		return
	}

	// Every held scope belongs to this session. Losing it revokes ALL gates.
	if err := conn.PingContext(ctx); err != nil {
		cl.demote("lease connection lost", err)
		return
	}

	for key, state := range cl.leases {
		if state.Load() {
			continue // session locks are reentrant; never acquire a held key twice
		}
		var acquired bool
		if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&acquired); err != nil {
			cl.demote("advisory-lock query failed", err)
			return
		}
		if acquired {
			state.Store(true)
			if cl.logger != nil {
				cl.logger.Info("cron leader acquired -- this node runs scheduled automations", "lockKey", key)
			}
		}
	}
}

// Caller holds mu.
func (cl *CronLeader) ensureConn(ctx context.Context) (*sql.Conn, error) {
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

// Caller holds mu. Drop every scope before releasing its session.
func (cl *CronLeader) demote(reason string, err error) {
	wasLeader := false
	for _, state := range cl.leases {
		wasLeader = state.Swap(false) || wasLeader
	}
	cl.closeConn()
	if wasLeader && cl.logger != nil {
		cl.logger.Warn("cron leadership released", "reason", reason, "error", err)
	}
}

func (cl *CronLeader) closeConn() {
	if cl.conn != nil {
		// Confirm release on a healthy session before another replica polls.
		// If the connection is broken, discarding it below still releases the
		// locks when PostgreSQL observes that session's loss.
		ctx, cancel := context.WithTimeout(context.Background(), cronLeaderPollTimeout)
		_, _ = cl.conn.ExecContext(ctx, "SELECT pg_advisory_unlock_all()")
		cancel()
		// sql.Conn.Close merely returns a session to its pool; its advisory
		// locks would survive there. Discard it so ALL registered keys release,
		// including a key acquired just before a query's response was lost.
		_ = cl.conn.Raw(func(any) error { return driver.ErrBadConn })
		_ = cl.conn.Close()
		cl.conn = nil
	}
}

func (cl *CronLeader) cleanup() {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	for _, state := range cl.leases {
		state.Store(false)
	}
	cl.closeConn()
}
