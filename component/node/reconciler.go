package node

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component"
	"github.com/znasllc-io/memql/component/auth"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

// TopologyReconciler is the active topology reconciliation loop / fast reaper
// (epic #1871, issue #1874). It closes the gap left by the lazy heartbeat
// prune (pruneStaleClusterNodes, 10-min cron / 30-min window): a pod that
// crashes, is OOM-killed, or is forcibly replaced is never deregistered and
// lingers in v1:cluster:node for up to 30+ minutes. This loop drives topology
// freshness to SECONDS by comparing the DB node set against two live signals
// every few seconds and retiring nodes that are demonstrably gone:
//
//  1. Mesh membership (liveness fast-reap). The live set is this node plus
//     every healthy peer the PeerManager currently knows (its own monitored
//     peers + gossiped siblings). A registered, non-stopped v1:cluster:node
//     that is ABSENT from that live set -- continuously, for a short grace
//     window -- is marked health="stopped". This catches the crashed-pod case
//     the gossip StatusChangeHandler misses (it only authors transitions for
//     directly-monitored peers; an unmonitored sibling that dies is merely
//     reaped from the in-memory table, never marked down in the DB). Using the
//     in-memory mesh as the liveness source -- rather than a DB lastSeen
//     freshness heartbeat -- means NO extra periodic writes to the (append-
//     only) node table: the only writes this loop makes are the actual
//     terminal transitions.
//
//  2. Deployment status (orphan reap). Nodes whose deploymentId belongs to a
//     deployment the deploy driver has marked superseded / failed /
//     rolled_back (supersededDeployments, #1872/#1873) are retired
//     IMMEDIATELY -- no grace -- so a superseded deployment's pods clear from
//     topology within seconds of promotion completing.
//
// Multi-node posture: every mesh replica runs this loop, but a Postgres
// session-level advisory lock (the CronLeader pattern, #561) elects ONE
// cluster-wide leader that actually reconciles -- so two replicas never
// double-retire the same node. Failover is automatic: if the leader pod dies
// its lock connection drops and Postgres releases the lease, and the next
// poll on a surviving replica re-acquires it.
//
// Relationship to the 30-min prune: this loop OWNS the fast path (offline/
// stopped within seconds). pruneStaleClusterNodes stays as the lazy
// backstop -- it only ever marks nodes the fast path missed, and both write
// the same idempotent terminal health="stopped" (staleClusterNodes
// excludes already-stopped rows), so they can never write conflicting
// statuses for the same node.
type TopologyReconciler struct {
	*component.Component

	self     *Identity
	peers    meshMembership
	engine   reconcileEngine
	dbGetter func() *bun.DB
	logger   *slog.Logger

	interval     time.Duration
	grace        time.Duration
	startupDelay time.Duration

	// absentSince tracks, per node id, the first tick at which it was found
	// missing from the live mesh set. A node is only liveness-reaped once it
	// has been continuously absent for >= grace, which rides over a transient
	// gossip-propagation gap (a freshly registered node that has not yet
	// reached the leader's peer view).
	absentSince map[string]time.Time

	// Advisory-lock lease (mirrors CronLeader). Held for the loop's lifetime;
	// only the leader reconciles. The dbGetter is wired to the DIRECT,
	// non-pooled endpoint in app/ (memql#1930) -- a lifetime-held session lock
	// cannot ride a transaction-mode pooler that recycles the backend between
	// statements.
	mu     sync.Mutex
	conn   *sql.Conn
	leader bool

	now func() time.Time
}

// reconcileEngine is the narrow read/write seam the reconciler needs from the
// MemQL engine: execute a query/mutation string and (for reads) return the
// result bundle. *memqlengine.MemQLEngine satisfies it; tests inject a fake.
type reconcileEngine interface {
	Execute(ctx context.Context, query string) (*memqlengine.ExecuteResult, error)
}

// meshMembership is the narrow live-mesh view the reconciler needs: the set of
// peers this node currently knows. *PeerManager satisfies it; tests inject a
// fake peer list.
type meshMembership interface {
	AllPeers() []*PeerEntry
}

const (
	TopologyReconcilerComponentName = common.ComponentName("nodeTopologyReconciler")

	// reconcilerOrder places the reconciler late in startup (after the engine,
	// DB, and peer manager), matching the worker dialer's neighborhood.
	reconcilerOrder = 95

	// reconcilerLockKey is a dedicated 64-bit advisory-lock id for the
	// "owner of topology reconciliation" lease. Distinct from
	// cronLeaderLockKey so the two leases are independent.
	reconcilerLockKey int64 = 7756010113207010574

	defaultReconcileInterval = 3 * time.Second
	defaultReconcileGrace    = 20 * time.Second

	// reconcileStartupDelay defers the first reconcile so a booting cluster's
	// mesh + DB node rows settle before we judge anyone absent (avoids a
	// startup-race false-reap during the first gossip convergence).
	reconcileStartupDelay = 20 * time.Second

	// Operator knobs (documented in docs/public/operate/env-vars.md).
	reconcileIntervalEnv = "MEMQL_NODE_RECONCILE_INTERVAL_SECONDS"
	reconcileGraceEnv    = "MEMQL_NODE_RECONCILE_GRACE_SECONDS"
)

// NewTopologyReconciler builds the reconciler. A nil engine, nil peerMgr, or
// nil dbGetter degrades to a no-op loop (it still starts as a Dependency but
// reconciles nothing) so non-mesh / DB-less binaries and tests are tolerated.
// Interval + grace come from the env knobs with sane defaults.
func NewTopologyReconciler(
	self *Identity,
	peerMgr *PeerManager,
	engine reconcileEngine,
	dbGetter func() *bun.DB,
	logger *slog.Logger,
) *TopologyReconciler {
	comp, _ := component.New(TopologyReconcilerComponentName)
	// Convert the concrete *PeerManager to the interface only when non-nil so a
	// nil peerMgr leaves r.peers a true nil interface (not a typed-nil), which
	// the run-hook disabled check relies on.
	var mm meshMembership
	if peerMgr != nil {
		mm = peerMgr
	}
	r := &TopologyReconciler{
		Component:    comp,
		self:         self,
		peers:        mm,
		engine:       engine,
		dbGetter:     dbGetter,
		logger:       logger,
		interval:     envDurationSeconds(reconcileIntervalEnv, defaultReconcileInterval, logger),
		grace:        envDurationSeconds(reconcileGraceEnv, defaultReconcileGrace, logger),
		startupDelay: reconcileStartupDelay,
		absentSince:  map[string]time.Time{},
		now:          time.Now,
	}
	_ = r.ConfigureLifecycle(
		component.WithRunHook(r.run),
		component.WithOnStopHook(r.cleanup),
	)
	return r
}

// Order runs the reconciler late in the startup sequence.
func (*TopologyReconciler) Order() int { return reconcilerOrder }

func (r *TopologyReconciler) run(ctx context.Context, markStarted func()) error {
	markStarted()

	if r.engine == nil || r.peers == nil || r.dbGetter == nil {
		r.info("topology reconciler disabled (no engine / peer manager / database); the 30-min prune remains the only stale-node cleanup")
		<-ctx.Done()
		return nil
	}

	r.info("topology reconciler scheduled",
		"interval", r.interval.String(),
		"grace", r.grace.String(),
		"startup_delay", r.startupDelay.String())

	select {
	case <-ctx.Done():
		return nil
	case <-time.After(r.startupDelay):
	}

	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			r.tick(ctx)
		}
	}
}

// tick acquires/keepalives the leader lease and, when leader, runs one
// reconcile pass. Non-leaders do nothing.
func (r *TopologyReconciler) tick(ctx context.Context) {
	if !r.ensureLeader(ctx) {
		return
	}
	r.reconcile(ctx)
}

// ensureLeader acquires the advisory lock (non-leader) or keepalives it
// (leader). Returns whether this node currently owns the reconcile lease.
// Mirrors CronLeader.poll: a dead lock connection drops the lease so a
// healthy replica takes over.
func (r *TopologyReconciler) ensureLeader(ctx context.Context) bool {
	conn, err := r.ensureConn(ctx)
	if err != nil {
		r.demote()
		return false
	}
	if r.leader {
		if err := conn.PingContext(ctx); err != nil {
			r.demote()
			return false
		}
		return true
	}
	var acquired bool
	if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", reconcilerLockKey).Scan(&acquired); err != nil {
		r.demote()
		return false
	}
	if acquired {
		r.leader = true
		r.info("topology reconciler leader acquired -- this node reconciles topology")
	}
	return acquired
}

func (r *TopologyReconciler) ensureConn(ctx context.Context) (*sql.Conn, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conn != nil {
		return r.conn, nil
	}
	db := r.dbGetter()
	if db == nil || db.DB == nil {
		return nil, sql.ErrConnDone
	}
	conn, err := db.DB.Conn(ctx)
	if err != nil {
		return nil, err
	}
	r.conn = conn
	return conn, nil
}

func (r *TopologyReconciler) demote() {
	r.leader = false
	r.mu.Lock()
	conn := r.conn
	r.conn = nil
	r.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

func (r *TopologyReconciler) cleanup() {
	r.mu.Lock()
	conn := r.conn
	r.conn = nil
	leader := r.leader
	r.leader = false
	r.mu.Unlock()
	if conn != nil {
		if leader {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_, _ = conn.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", reconcilerLockKey)
			cancel()
		}
		_ = conn.Close()
	}
}

// reconcileNode is the minimal projection of a v1:cluster:node row the
// reconciler reasons about.
type reconcileNode struct {
	id           string // bare node id (the concept id updateNodeHealth resolves)
	nodeType     string
	address      string
	health       string
	deploymentId string
	lastSeen     time.Time
	lastSeenOK   bool
}

// reconcile runs one leader pass: read the current non-stopped node set + the
// superseded-deployment set, then retire orphans (immediately) and liveness-
// lapsed nodes (after grace). All writes are the idempotent terminal
// health="stopped" transition via updateNodeHealth.
func (r *TopologyReconciler) reconcile(ctx context.Context) {
	nodes, err := r.loadNonStoppedNodes(ctx)
	if err != nil {
		r.warn("topology reconciler: node load failed; retrying next tick", "error", err)
		return
	}
	superseded := r.loadSupersededDeploymentIds(ctx)
	live := r.liveMeshSet()
	now := r.now()

	var orphanReaped, livenessReaped int
	present := map[string]struct{}{}

	for _, n := range nodes {
		if n.id == "" || n.id == r.selfID() {
			continue // never reap self
		}

		// Orphan reap: deployment is superseded/failed/rolled_back -> gone,
		// regardless of mesh presence. Immediate, no grace.
		if n.deploymentId != "" {
			if _, bad := superseded[n.deploymentId]; bad {
				if r.retire(ctx, n, "deployment_superseded") {
					orphanReaped++
				}
				delete(r.absentSince, n.id)
				continue
			}
		}

		// Liveness reap: absent from the live mesh set for >= grace.
		if _, alive := live[n.id]; alive {
			delete(r.absentSince, n.id)
			present[n.id] = struct{}{}
			continue
		}

		// Guard: a node whose DB lastSeen is fresher than the grace window was
		// registered/seen very recently and may simply not have propagated to
		// the leader's peer view yet -- do not reap it.
		if n.lastSeenOK && now.Sub(n.lastSeen) < r.grace {
			delete(r.absentSince, n.id)
			continue
		}

		first, tracked := r.absentSince[n.id]
		if !tracked {
			r.absentSince[n.id] = now
			continue
		}
		if now.Sub(first) >= r.grace {
			if r.retire(ctx, n, "mesh_absent") {
				livenessReaped++
			}
			delete(r.absentSince, n.id)
		}
	}

	// Drop tracking for ids no longer in the candidate set (recovered, or
	// already stopped) so the map can't grow unbounded across rollovers.
	for id := range r.absentSince {
		if _, stillCandidate := present[id]; stillCandidate {
			continue
		}
		if !containsNodeID(nodes, id) {
			delete(r.absentSince, id)
		}
	}

	if orphanReaped > 0 || livenessReaped > 0 {
		r.info("topology reconciler retired stale nodes",
			"orphan_reaped", orphanReaped,
			"liveness_reaped", livenessReaped,
			"candidates", len(nodes),
			"live_mesh", len(live))
	}
}

// retire appends a terminal health="stopped" row for the node. Returns whether
// the write succeeded.
func (r *TopologyReconciler) retire(ctx context.Context, n reconcileNode, reason string) bool {
	call, err := buildUpdateNodeHealthCall(n.id, n.nodeType, n.address, "stopped", r.now().UTC().Format(time.RFC3339))
	if err != nil {
		r.warn("topology reconciler: failed to build retire mutation", "node_id", n.id, "error", err)
		return false
	}
	// System actor so the engine's mutation path accepts the write (same
	// attribution the NodeStatusWriter uses).
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	wctx = auth.ContextWithToken(wctx, &auth.TokenInfo{Subject: "system:topology-reconciler"})
	if _, err := r.engine.Execute(wctx, call); err != nil {
		r.warn("topology reconciler: retire write failed", "node_id", n.id, "reason", reason, "error", err)
		return false
	}
	r.info("topology reconciler retired node",
		"node_id", n.id, "node_type", n.nodeType, "reason", reason)
	return true
}

// loadNonStoppedNodes reads the latest-per-id non-stopped v1:cluster:node set
// via staleClusterNodes (asOf latest, health != "stopped") -- the same
// candidate set the prune walks. One query serves both the orphan + liveness
// passes.
func (r *TopologyReconciler) loadNonStoppedNodes(ctx context.Context) ([]reconcileNode, error) {
	result, err := r.engine.Execute(ctx, "staleClusterNodes({})")
	if err != nil {
		return nil, err
	}
	if result == nil || result.Bundle == nil {
		return nil, nil
	}
	out := make([]reconcileNode, 0, len(result.Bundle.Nodes))
	for _, n := range result.Bundle.Nodes {
		payload := n.GetPayload()
		if payload == nil {
			continue
		}
		f := payload.GetFields()
		rn := reconcileNode{
			id:           bareNodeID(n.GetId()),
			nodeType:     strings.TrimSpace(f["nodeType"].GetStringValue()),
			address:      strings.TrimSpace(f["address"].GetStringValue()),
			health:       strings.ToLower(strings.TrimSpace(f["health"].GetStringValue())),
			deploymentId: strings.TrimSpace(f["deploymentId"].GetStringValue()),
		}
		if ls := strings.TrimSpace(f["lastSeen"].GetStringValue()); ls != "" {
			if t, perr := time.Parse(time.RFC3339, ls); perr == nil {
				rn.lastSeen = t
				rn.lastSeenOK = true
			}
		}
		out = append(out, rn)
	}
	return out, nil
}

// loadSupersededDeploymentIds reads the set of deploymentIds whose deployment
// is terminal-not-active (supersededDeployments). A read failure returns
// an empty set -- the orphan pass simply does nothing this tick (the liveness
// pass + the prune still run), never a wrong retirement.
func (r *TopologyReconciler) loadSupersededDeploymentIds(ctx context.Context) map[string]struct{} {
	ids := map[string]struct{}{}
	result, err := r.engine.Execute(ctx, "supersededDeployments({})")
	if err != nil {
		r.warn("topology reconciler: superseded-deployment load failed; skipping orphan pass this tick", "error", err)
		return ids
	}
	if result == nil || result.Bundle == nil {
		return ids
	}
	for _, n := range result.Bundle.Nodes {
		payload := n.GetPayload()
		if payload == nil {
			continue
		}
		if id := strings.TrimSpace(payload.GetFields()["deploymentId"].GetStringValue()); id != "" {
			ids[id] = struct{}{}
		}
	}
	return ids
}

// liveMeshSet is the set of node ids considered alive: this node plus every
// peer the PeerManager currently knows whose advertised health is not
// offline/stopped. Gossiped siblings are included, so the leader's view spans
// the whole mesh -- a node missing from it is genuinely absent, not merely
// unmonitored-by-this-replica.
func (r *TopologyReconciler) liveMeshSet() map[string]struct{} {
	live := map[string]struct{}{}
	if id := r.selfID(); id != "" {
		live[id] = struct{}{}
	}
	for _, p := range r.peers.AllPeers() {
		if p == nil || p.Info == nil || p.Info.NodeId == "" {
			continue
		}
		switch HealthLabel(p.Info.Health) {
		case "offline", "stopped":
			// A peer the mesh itself considers down is not "live" for the
			// reconciler -- let the liveness pass retire its DB row.
			continue
		}
		live[p.Info.NodeId] = struct{}{}
	}
	return live
}

func (r *TopologyReconciler) selfID() string {
	if r.self == nil {
		return ""
	}
	return r.self.ID
}

func (r *TopologyReconciler) info(msg string, args ...any) {
	if r.logger != nil {
		r.logger.Info(msg, args...)
	}
}

func (r *TopologyReconciler) warn(msg string, args ...any) {
	if r.logger != nil {
		r.logger.Warn(msg, args...)
	}
}

// bareNodeID extracts the concept id updateNodeHealth resolves from a
// query-result row id ("_system:v1:cluster:node:<id>" -> "<id>"). Mirrors the
// worker dialer's extraction; matches the bare NodeId the status writer +
// PeerManager use, so compare + write keys agree.
func bareNodeID(rowId string) string {
	rowId = strings.TrimSpace(rowId)
	if idx := strings.LastIndex(rowId, ":"); idx >= 0 && idx < len(rowId)-1 {
		return rowId[idx+1:]
	}
	return rowId
}

func containsNodeID(nodes []reconcileNode, id string) bool {
	for _, n := range nodes {
		if n.id == id {
			return true
		}
	}
	return false
}

// envDurationSeconds reads an integer-seconds env var, returning the default
// when unset/unparsable/non-positive. A typo'd knob must not silently disable
// the loop, so a bad value warns + falls back rather than yielding 0.
func envDurationSeconds(envVar string, def time.Duration, logger *slog.Logger) time.Duration {
	raw := strings.TrimSpace(os.Getenv(envVar))
	if raw == "" {
		return def
	}
	secs, err := strconv.Atoi(raw)
	if err != nil || secs <= 0 {
		if logger != nil {
			logger.Warn("topology reconciler: invalid duration env var; using default",
				"env", envVar, "value", raw, "default", def.String())
		}
		return def
	}
	return time.Duration(secs) * time.Second
}

// compile-time guards.
var (
	_ common.Dependency = (*TopologyReconciler)(nil)
	_ reconcileEngine   = (*memqlengine.MemQLEngine)(nil)
)
