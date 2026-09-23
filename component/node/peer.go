package node

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/znasllc-io/memql/component/metrics"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/component"
)

const (
	PeerManagerComponentName = common.ComponentName("nodePeerManager")
	peerManagerOrder         = 45

	defaultHeartbeatInterval = 5 * time.Second
	defaultLivenessTimeout   = 15 * time.Second // 3 missed heartbeats
	defaultOfflineTimeout    = 25 * time.Second // 5 missed heartbeats

	// defaultStaleGossipTimeout bounds how long an UNMONITORED peer
	// (learned via PeerIntroduction / NodeWelcome, Connection == nil --
	// this node never receives its heartbeats) may linger in the peer
	// table before it is reaped. Such entries are never subject to the
	// healthy->degraded->offline transition logic (that's monitored-only),
	// so without this window they would accumulate forever across K8s
	// rollouts (peers keyed by pod name, #1042). Set to a small multiple of
	// offlineTimeout (~2.4x) so a transiently-quiet sibling that is still
	// being re-advertised by the mesh is not reaped prematurely, while dead
	// pods are cleared within ~1 minute.
	defaultStaleGossipTimeout = 60 * time.Second
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

	// inbound holds the push half of every stream a peer OPENED to this node,
	// by the peer's node id, oldest first (memql#5338, D1). Guarded by mu. A
	// peer can hold more than one: a static seed and a discovered address can
	// land on the same pod, and a reconnect can overlap the stream it
	// replaces. eventTarget uses the newest one that is still open.
	inbound map[string][]*inboundStream

	// sink is where an event arriving on ANY stream goes -- the server side of
	// an accepted stream, the ParentConnector and the WorkerDialer alike
	// (memql#5338, D6). NewEventBridge installs itself here, so a component
	// that reads events off the wire needs no wiring of its own: the
	// WorkerDialer, wired by hand at four call sites, never was, and dropped
	// every event a server pushed to it.
	sinkMu sync.RWMutex
	sink   meshEventSink

	heartbeatInterval  time.Duration
	livenessTimeout    time.Duration
	offlineTimeout     time.Duration
	staleGossipTimeout time.Duration

	// onStatusChange is invoked on every actual health transition. Set via
	// SetStatusChangeHandler during bootstrap. nil handler == no-op.
	statusMu       sync.RWMutex
	onStatusChange StatusChangeHandler

	// lifecycle is THIS node's self-asserted lifecycle state machine
	// (Starting/Ready/Draining/Stopped, epic memql#1259 #4). The heartbeat
	// builders advertise lifecycle.Health() in gossip so peers learn this
	// node's state; the readiness handler consults it so a Draining node
	// reports not-ready while staying alive on /livez. Never nil after
	// construction.
	lifecycle *NodeLifecycle

	logger *slog.Logger
}

// NewPeerManager creates a PeerManager for the given node identity with
// default heartbeat timings. Use SetTimings to override.
func NewPeerManager(identity *Identity, logger *slog.Logger) *PeerManager {
	comp, _ := component.New(PeerManagerComponentName)

	pm := &PeerManager{
		Component:          comp,
		identity:           identity,
		peers:              make(map[string]*PeerEntry),
		byType:             make(map[NodeType]map[string]*PeerEntry),
		inbound:            make(map[string][]*inboundStream),
		heartbeatInterval:  defaultHeartbeatInterval,
		livenessTimeout:    defaultLivenessTimeout,
		offlineTimeout:     defaultOfflineTimeout,
		staleGossipTimeout: defaultStaleGossipTimeout,
		lifecycle:          NewNodeLifecycle(),
		logger:             logger,
	}

	pm.ConfigureLifecycle(
		component.WithRunHook(pm.run),
		component.WithOnStopHook(pm.cleanup),
	)

	return pm
}

// SelfNodeId / SelfNodeType report this node's own identity. The mesh
// forwarded-auth router stamps them onto every authority assertion as audit
// provenance, from one place, so no producer call site can forget it
// (memql#3205). Audit-only: never an input to an authorization decision, so an
// empty value never changes a verdict.
func (pm *PeerManager) SelfNodeId() string {
	if pm == nil || pm.identity == nil {
		return ""
	}
	return pm.identity.ID
}

func (pm *PeerManager) SelfNodeType() string {
	if pm == nil || pm.identity == nil {
		return ""
	}
	return string(pm.identity.Type)
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

// SetStaleGossipTimeout overrides the window after which an unmonitored,
// unconnected peer (learned via gossip) is reaped from the peer table. A
// zero or negative value leaves the current value unchanged. Must be
// called before Start. Kept separate from SetTimings so the existing
// 3-arg timing contract (and its callers) stays stable.
func (pm *PeerManager) SetStaleGossipTimeout(d time.Duration) {
	if d > 0 {
		pm.staleGossipTimeout = d
	}
}

// sendTarget atomically reads the entry's OUTBOUND Connection under the read
// lock and returns it, or (nil, false) when this node never dialed the peer.
// Returning the connection from inside the lock-protected read closes the
// check->Send TOCTOU window: callers send on the snapshot they got here rather
// than re-reading entry.Connection (which a concurrent DetachConnection could
// have niled).
//
// This is the transport for REQUESTS (the capability router, and the tests
// that bind a dial). Events use eventTarget, which can also push down a stream
// the peer opened.
func (pm *PeerManager) sendTarget(entry *PeerEntry) (*peerConnection, bool) {
	if entry == nil {
		return nil, false
	}
	pm.mu.RLock()
	conn := entry.Connection
	pm.mu.RUnlock()

	return conn, conn != nil
}

// meshEventSink is what takes an event off the wire: the EventBridge. One
// method, one arrival path, whichever direction of whichever stream the event
// came down (memql#5338, D6).
type meshEventSink interface {
	ReceiveForward(evt *nodev1.EventForward, fromNodeId string)
}

// setEventSink installs the bridge that events arriving on this node's streams
// go to. NewEventBridge calls it.
func (pm *PeerManager) setEventSink(s meshEventSink) {
	if pm == nil {
		return
	}
	pm.sinkMu.Lock()
	pm.sink = s
	pm.sinkMu.Unlock()
}

// receiveEvent hands an EventForward that arrived from fromNodeId to the
// bridge. A node with no bridge drops it: nothing on it could consume the
// event, and saying so at Debug is all there is to say.
func (pm *PeerManager) receiveEvent(evt *nodev1.EventForward, fromNodeId string) {
	if pm == nil || evt == nil {
		return
	}
	pm.sinkMu.RLock()
	sink := pm.sink
	pm.sinkMu.RUnlock()
	if sink == nil {
		pm.logger.Debug("mesh event dropped: no event bridge on this node",
			"topic", evt.Topic, "from", fromNodeId)
		return
	}
	sink.ReceiveForward(evt, fromNodeId)
}

// attachInbound records the push half of a stream peerId opened to this node.
// The stream handler calls it once the handshake completes, before the
// welcome goes out, so by the time the peer binds its own side of the stream
// this side can already push.
func (pm *PeerManager) attachInbound(peerId string, s *inboundStream) {
	if pm == nil || peerId == "" || s == nil {
		return
	}
	pm.mu.Lock()
	pm.inbound[peerId] = append(pm.inbound[peerId], s)
	pm.mu.Unlock()
}

// detachInbound releases exactly the stream it is given -- compare-and-delete,
// so a stream that ends late can never release the replacement that
// reconnected under the same peer id.
func (pm *PeerManager) detachInbound(peerId string, s *inboundStream) {
	if pm == nil || peerId == "" || s == nil {
		return
	}
	pm.mu.Lock()
	defer pm.mu.Unlock()
	streams := pm.inbound[peerId]
	for i, cur := range streams {
		if cur == s {
			streams = append(streams[:i:i], streams[i+1:]...)
			break
		}
	}
	if len(streams) == 0 {
		delete(pm.inbound, peerId)
		return
	}
	pm.inbound[peerId] = streams
}

// latestOpenInboundLocked returns the newest accepted stream from peerId that
// still takes events. Caller holds mu.
func (pm *PeerManager) latestOpenInboundLocked(peerId string) *inboundStream {
	streams := pm.inbound[peerId]
	for i := len(streams) - 1; i >= 0; i-- {
		if streams[i].open() {
			return streams[i]
		}
	}
	return nil
}

// eventSender puts one EventForward on one transport without blocking, and
// reports whether it was queued.
type eventSender func(*nodev1.EventForward) bool

// eventTarget picks the ONE transport an event goes to this peer on
// (memql#5338, D2), and names it for the counters. A node often holds two
// streams to one peer -- the bff dials an agent that also parent-dials the
// bff -- and sending on both would put a duplicate on the wire for dedup to
// throw away. In order:
//
//  1. the dialed connection, when its stream is up right now;
//  2. the newest accepted stream that is still open;
//  3. the dialed connection while it reconnects: its outbox holds the event
//     until the stream is back, which is what it always did.
//
// A peer with none of the three -- a sibling learned only from gossip -- has
// no transport, and a relay reaches it instead (D3).
func (pm *PeerManager) eventTarget(entry *PeerEntry) (eventSender, string, bool) {
	if pm == nil || entry == nil || entry.Info == nil {
		return nil, "", false
	}
	pm.mu.RLock()
	dialed := entry.Connection
	accepted := pm.latestOpenInboundLocked(entry.Info.NodeId)
	pm.mu.RUnlock()

	switch {
	case dialed != nil && dialed.connected():
		return dialed.sendEvent, metrics.MeshTransportDialed, true
	case accepted != nil:
		return accepted.sendEvent, metrics.MeshTransportAccepted, true
	case dialed != nil:
		return dialed.sendEvent, metrics.MeshTransportDialed, true
	}
	return nil, "", false
}

// MeshLink is one peer this node exchanges events with, as its delivery
// report spells it (memql#5338, D7).
type MeshLink struct {
	// Node is the peer's node id.
	Node string
	// Type is the peer's node type, as it announced itself.
	Type string
	// Via says which way the stream runs: "dialed" (this node opened it),
	// "accepted" (the peer opened it) or "both".
	Via string
}

// meshLinks lists every peer this node holds a stream to, in either direction,
// sorted by node id. A gossiped sibling with no stream is not a link.
func (pm *PeerManager) meshLinks() []MeshLink {
	if pm == nil {
		return nil
	}
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	links := make([]MeshLink, 0, len(pm.peers))
	for nodeId, entry := range pm.peers {
		dialed := entry.Connection != nil
		accepted := pm.latestOpenInboundLocked(nodeId) != nil
		var via string
		switch {
		case dialed && accepted:
			via = "both"
		case dialed:
			via = metrics.MeshTransportDialed
		case accepted:
			via = metrics.MeshTransportAccepted
		default:
			continue
		}
		nodeType := ""
		if entry.Info != nil {
			nodeType = entry.Info.NodeType
		}
		links = append(links, MeshLink{Node: nodeId, Type: nodeType, Via: via})
	}
	sort.Slice(links, func(i, j int) bool { return links[i].Node < links[j].Node })
	return links
}

// HeartbeatInterval returns the interval between outbound heartbeats.
// Exposed so peer connections can install their own send tickers using the
// same cadence the liveness checker expects.
func (pm *PeerManager) HeartbeatInterval() time.Duration {
	return pm.heartbeatInterval
}

// Lifecycle returns this node's self-asserted lifecycle state machine
// (Starting/Ready/Draining/Stopped, epic memql#1259 #4). Callers use it to
// flip the node Ready at boot, mark it Draining on shutdown (the actual
// triggers land in memql#1269 / #1270), and to gate readiness. The heartbeat
// builders read Lifecycle().Health() so the advertised gossip health tracks
// the lifecycle. Never nil.
func (pm *PeerManager) Lifecycle() *NodeLifecycle {
	return pm.lifecycle
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

// SnapshotByType returns immutable selections for operations that outlive the
// peer-table lock. The connection remains the selected transport even if the
// manager detaches or replaces it; its own lifecycle reports disconnection.
func (pm *PeerManager) SnapshotByType(t NodeType) []*PeerEntry {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	entries := make([]*PeerEntry, 0, len(pm.byType[t]))
	for _, entry := range pm.byType[t] {
		snapshot := *entry
		if entry.Info != nil {
			snapshot.Info = proto.Clone(entry.Info).(*nodev1.PeerInfo)
		}
		entries = append(entries, &snapshot)
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

// PeerInfoList returns COPIES of the PeerInfo protos for all current peers.
//
// Copies, because the caller marshals them onto a stream (the NodeWelcome)
// outside this lock while a heartbeat on another stream can be updating the
// same peer's Health in place (UpdatePeerHealth). Handing out the table's own
// pointers was a data race the real-transport gate found the first time it
// ran several streams at once (mesh_delivery_test.go, under -race).
func (pm *PeerManager) PeerInfoList() []*nodev1.PeerInfo {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	infos := make([]*nodev1.PeerInfo, 0, len(pm.peers))
	for _, entry := range pm.peers {
		infos = append(infos, proto.Clone(entry.Info).(*nodev1.PeerInfo))
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
// the given nodeId so downstream code (AiForwardRouter, EventBridge,
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
	if entry, ok := pm.peers[nodeId]; ok {
		entry.Connection = conn
	}
	pm.mu.Unlock()
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

// detachConnectionIf releases only the transport owned by the caller, so an
// older attempt cannot detach a replacement registered under the same peer ID.
func (pm *PeerManager) detachConnectionIf(nodeID string, conn *peerConnection) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if entry := pm.peers[nodeID]; entry != nil && entry.Connection == conn {
		entry.Connection = nil
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
		// subject to liveness *tripping* (the healthy->degraded->offline
		// transitions below). Peers advertised via PeerIntroduction /
		// NodeWelcome (siblings) are visible for routing but not owned by
		// this PeerManager -- this node never receives their heartbeats, so
		// it cannot author a health transition for them.
		//
		// They still need to be *reaped* once they go stale, though:
		// otherwise gossiped entries for dead pods accumulate in the table
		// forever across K8s rollouts (peers keyed by pod name, #1042). An
		// unmonitored entry with no live outbound connection that hasn't
		// been re-advertised within staleGossipTimeout is treated as gone
		// and removed. This is a pure table reap, NOT a health transition:
		// we do not emit a status change (no spurious "offline" DB health
		// row for a pod this node never monitored). selectPeer
		// (component/grpc/ai_forward.go) already requires Connection != nil,
		// so these Connection==nil entries are already unroutable -- removing
		// them cannot change routing results, it only frees memory and keeps
		// the gossip/topology view accurate.
		if !entry.Monitored {
			if entry.Connection == nil && now.Sub(entry.LastSeen) > pm.staleGossipTimeout {
				nodeType := NodeType(entry.Info.NodeType)
				if typeMap, ok := pm.byType[nodeType]; ok {
					delete(typeMap, nodeId)
					if len(typeMap) == 0 {
						delete(pm.byType, nodeType)
					}
				}
				delete(pm.peers, nodeId)

				pm.logger.Info("stale unmonitored peer reaped from peer table",
					"peer_id", nodeId,
					"last_seen", entry.LastSeen,
					"elapsed", now.Sub(entry.LastSeen),
				)
			}
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

	// The accepted streams belong to their handlers, which close them as the
	// server stops; forgetting them here is all the table owes.
	pm.mu.Lock()
	pm.inbound = make(map[string][]*inboundStream)
	pm.mu.Unlock()

	if pm.parentConn != nil {
		pm.parentConn.Close()
		pm.parentConn = nil
	}

	pm.logger.Info("peer manager cleaned up")
}
