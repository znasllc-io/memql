package node

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/znasllc-io/memql/core/component"
	"github.com/znasllc-io/memql/component/events"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	"github.com/znasllc-io/memql/core/common"
)

const (
	WorkerDialerComponentName = common.ComponentName("nodeWorkerDialer")
	// Place the dialer after NodeServer (48) and ParentConnector (46) so
	// the local server is listening and a parent link (if any) is up
	// before we start dialing workers. Before the gRPC transport
	// (typically 50+) so AI handlers see Connection set the first time
	// they fire.
	workerDialerOrder = 49

	// Reconcile cadence: DB-event-driven reconciles are the primary
	// mechanism; this ticker is a belt-and-suspenders fallback that
	// covers missed events and reconvergence after a transient DB
	// failure.
	workerDialerReconcile = 30 * time.Second
)

// WorkerTarget is a (type, address) pair the WorkerDialer knows to dial.
// NodeId may be empty for targets sourced from a static env-var seed
// (we learn the real NodeId from NodeWelcome on the first handshake)
// and populated for targets sourced from a v1:cluster:node DB row.
type WorkerTarget struct {
	NodeType NodeType
	Address  string
	NodeId   string
}

// isWorkerType reports whether the given node type is a worker the BFF
// should dial for forwarding purposes. Currently all non-BFF types;
// kept as a predicate so we can narrow it later (e.g. skip planner if
// it stops serving forwarded requests).
func isWorkerType(t NodeType) bool {
	switch t {
	case NodeTypeAgent, NodeTypeVoice, NodeTypeCognition, NodeTypePlanner:
		return true
	default:
		return false
	}
}

// ParseWorkerPeers parses the MEMQL_WORKER_PEERS env-var format:
//
//	"voice=voice:50059,agent=agent:50055,cognition=cognition:50054"
//
// Whitespace around entries and pairs is tolerated. Unknown node types
// are silently skipped; malformed entries log-worthy at the call site
// if wanted, but we keep this pure to stay trivially testable.
func ParseWorkerPeers(raw string) []WorkerTarget {
	var out []WorkerTarget
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	seen := make(map[string]bool)
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		typeStr, addr, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		typeStr = strings.TrimSpace(strings.ToLower(typeStr))
		addr = strings.TrimSpace(addr)
		if typeStr == "" || addr == "" {
			continue
		}
		nodeType := NodeType(typeStr)
		if !ValidNodeTypes[nodeType] || !isWorkerType(nodeType) {
			continue
		}
		key := string(nodeType) + "@" + addr
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, WorkerTarget{NodeType: nodeType, Address: addr})
	}
	return out
}

// WorkerDialer opens outbound NodeService streams from a BFF to each
// worker-type peer so that the BFF can push NodeClientMessage envelopes
// (AiForwardRequest / AiForwardCancel, and later capability /
// event-forward messages) through them.
//
// The dial set is reconciled from two sources:
//
//  1. Static seeds (from MEMQL_WORKER_PEERS). Used for deterministic
//     first-boot when the DB has not yet been populated -- e.g. in a
//     multi-node cluster where workers boot in parallel with the BFF
//     and may not have written their v1:cluster:node row by the time
//     the first frontend request arrives.
//
//  2. DB discovery: the dialer subscribes to cluster:node
//     created/updated events AND polls every 30s, reconciling its
//     connection table against healthy worker rows. New workers are
//     dialed; workers that transition to OFFLINE/STOPPED or disappear
//     from the DB are dropped.
//
// Each active dial owns a *peerConnection (the same type ParentConnector
// uses). On NodeWelcome we register the peer as monitored in
// PeerManager and bind the *peerConnection onto its PeerEntry so
// AiForwardRouter / EventBridge / CapabilityRouter can find it.
//
// WorkerDialer is BFF-only -- workers themselves never dial peers in
// this topology. Callers that are not BFF should not construct one.
type WorkerDialer struct {
	*component.Component

	identity *Identity
	peerMgr  *PeerManager
	engine   *memqlengine.MemQLEngine
	eventBus *events.Bus
	logger   *slog.Logger

	// Static seeds -- deep-copied at construction so callers can't mutate
	// them out from under us.
	seeds []WorkerTarget

	// dialTypes optionally narrows which worker types this dialer will
	// dial. Empty means "all worker types" (the BFF default). Set to
	// e.g. []NodeType{NodeTypeAgent} on cognition nodes, which only need
	// to forward agent-turn work to agent peers.
	dialTypes []NodeType

	mu      sync.Mutex
	conns   map[string]*dialEntry // key = "<type>@<address>"
	cancel  context.CancelFunc
	unsubs  []func()
	trigger chan struct{} // buffered(1); non-nil once run() has started

	// AI forwarding response sink. Every AiForwardResponse arriving on a
	// managed connection is dispatched here; on non-BFF binaries or
	// before SetAiForwardResponseSink is called the messages are
	// dropped (the router on the BFF is the only legitimate consumer).
	sinkMu               sync.RWMutex
	aiForwardSink        AiForwardResponseSink
	workbenchForwardSink WorkbenchForwardResponseSink
}

// dialEntry tracks a single active outbound dial. The conn is owned by
// this entry; on shutdown / removal we cancel the per-entry ctx (which
// unblocks peerConnection.Connect) and then Close() the connection.
// nodeId becomes non-empty once NodeWelcome has been processed.
type dialEntry struct {
	target WorkerTarget
	conn   *peerConnection
	cancel context.CancelFunc

	mu     sync.Mutex
	nodeId string
}

// NewWorkerDialer constructs a dialer. Returns nil if we would have
// nothing to do -- no seeds AND no engine/eventBus for DB discovery --
// so callers can unconditionally append the result to the dependency
// list without branching.
//
// peerMgr is required (it's where we publish discovered connections).
// identity is required for the NodeHello handshake on each dialed
// stream.
func NewWorkerDialer(
	identity *Identity,
	peerMgr *PeerManager,
	engine *memqlengine.MemQLEngine,
	eventBus *events.Bus,
	seeds []WorkerTarget,
	logger *slog.Logger,
) *WorkerDialer {
	if identity == nil || peerMgr == nil || logger == nil {
		return nil
	}
	if engine == nil && eventBus == nil && len(seeds) == 0 {
		// Nothing to drive reconciliation from, and no static targets
		// to dial.
		return nil
	}
	comp, _ := component.New(WorkerDialerComponentName)

	wd := &WorkerDialer{
		Component: comp,
		identity:  identity,
		peerMgr:   peerMgr,
		engine:    engine,
		eventBus:  eventBus,
		logger:    logger,
		seeds:     append([]WorkerTarget(nil), seeds...),
		conns:     make(map[string]*dialEntry),
	}
	wd.ConfigureLifecycle(
		component.WithRunHook(wd.run),
		component.WithOnStopHook(wd.cleanup),
	)
	return wd
}

// SetDialTypes narrows the dialer to a specific set of worker types.
// An empty or nil slice restores the default (dial all worker types).
// Intended for workers that need a limited peer mesh -- e.g. cognition
// only needs outbound streams to agent peers. Must be called before
// Start() for the filter to apply to the initial reconcile.
func (wd *WorkerDialer) SetDialTypes(types ...NodeType) {
	if wd == nil {
		return
	}
	wd.mu.Lock()
	defer wd.mu.Unlock()
	if len(types) == 0 {
		wd.dialTypes = nil
		return
	}
	wd.dialTypes = append([]NodeType(nil), types...)
}

// allowDialType reports whether this dialer is configured to dial the
// given worker type. Callers hold no locks.
func (wd *WorkerDialer) allowDialType(t NodeType) bool {
	if wd == nil {
		return false
	}
	wd.mu.Lock()
	allowed := wd.dialTypes
	wd.mu.Unlock()
	if len(allowed) == 0 {
		return true
	}
	for _, a := range allowed {
		if a == t {
			return true
		}
	}
	return false
}

// SetAiForwardResponseSink installs the sink that receives
// AiForwardResponse messages arriving on our managed connections.
// Called during bootstrap on BFF binaries; workers leave this nil.
// Thread-safe; may be called at any time.
func (wd *WorkerDialer) SetAiForwardResponseSink(sink AiForwardResponseSink) {
	if wd == nil {
		return
	}
	wd.sinkMu.Lock()
	wd.aiForwardSink = sink
	wd.sinkMu.Unlock()
}

// SetWorkbenchForwardResponseSink installs the sink that receives
// WorkbenchForwardResponse messages arriving on our managed
// connections. Called during agent-node bootstrap when cluster-mode
// workbench forwarding is enabled; other node types leave nil.
// Thread-safe; may be called at any time.
func (wd *WorkerDialer) SetWorkbenchForwardResponseSink(sink WorkbenchForwardResponseSink) {
	if wd == nil {
		return
	}
	wd.sinkMu.Lock()
	wd.workbenchForwardSink = sink
	wd.sinkMu.Unlock()
}

// Order returns the startup priority for the dialer.
func (*WorkerDialer) Order() int {
	return workerDialerOrder
}

// ActiveAddresses returns the addresses this dialer currently has an
// outbound stream to. Primarily useful in tests and for operator
// diagnostics.
func (wd *WorkerDialer) ActiveAddresses() []string {
	if wd == nil {
		return nil
	}
	wd.mu.Lock()
	defer wd.mu.Unlock()
	out := make([]string, 0, len(wd.conns))
	for _, e := range wd.conns {
		out = append(out, e.target.Address)
	}
	return out
}

func (wd *WorkerDialer) run(ctx context.Context, markStarted func()) error {
	ctx, cancel := context.WithCancel(ctx)

	// Buffered(1) trigger channel gives us at-least-once reconcile
	// semantics without blocking publishers. Multiple events collapse
	// into a single reconcile pass.
	trigger := make(chan struct{}, 1)

	wd.mu.Lock()
	wd.cancel = cancel
	wd.trigger = trigger
	wd.mu.Unlock()

	if wd.eventBus != nil {
		unsubCreated := wd.eventBus.Subscribe(
			"graph.node.created.v1:cluster:node",
			func(events.Event) { wd.triggerReconcile() },
			events.WithSubscriberName("worker_dialer:nodeCreated"),
		)
		unsubUpdated := wd.eventBus.Subscribe(
			"graph.node.updated.v1:cluster:node",
			func(events.Event) { wd.triggerReconcile() },
			events.WithSubscriberName("worker_dialer:nodeUpdated"),
		)
		wd.mu.Lock()
		wd.unsubs = []func(){unsubCreated, unsubUpdated}
		wd.mu.Unlock()
	}

	markStarted()

	wd.logger.Info("worker_dialer: started",
		"static_seeds", len(wd.seeds),
		"db_discovery", wd.engine != nil,
		"reconcile_interval", workerDialerReconcile,
	)

	// Initial reconcile: use seeds immediately so we aren't waiting on
	// the DB for a cluster that was configured via env.
	wd.reconcile(ctx)

	ticker := time.NewTicker(workerDialerReconcile)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			wd.reconcile(ctx)
		case <-trigger:
			wd.reconcile(ctx)
		}
	}
}

// triggerReconcile requests a reconcile without blocking the caller.
// Safe to call from any goroutine (event handlers, tests, etc.).
func (wd *WorkerDialer) triggerReconcile() {
	if wd == nil {
		return
	}
	wd.mu.Lock()
	ch := wd.trigger
	wd.mu.Unlock()
	if ch == nil {
		return // run() has not started yet
	}
	select {
	case ch <- struct{}{}:
	default:
		// A reconcile is already queued; collapse.
	}
}

// reconcile rebuilds the desired target set (seeds ∪ DB rows), diffs it
// against currently-active dials, and dials/closes to converge.
func (wd *WorkerDialer) reconcile(ctx context.Context) {
	desired := wd.buildDesiredSet(ctx)

	wd.mu.Lock()
	defer wd.mu.Unlock()

	// Add missing.
	for key, target := range desired {
		if _, ok := wd.conns[key]; ok {
			continue
		}
		entry := wd.dialTarget(ctx, target)
		if entry != nil {
			wd.conns[key] = entry
		}
	}

	// Remove gone.
	for key, entry := range wd.conns {
		if _, ok := desired[key]; ok {
			continue
		}
		wd.logger.Info("worker_dialer: closing dial (no longer in desired set)",
			"address", entry.target.Address,
			"node_type", string(entry.target.NodeType),
		)
		wd.shutdownEntry(entry)
		delete(wd.conns, key)
	}
}

// buildDesiredSet returns the full desired target map (seeds ∪ DB
// discovery), keyed by "<type>@<address>". Callers hold no locks.
func (wd *WorkerDialer) buildDesiredSet(ctx context.Context) map[string]WorkerTarget {
	desired := make(map[string]WorkerTarget)

	for _, t := range wd.seeds {
		if wd.isSelf(t.Address) {
			continue
		}
		if !wd.allowDialType(t.NodeType) {
			continue
		}
		desired[targetKey(t)] = t
	}

	if wd.engine != nil {
		for _, t := range wd.discoverFromDB(ctx) {
			if wd.isSelf(t.Address) {
				continue
			}
			if !wd.allowDialType(t.NodeType) {
				continue
			}
			// DB-sourced targets override seeds on the same key because
			// they carry a node_id we'd otherwise have to learn from
			// NodeWelcome.
			desired[targetKey(t)] = t
		}
	}

	return desired
}

// isSelf reports whether the given address points at this node (avoid
// self-dial loops). Both our advertised address and an empty address
// count as self.
func (wd *WorkerDialer) isSelf(addr string) bool {
	if addr == "" {
		return true
	}
	self := strings.TrimSpace(wd.identity.Address)
	return self != "" && self == addr
}

// discoverFromDB queries v1:cluster:node and returns worker targets.
// Skips rows without an address, non-worker node types, and rows
// marked OFFLINE/STOPPED.
func (wd *WorkerDialer) discoverFromDB(ctx context.Context) []WorkerTarget {
	if wd.engine == nil {
		return nil
	}
	result, err := wd.engine.Execute(ctx, "concept==v1:cluster:node")
	if err != nil {
		wd.logger.Debug("worker_dialer: DB discovery query failed",
			"error", err,
		)
		return nil
	}
	if result == nil || result.Bundle == nil {
		return nil
	}

	var out []WorkerTarget
	for _, n := range result.Bundle.Nodes {
		payload := n.GetPayload()
		if payload == nil {
			continue
		}
		fields := payload.GetFields()

		nodeType := NodeType(strings.ToLower(strings.TrimSpace(fields["nodeType"].GetStringValue())))
		if !isWorkerType(nodeType) {
			continue
		}

		addr := strings.TrimSpace(fields["address"].GetStringValue())
		if addr == "" {
			continue
		}

		health := strings.ToLower(strings.TrimSpace(fields["health"].GetStringValue()))
		switch health {
		case "offline", "stopped":
			// Skip -- we don't want to dial a worker the cluster knows
			// is down. A later updated event will bring it back into
			// the desired set when it recovers.
			continue
		}

		// Extract the real node_id from the row's id ("_system:v1:cluster:node:<id>").
		rowId := n.GetId()
		nodeId := rowId
		if idx := strings.LastIndex(rowId, ":"); idx >= 0 && idx < len(rowId)-1 {
			nodeId = rowId[idx+1:]
		}

		out = append(out, WorkerTarget{
			NodeType: nodeType,
			Address:  addr,
			NodeId:   nodeId,
		})
	}
	return out
}

// dialTarget creates a *peerConnection for the target and starts its
// Connect loop on a goroutine. Returns the tracking entry.
func (wd *WorkerDialer) dialTarget(parent context.Context, target WorkerTarget) *dialEntry {
	dialCtx, cancel := context.WithCancel(parent)

	conn := newPeerConnection(wd.identity, target.NodeId, target.Address, wd.logger)
	if hb := wd.peerMgr.HeartbeatInterval(); hb > 0 {
		conn.SetHeartbeatInterval(hb)
	}
	// Advertise this node's lifecycle health on every heartbeat to the worker
	// (memql#1268) so peers route around us the instant we drain.
	conn.SetHealthFn(wd.peerMgr.Lifecycle().Health)
	// Re-mint our node token if the worker rejects it after an identity key
	// rotation (memql#1521), so the BFF recovers on its own instead of looping
	// forever on a dead token. Only wired when self-bootstrap is configured.
	if wd.identity.CanRemintBearerToken() {
		conn.SetReauthFn(func(ctx context.Context) (string, error) {
			return wd.identity.RemintBearerToken(ctx, wd.logger)
		})
	}

	entry := &dialEntry{
		target: target,
		conn:   conn,
		cancel: cancel,
		nodeId: target.NodeId,
	}

	wd.logger.Info("worker_dialer: dialing worker",
		"address", target.Address,
		"node_type", string(target.NodeType),
		"preferred_node_id", target.NodeId,
	)

	go func() {
		err := conn.Connect(dialCtx, func(msg *nodev1.NodeServerMessage) {
			wd.handleServerMessage(entry, msg)
		})
		// dialCtx cancelled -> we (shutdownEntry) initiated the teardown,
		// nothing more to do.
		if dialCtx.Err() != nil {
			return
		}
		// Unexpected exit. Connect's reconnect loop only returns when its
		// inner ctx is cancelled, which means pc.Close() was called from
		// outside the dialer -- typically peerManager.Remove(workerId)
		// firing from nodeService.Stream's defer when the worker's inbound
		// ParentConnector stream dies on container restart. Without this
		// recovery path, the dead entry sits in wd.conns forever, blocking
		// reconcile from re-dialing because the key is "occupied."
		if err != nil {
			wd.logger.Warn("worker_dialer: connection ended unexpectedly, will redial",
				"address", target.Address,
				"node_type", string(target.NodeType),
				"error", err,
			)
		}
		wd.handleConnectExit(entry)
	}()

	return entry
}

// handleConnectExit reclaims a dialEntry whose Connect() returned without a
// worker_dialer-initiated shutdown. Drops it from wd.conns, detaches the
// (now closed) connection from PeerManager so other components stop trying
// to Send on it, and triggers an immediate reconcile so the next pass
// re-dials if the target is still desired.
func (wd *WorkerDialer) handleConnectExit(entry *dialEntry) {
	if wd == nil || entry == nil {
		return
	}

	wd.mu.Lock()
	key := targetKey(entry.target)
	cur, ok := wd.conns[key]
	if !ok || cur != entry {
		// Already replaced or removed by reconcile -- another dial is
		// either active or pending under this key, leave it alone.
		wd.mu.Unlock()
		return
	}
	delete(wd.conns, key)
	wd.mu.Unlock()

	entry.mu.Lock()
	nodeId := entry.nodeId
	entry.mu.Unlock()
	if nodeId != "" {
		wd.peerMgr.DetachConnection(nodeId)
	}

	wd.triggerReconcile()
}

// shutdownEntry cancels the entry's Connect loop, detaches its
// Connection from PeerManager, and closes the connection. Caller holds
// wd.mu.
func (wd *WorkerDialer) shutdownEntry(entry *dialEntry) {
	if entry == nil {
		return
	}
	entry.mu.Lock()
	nodeId := entry.nodeId
	entry.mu.Unlock()

	if nodeId != "" {
		wd.peerMgr.DetachConnection(nodeId)
	}
	if entry.cancel != nil {
		entry.cancel()
	}
	entry.conn.Close()
}

// handleServerMessage is the onMessage callback for each managed
// connection. We mirror the relevant handling in parent_connector.go
// (register peer on welcome, absorb peer introductions, route
// AiForwardResponse into the sink).
func (wd *WorkerDialer) handleServerMessage(entry *dialEntry, msg *nodev1.NodeServerMessage) {
	if msg == nil {
		return
	}

	// Any inbound message from the worker confirms its liveness. Once
	// we've learned its NodeId we refresh LastSeen so the liveness
	// checker doesn't degrade a silently-productive peer.
	entry.mu.Lock()
	knownId := entry.nodeId
	entry.mu.Unlock()
	if knownId != "" {
		wd.peerMgr.TouchPeer(knownId)
	}

	switch payload := msg.Payload.(type) {
	case *nodev1.NodeServerMessage_NodeWelcome:
		welcome := payload.NodeWelcome
		if welcome == nil || welcome.NodeId == "" || welcome.NodeId == wd.identity.ID {
			return
		}

		peerInfo := &nodev1.PeerInfo{
			NodeId:   welcome.NodeId,
			NodeType: string(entry.target.NodeType),
			Address:  entry.target.Address,
			Health:   nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY,
		}
		wd.peerMgr.RegisterMonitored(peerInfo)
		wd.peerMgr.AttachConnection(welcome.NodeId, entry.conn)

		entry.mu.Lock()
		entry.nodeId = welcome.NodeId
		entry.mu.Unlock()

		wd.logger.Info("worker_dialer: worker ready",
			"address", entry.target.Address,
			"node_type", string(entry.target.NodeType),
			"node_id", welcome.NodeId,
		)

		// Absorb siblings the worker knows about. These are unmonitored
		// because we don't have a direct stream to them (unless we
		// separately dialed them ourselves, in which case
		// RegisterMonitored will have already won with the sticky-true
		// flag).
		for _, peer := range welcome.Peers {
			if peer.NodeId == wd.identity.ID {
				continue
			}
			wd.peerMgr.Register(peer)
		}

	case *nodev1.NodeServerMessage_PeerIntro:
		intro := payload.PeerIntro
		if intro == nil {
			return
		}
		for _, peer := range intro.Peers {
			if peer.NodeId == wd.identity.ID {
				continue
			}
			if intro.Removal {
				wd.peerMgr.Remove(peer.NodeId)
			} else {
				wd.peerMgr.Register(peer)
			}
		}

	case *nodev1.NodeServerMessage_AiForwardResponse:
		wd.sinkMu.RLock()
		sink := wd.aiForwardSink
		wd.sinkMu.RUnlock()
		if sink != nil {
			sink.Dispatch(payload.AiForwardResponse)
		}

	case *nodev1.NodeServerMessage_WorkbenchForwardResponse:
		wd.sinkMu.RLock()
		sink := wd.workbenchForwardSink
		wd.sinkMu.RUnlock()
		if sink != nil {
			sink.Dispatch(payload.WorkbenchForwardResponse)
		}
	}
}

func (wd *WorkerDialer) cleanup() {
	wd.mu.Lock()
	cancel := wd.cancel
	unsubs := wd.unsubs
	entries := make([]*dialEntry, 0, len(wd.conns))
	for _, e := range wd.conns {
		entries = append(entries, e)
	}
	wd.conns = make(map[string]*dialEntry)
	wd.unsubs = nil
	wd.trigger = nil
	wd.mu.Unlock()

	for _, u := range unsubs {
		if u != nil {
			u()
		}
	}
	if cancel != nil {
		cancel()
	}
	for _, e := range entries {
		wd.shutdownEntry(e)
	}

	wd.logger.Info("worker_dialer: cleaned up")
}

func targetKey(t WorkerTarget) string {
	return fmt.Sprintf("%s@%s", t.NodeType, t.Address)
}
