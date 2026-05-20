package node

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	"github.com/znasllc-io/memql/core/common"
)

const (
	PeerManagerComponentName = common.ComponentName("nodePeerManager")
	peerManagerOrder         = 45

	defaultHeartbeatInterval = 5 * time.Second
	defaultLivenessTimeout   = 15 * time.Second // 3 missed heartbeats
	defaultOfflineTimeout    = 25 * time.Second // 5 missed heartbeats
)

// StatusChangeHandler is invoked whenever a peer's health status transitions.
// It is called synchronously from the caller's goroutine (either a liveness
// tick, a Register, or a TouchPeer) so implementations must return quickly
// and must not block. A typical implementation schedules an async concept
// upsert on its own goroutine.
//
// The handler receives a snapshot of the peer info at the transition point.
// `old` is the previous NodeHealthStatus, `new` is the incoming status.
// `lastSeen` is the timestamp used to evaluate the transition.
type StatusChangeHandler func(ctx context.Context, peer *nodev1.PeerInfo, old, new nodev1.NodeHealthStatus, lastSeen time.Time)

// PeerEntry represents a known peer in the cluster.
//
// Monitored reports whether this node is the liveness authority for the
// peer. A PeerManager only fires degraded/offline transitions for
// monitored peers -- typically peers on the other end of a direct stream
// (inbound handshake, child's ParentConnector). Peers learned
// indirectly through PeerIntroduction stay unmonitored so sibling nodes
// don't false-positive a peer they never receive heartbeats from.
type PeerEntry struct {
	Info       *nodev1.PeerInfo
	LastSeen   time.Time
	Connection *peerConnection // nil if not directly connected
	Monitored  bool
}

// PeerManager maintains an in-memory peer table and manages the heartbeat
// lifecycle. It implements common.Dependency.
type PeerManager struct {
	*component.Component

	identity *Identity
	mu       sync.RWMutex
	peers    map[string]*PeerEntry              // keyed by node_id
	byType   map[NodeType]map[string]*PeerEntry // type -> node_id -> entry

	// Parent connection (to the peer that bootstrapped this node).
	parentConn *peerConnection

	// Child connections (nodes that registered with this node as parent).
	childMu    sync.RWMutex
	childConns map[string]*peerConnection

	heartbeatInterval time.Duration
	livenessTimeout   time.Duration
	offlineTimeout    time.Duration

	// onStatusChange is invoked on every actual health transition. Set via
	// SetStatusChangeHandler during bootstrap. nil handler == no-op.
	statusMu       sync.RWMutex
	onStatusChange StatusChangeHandler

	logger *slog.Logger
}

// NewPeerManager creates a PeerManager for the given node identity with
// default heartbeat timings. Use SetTimings to override.
func NewPeerManager(identity *Identity, logger *slog.Logger) *PeerManager {
	comp, _ := component.New(PeerManagerComponentName)

	pm := &PeerManager{
		Component:         comp,
		identity:          identity,
		peers:             make(map[string]*PeerEntry),
		byType:            make(map[NodeType]map[string]*PeerEntry),
		childConns:        make(map[string]*peerConnection),
		heartbeatInterval: defaultHeartbeatInterval,
		livenessTimeout:   defaultLivenessTimeout,
		offlineTimeout:    defaultOfflineTimeout,
		logger:            logger,
	}

	pm.ConfigureLifecycle(
		component.WithRunHook(pm.run),
		component.WithOnStopHook(pm.cleanup),
	)

	return pm
}

// SetTimings overrides the default heartbeat / liveness / offline durations.
// Any zero-valued duration leaves its field at the current value. Must be
// called before Start.
func (pm *PeerManager) SetTimings(heartbeat, liveness, offline time.Duration) {
	if heartbeat > 0 {
		pm.heartbeatInterval = heartbeat
	}
	if liveness > 0 {
		pm.livenessTimeout = liveness
	}
	if offline > 0 {
		pm.offlineTimeout = offline
	}
}

// HeartbeatInterval returns the interval between outbound heartbeats.
// Exposed so peer connections can install their own send tickers using the
// same cadence the liveness checker expects.
func (pm *PeerManager) HeartbeatInterval() time.Duration {
	return pm.heartbeatInterval
}

// SetStatusChangeHandler installs a callback invoked on every health
// transition. Passing nil disables notifications. Thread-safe; may be
// swapped at runtime.
func (pm *PeerManager) SetStatusChangeHandler(h StatusChangeHandler) {
	pm.statusMu.Lock()
	pm.onStatusChange = h
	pm.statusMu.Unlock()
}

func (pm *PeerManager) fireStatusChange(peer *nodev1.PeerInfo, old, new nodev1.NodeHealthStatus, lastSeen time.Time) {
	if peer == nil || old == new {
		return
	}
	pm.statusMu.RLock()
	handler := pm.onStatusChange
	pm.statusMu.RUnlock()
	if handler == nil {
		return
	}
	// Copy the PeerInfo so the handler cannot race with in-place mutations
	// of pm.peers[...].Info (proto is a struct and has atomic-ish fields
	// but we want a stable snapshot).
	snapshot := &nodev1.PeerInfo{
		NodeId:       peer.NodeId,
		NodeType:     peer.NodeType,
		Address:      peer.Address,
		Health:       new,
		Capabilities: append([]string(nil), peer.Capabilities...),
		Labels:       cloneStringMap(peer.Labels),
	}
	handler(context.Background(), snapshot, old, new, lastSeen)
}

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// Order returns the startup order for the PeerManager.
func (*PeerManager) Order() int {
	return peerManagerOrder
}

// Register adds or updates a peer in the peer table. Equivalent to
// RegisterPeer(info, false) -- appropriate for fan-out registrations
// (PeerIntroduction) where this node cannot observe the peer's liveness
// directly.
func (pm *PeerManager) Register(info *nodev1.PeerInfo) {
	pm.RegisterPeer(info, false)
}

// RegisterMonitored marks the peer as directly-observable: this node's
// liveness checker will degrade/offline the peer if heartbeats stop.
// Callers are stream_handler.handleHello (inbound direct peer) and
// ParentConnector's NodeWelcome handler (outbound to parent).
func (pm *PeerManager) RegisterMonitored(info *nodev1.PeerInfo) {
	pm.RegisterPeer(info, true)
}

// RegisterPeer adds or updates a peer with an explicit monitored flag.
func (pm *PeerManager) RegisterPeer(info *nodev1.PeerInfo, monitored bool) {
	if info == nil || info.NodeId == "" {
		return
	}

	now := time.Now()

	pm.mu.Lock()
	entry, exists := pm.peers[info.NodeId]
	var (
		priorHealth    nodev1.NodeHealthStatus
		newHealth      nodev1.NodeHealthStatus
		fireTransition bool
	)
	if exists {
		// Returning peer. If it was degraded or offline, transition to
		// healthy immediately since a fresh registration implies liveness.
		priorHealth = entry.Info.Health
		entry.Info = info
		entry.LastSeen = now
		// Preserve the newly-arrived health but upgrade to HEALTHY if the
		// incoming side didn't specify (peer handshake uses HEALTHY default
		// already, but be defensive).
		if info.Health == nodev1.NodeHealthStatus_NODE_HEALTH_UNSPECIFIED {
			entry.Info.Health = nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY
		}
		// Monitored flag is sticky-true: once we start monitoring a peer
		// we don't stop just because a later PeerIntroduction re-advertises
		// it.
		if monitored {
			entry.Monitored = true
		}
		newHealth = entry.Info.Health
		fireTransition = priorHealth != newHealth
	} else {
		// First time we see this peer. It has technically reached us, so
		// it's at least CONNECTING. If the incoming side advertised HEALTHY
		// in its handshake (the common case), honour that.
		if info.Health == nodev1.NodeHealthStatus_NODE_HEALTH_UNSPECIFIED {
			info.Health = nodev1.NodeHealthStatus_NODE_HEALTH_CONNECTING
		}
		entry = &PeerEntry{
			Info:      info,
			LastSeen:  now,
			Monitored: monitored,
		}
		pm.peers[info.NodeId] = entry
		priorHealth = nodev1.NodeHealthStatus_NODE_HEALTH_UNSPECIFIED
		newHealth = info.Health
		fireTransition = true
	}

	// Update type index.
	nodeType := NodeType(info.NodeType)
	if pm.byType[nodeType] == nil {
		pm.byType[nodeType] = make(map[string]*PeerEntry)
	}
	pm.byType[nodeType][info.NodeId] = entry

	// Snapshot for logging/notification outside the lock.
	peerSnapshot := entry.Info
	pm.mu.Unlock()

	pm.logger.Info("peer registered",
		"peer_id", info.NodeId,
		"peer_type", info.NodeType,
		"peer_address", info.Address,
		"health", newHealth.String(),
	)

	if fireTransition {
		pm.fireStatusChange(peerSnapshot, priorHealth, newHealth, now)
	}
}

// Remove removes a peer from the peer table and fires a STOPPED transition
// so downstream consumers (the concept store, the CLI) observe the exit.
func (pm *PeerManager) Remove(nodeId string) {
	pm.mu.Lock()
	entry, exists := pm.peers[nodeId]
	if !exists {
		pm.mu.Unlock()
		return
	}

	priorHealth := entry.Info.Health
	peerSnapshot := entry.Info
	lastSeen := entry.LastSeen

	// Remove from type index.
	nodeType := NodeType(entry.Info.NodeType)
	if typeMap, ok := pm.byType[nodeType]; ok {
		delete(typeMap, nodeId)
		if len(typeMap) == 0 {
			delete(pm.byType, nodeType)
		}
	}

	// Close connection if any.
	if entry.Connection != nil {
		entry.Connection.Close()
	}

	delete(pm.peers, nodeId)
	pm.mu.Unlock()

	pm.logger.Info("peer removed", "peer_id", nodeId)

	pm.fireStatusChange(peerSnapshot, priorHealth, nodev1.NodeHealthStatus_NODE_HEALTH_STOPPED, lastSeen)
}

// UpdatePeerHealth records a peer-reported health status (e.g. DRAINING)
// and fires a transition if it differs from the current value. Unlike
// TouchPeer, this does NOT update LastSeen -- pair it with TouchPeer in
// the heartbeat handler.
func (pm *PeerManager) UpdatePeerHealth(nodeId string, reported nodev1.NodeHealthStatus) {
	if reported == nodev1.NodeHealthStatus_NODE_HEALTH_UNSPECIFIED {
		return
	}

	pm.mu.Lock()
	entry, ok := pm.peers[nodeId]
	if !ok {
		pm.mu.Unlock()
		return
	}

	priorHealth := entry.Info.Health
	if priorHealth == reported {
		pm.mu.Unlock()
		return
	}
	entry.Info.Health = reported
	peerSnapshot := entry.Info
	lastSeen := entry.LastSeen
	pm.mu.Unlock()

	pm.fireStatusChange(peerSnapshot, priorHealth, reported, lastSeen)
}

// TouchPeer updates the LastSeen timestamp for a peer and, if the peer was
// previously marked DEGRADED or CONNECTING, transitions it back to HEALTHY.
func (pm *PeerManager) TouchPeer(nodeId string) {
	now := time.Now()

	pm.mu.Lock()
	entry, ok := pm.peers[nodeId]
	if !ok {
		pm.mu.Unlock()
		return
	}

	entry.LastSeen = now
	priorHealth := entry.Info.Health
	fireTransition := false
	// A heartbeat from a degraded peer is an immediate recovery.
	if priorHealth == nodev1.NodeHealthStatus_NODE_HEALTH_DEGRADED ||
		priorHealth == nodev1.NodeHealthStatus_NODE_HEALTH_CONNECTING {
		entry.Info.Health = nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY
		fireTransition = true
	}
	peerSnapshot := entry.Info
	newHealth := entry.Info.Health
	pm.mu.Unlock()

	if fireTransition {
		pm.fireStatusChange(peerSnapshot, priorHealth, newHealth, now)
	}
}

// Get returns a peer entry by node ID.
func (pm *PeerManager) Get(nodeId string) *PeerEntry {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	return pm.peers[nodeId]
}

// ByType returns all peers of the given node type.
func (pm *PeerManager) ByType(t NodeType) []*PeerEntry {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	typeMap, ok := pm.byType[t]
	if !ok {
		return nil
	}

	entries := make([]*PeerEntry, 0, len(typeMap))
	for _, entry := range typeMap {
		entries = append(entries, entry)
	}
	return entries
}

// ByCapability returns peers that advertise the given capability FQN.
func (pm *PeerManager) ByCapability(capabilityFQN string) []*PeerEntry {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	var entries []*PeerEntry
	for _, entry := range pm.peers {
		for _, cap := range entry.Info.Capabilities {
			if cap == capabilityFQN {
				entries = append(entries, entry)
				break
			}
		}
	}
	return entries
}

// AllPeers returns a snapshot of all current peers.
func (pm *PeerManager) AllPeers() []*PeerEntry {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	entries := make([]*PeerEntry, 0, len(pm.peers))
	for _, entry := range pm.peers {
		entries = append(entries, entry)
	}
	return entries
}

// PeerInfoList returns PeerInfo protos for all current peers.
func (pm *PeerManager) PeerInfoList() []*nodev1.PeerInfo {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	infos := make([]*nodev1.PeerInfo, 0, len(pm.peers))
	for _, entry := range pm.peers {
		infos = append(infos, entry.Info)
	}
	return infos
}

// PeerCount returns the number of registered peers.
func (pm *PeerManager) PeerCount() int {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return len(pm.peers)
}

// SetParentConnection sets the connection to the parent peer.
func (pm *PeerManager) SetParentConnection(conn *peerConnection) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.parentConn = conn
}

// AttachConnection binds an outbound *peerConnection to the PeerEntry for
// the given nodeId so downstream code (SIForwardRouter, EventBridge,
// CapabilityRouter) can call Send on it. Callers MUST hold the
// connection's lifetime until DetachConnection is invoked or the entry
// is removed; PeerManager does not own the connection.
//
// If no entry exists for nodeId the call is a no-op -- the typical
// flow is RegisterMonitored(...) followed immediately by
// AttachConnection, so a missing entry indicates a race that will be
// corrected on the next register.
func (pm *PeerManager) AttachConnection(nodeId string, conn *peerConnection) {
	if pm == nil || nodeId == "" || conn == nil {
		return
	}
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if entry, ok := pm.peers[nodeId]; ok {
		entry.Connection = conn
	}
}

// DetachConnection clears the *peerConnection on the PeerEntry for
// nodeId without closing it -- the caller owns the close. Use this
// before shutting down a connection so subsequent Sends from other
// components fail fast rather than racing the close.
func (pm *PeerManager) DetachConnection(nodeId string) {
	if pm == nil || nodeId == "" {
		return
	}
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if entry, ok := pm.peers[nodeId]; ok {
		entry.Connection = nil
	}
}

// AddChildConnection adds a connection for a child node.
func (pm *PeerManager) AddChildConnection(nodeId string, conn *peerConnection) {
	pm.childMu.Lock()
	defer pm.childMu.Unlock()
	pm.childConns[nodeId] = conn
}

// RemoveChildConnection removes a child connection.
func (pm *PeerManager) RemoveChildConnection(nodeId string) {
	pm.childMu.Lock()
	defer pm.childMu.Unlock()

	if conn, ok := pm.childConns[nodeId]; ok {
		conn.Close()
		delete(pm.childConns, nodeId)
	}
}

// run is the PeerManager lifecycle loop that drives periodic liveness checks.
func (pm *PeerManager) run(ctx context.Context, markStarted func()) error {
	markStarted()

	tick := pm.heartbeatInterval
	if tick <= 0 {
		tick = defaultHeartbeatInterval
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			pm.checkLiveness()
		}
	}
}

// checkLiveness scans the peer table and transitions peers across liveness
// thresholds. Peers past the offline threshold are downgraded to OFFLINE and
// removed from the in-memory routing table (so no further traffic is sent
// their way), but the StatusChangeHandler is invoked first so the concept
// record survives with health="offline".
func (pm *PeerManager) checkLiveness() {
	now := time.Now()

	type transition struct {
		peer     *nodev1.PeerInfo
		oldH     nodev1.NodeHealthStatus
		newH     nodev1.NodeHealthStatus
		lastSeen time.Time
		remove   bool
	}
	var transitions []transition

	pm.mu.Lock()
	for nodeId, entry := range pm.peers {
		// Only peers whose heartbeats this node directly receives are
		// subject to liveness tripping. Peers advertised via
		// PeerIntroduction (siblings) are visible for routing but not
		// owned by this PeerManager.
		if !entry.Monitored {
			continue
		}
		elapsed := now.Sub(entry.LastSeen)
		prior := entry.Info.Health

		switch {
		case elapsed > pm.offlineTimeout:
			// Transition to OFFLINE and prepare for removal from the peer
			// table. We fire the status change handler BEFORE deleting so
			// the status writer can snapshot the peer info.
			entry.Info.Health = nodev1.NodeHealthStatus_NODE_HEALTH_OFFLINE
			transitions = append(transitions, transition{
				peer:     entry.Info,
				oldH:     prior,
				newH:     nodev1.NodeHealthStatus_NODE_HEALTH_OFFLINE,
				lastSeen: entry.LastSeen,
				remove:   true,
			})

			// Remove from type index + close connection.
			nodeType := NodeType(entry.Info.NodeType)
			if typeMap, ok := pm.byType[nodeType]; ok {
				delete(typeMap, nodeId)
				if len(typeMap) == 0 {
					delete(pm.byType, nodeType)
				}
			}
			if entry.Connection != nil {
				entry.Connection.Close()
			}
			delete(pm.peers, nodeId)

			pm.logger.Warn("peer marked offline and removed from routing table",
				"peer_id", nodeId,
				"last_seen", entry.LastSeen,
				"elapsed", elapsed,
			)

		case elapsed > pm.livenessTimeout && prior != nodev1.NodeHealthStatus_NODE_HEALTH_DEGRADED && prior != nodev1.NodeHealthStatus_NODE_HEALTH_DRAINING:
			entry.Info.Health = nodev1.NodeHealthStatus_NODE_HEALTH_DEGRADED
			transitions = append(transitions, transition{
				peer:     entry.Info,
				oldH:     prior,
				newH:     nodev1.NodeHealthStatus_NODE_HEALTH_DEGRADED,
				lastSeen: entry.LastSeen,
			})

			pm.logger.Warn("peer degraded due to missed heartbeats",
				"peer_id", nodeId,
				"last_seen", entry.LastSeen,
				"elapsed", elapsed,
			)
		}
	}
	pm.mu.Unlock()

	// Fire handlers outside the lock. Preserving `remove` flag for possible
	// future use (e.g. emitting distinct events for degradation vs. eviction).
	for _, t := range transitions {
		pm.fireStatusChange(t.peer, t.oldH, t.newH, t.lastSeen)
	}
}

// cleanup closes all peer connections on shutdown.
func (pm *PeerManager) cleanup() {
	pm.mu.Lock()
	for _, entry := range pm.peers {
		if entry.Connection != nil {
			entry.Connection.Close()
		}
	}
	pm.peers = make(map[string]*PeerEntry)
	pm.byType = make(map[NodeType]map[string]*PeerEntry)
	pm.mu.Unlock()

	pm.childMu.Lock()
	for _, conn := range pm.childConns {
		conn.Close()
	}
	pm.childConns = make(map[string]*peerConnection)
	pm.childMu.Unlock()

	if pm.parentConn != nil {
		pm.parentConn.Close()
		pm.parentConn = nil
	}

	pm.logger.Info("peer manager cleaned up")
}
