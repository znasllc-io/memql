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
	ParentConnectorComponentName = common.ComponentName("nodeParentConnector")
	parentConnectorOrder         = 46 // after PeerManager (45), before NodeServer
)

// ParentConnector dials the node's parent peer and keeps a single
// bidirectional NodeService stream open. While the stream is up the
// connection's heartbeat loop (see connection.go:heartbeatLoop) emits
// NodeHeartbeat messages on the configured interval; this is what gives
// the parent's PeerManager the liveness signal it needs to mark a child
// degraded or offline when it disappears.
//
// ParentConnector is only installed on node types that have a parent
// address configured. Root nodes and standalone nodes skip it.
type ParentConnector struct {
	*component.Component

	identity *Identity
	peerMgr  *PeerManager
	logger   *slog.Logger

	mu                   sync.Mutex
	conn                 *peerConnection
	cancel               context.CancelFunc
	parentNodeId         string                       // learned from NodeWelcome
	aiForwardSink        SIForwardResponseSink        // optional, set on BFF binaries
	workbenchForwardSink WorkbenchForwardResponseSink // optional, set on agent binaries in cluster mode
	eventInbound         EventInbound                 // republishes parent-pushed events onto local bus
}

// SetAiForwardResponseSink registers a sink for SIForwardResponse
// messages received over the parent connection. Called during bootstrap
// on BFF binaries. Workers leave this nil.
func (pc *ParentConnector) SetAiForwardResponseSink(sink SIForwardResponseSink) {
	if pc == nil {
		return
	}
	pc.mu.Lock()
	defer pc.mu.Unlock()
	pc.aiForwardSink = sink
}

// SetWorkbenchForwardResponseSink registers a sink for inbound
// WorkbenchForwardResponse messages received over the parent
// connection. Called during agent-node bootstrap when cluster-mode
// workbench forwarding is enabled.
func (pc *ParentConnector) SetWorkbenchForwardResponseSink(sink WorkbenchForwardResponseSink) {
	if pc == nil {
		return
	}
	pc.mu.Lock()
	defer pc.mu.Unlock()
	pc.workbenchForwardSink = sink
}

// SetEventInbound registers the local-bus bridge for EventForward
// messages the parent pushes down this stream. Without it, events sent
// parent -> child land in this callback and fall through silently.
func (pc *ParentConnector) SetEventInbound(h EventInbound) {
	if pc == nil {
		return
	}
	pc.mu.Lock()
	defer pc.mu.Unlock()
	pc.eventInbound = h
}

// NewParentConnector returns nil when the identity has no parent address,
// so callers can blindly append the result to the dependency list without
// branching on node type.
func NewParentConnector(identity *Identity, peerMgr *PeerManager, logger *slog.Logger) *ParentConnector {
	if identity == nil || identity.ParentAddress == "" {
		return nil
	}
	comp, _ := component.New(ParentConnectorComponentName)

	pc := &ParentConnector{
		Component: comp,
		identity:  identity,
		peerMgr:   peerMgr,
		logger:    logger,
	}
	pc.ConfigureLifecycle(
		component.WithRunHook(pc.run),
		component.WithOnStopHook(pc.cleanup),
	)
	return pc
}

// Order places this after PeerManager so the heartbeat interval is
// available, but before NodeServer/EventBridge so the stream is up by
// the time events start flowing.
func (*ParentConnector) Order() int {
	return parentConnectorOrder
}

// parentReconnectDelay is how long the supervising run loop waits before
// re-dialing the parent after Connect returns a non-context error (memql#1246).
// peerConnection.Connect already backs off internally between attempts within a
// single Connect call; this is the OUTER backstop for the rare path where
// Connect itself returns (e.g. a clean parent-side stream end during the
// parent's own rollout) so we re-establish promptly without busy-spinning.
const parentReconnectDelay = 2 * time.Second

func (pc *ParentConnector) run(ctx context.Context, markStarted func()) error {
	ctx, cancel := context.WithCancel(ctx)
	pc.mu.Lock()
	pc.cancel = cancel
	pc.mu.Unlock()

	conn := newPeerConnection(pc.identity, "", pc.identity.ParentAddress, pc.logger)
	conn.SetHeartbeatInterval(pc.peerMgr.HeartbeatInterval())
	// Advertise this node's lifecycle health on every heartbeat to the parent
	// (memql#1268) so the parent routes around us the instant we drain.
	conn.SetHealthFn(pc.peerMgr.Lifecycle().Health)

	pc.mu.Lock()
	pc.conn = conn
	pc.mu.Unlock()

	pc.peerMgr.SetParentConnection(conn)

	markStarted()

	pc.logger.Info("parent_connector: dialing parent",
		"parent_address", pc.identity.ParentAddress,
		"node_id", pc.identity.ID,
		"node_type", string(pc.identity.Type),
	)

	// Supervising reconnect loop (memql#1246). A lost parent stream -- even a
	// clean/transient one while the parent (e.g. identity) is ITSELF rolling
	// -- must NOT terminate this run hook: if it did, the component framework
	// would flip IsRunning() false permanently (ParentConnector is a /healthz
	// dependency), wedging the pod at readiness 503 / 0-1 until a manual delete
	// (the exact #1246 outage). Instead, keep re-dialing with backoff until the
	// context is cancelled (Stop). Readiness then reflects serving-capability
	// (the node reconnects on its own) rather than "every mesh dependency is
	// currently connected". peerConnection.Connect already reconnects
	// internally between attempts; this outer loop is the backstop for the rare
	// path where Connect itself returns.
	for {
		err := conn.Connect(ctx, pc.handleServerMessage)

		// Context cancelled -> Stop was requested; exit cleanly so the
		// component framework records a normal stop (not a crash).
		if ctx.Err() != nil {
			return nil
		}

		// Connect returned without a cancellation: the parent stream ended
		// (clean EOF or error). Log + re-dial after a short backoff rather than
		// returning, so the node stays serving-capable across the parent's
		// reconnect window.
		pc.logger.Warn("parent_connector: parent stream ended; will reconnect",
			"error", err,
			"parent_address", pc.identity.ParentAddress,
			"node_id", pc.identity.ID,
		)

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(parentReconnectDelay):
		}
	}
}

func (pc *ParentConnector) cleanup() {
	pc.mu.Lock()
	cancel := pc.cancel
	conn := pc.conn
	pc.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if conn != nil {
		conn.Close()
	}
}

// handleServerMessage is invoked for every message received on the
// parent->child direction of the stream. We register peer introductions
// into the local peer table so this node learns about its siblings
// through the parent's fan-out.
func (pc *ParentConnector) handleServerMessage(msg *nodev1.NodeServerMessage) {
	if msg == nil {
		return
	}
	// Any inbound message from the parent is evidence of its liveness.
	// The parent's NodeService server does not currently run an outbound
	// heartbeat ticker, so we treat every message (NodeWelcome,
	// PeerIntroduction, EventForward, etc.) as a keep-alive and refresh
	// the parent's LastSeen. Once the parent is registered (via
	// NodeWelcome below) future messages will bump its liveness.
	if pc.parentNodeId != "" {
		pc.peerMgr.TouchPeer(pc.parentNodeId)
	}
	switch payload := msg.Payload.(type) {
	case *nodev1.NodeServerMessage_NodeWelcome:
		welcome := payload.NodeWelcome
		if welcome == nil {
			return
		}
		// The parent announces its own NodeId via NodeWelcome.
		// Register it as monitored so our PeerManager trips degraded/
		// offline if the parent stops heartbeating (e.g. if the parent
		// container goes down).
		if welcome.NodeId != "" && welcome.NodeId != pc.identity.ID {
			pc.peerMgr.RegisterMonitored(&nodev1.PeerInfo{
				NodeId:   welcome.NodeId,
				NodeType: welcome.NodeType,
				Address:  pc.identity.ParentAddress,
				Health:   nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY,
			})
			// Bind our outbound stream onto the parent's PeerEntry so
			// EventBridge.forwardToPeers can Send on it. Without this the
			// connection lives on PeerManager.parentConn but every downstream
			// iterator over AllPeers()/ByType() sees peer.Connection == nil
			// and silently skips it -- events originated on this node never
			// reach the parent.
			pc.mu.Lock()
			conn := pc.conn
			pc.parentNodeId = welcome.NodeId
			pc.mu.Unlock()
			if conn != nil {
				pc.peerMgr.AttachConnection(welcome.NodeId, conn)
			}
		}
		// Siblings announced via welcome.Peers are unmonitored: we don't
		// have a direct stream to them and can't observe their liveness.
		for _, peer := range welcome.Peers {
			if peer.NodeId == pc.identity.ID {
				continue
			}
			pc.peerMgr.Register(peer)
		}
	case *nodev1.NodeServerMessage_PeerIntro:
		intro := payload.PeerIntro
		if intro == nil {
			return
		}
		for _, peer := range intro.Peers {
			if peer.NodeId == pc.identity.ID {
				continue
			}
			if intro.Removal {
				pc.peerMgr.Remove(peer.NodeId)
			} else {
				pc.peerMgr.Register(peer)
			}
		}

	case *nodev1.NodeServerMessage_SiForwardResponse:
		// Responses to forwards originated on this node. Route through
		// the registered sink (set on BFF binaries; no-op otherwise).
		pc.mu.Lock()
		sink := pc.aiForwardSink
		pc.mu.Unlock()
		if sink != nil {
			sink.Dispatch(payload.SiForwardResponse)
		}

	case *nodev1.NodeServerMessage_WorkbenchForwardResponse:
		// Workbench dispatch responses for cluster-mode forwards.
		// Set on agent binaries when MEMQL_WORKBENCH_REMOTE is on;
		// no-op otherwise.
		pc.mu.Lock()
		sink := pc.workbenchForwardSink
		pc.mu.Unlock()
		if sink != nil {
			sink.Dispatch(payload.WorkbenchForwardResponse)
		}

	case *nodev1.NodeServerMessage_EventForward:
		// Events the parent pushes down this stream need to land on the
		// local event bus, same as events received via nodeService.Stream
		// on the server side. Without this handoff, cross-node subscribers
		// silently never fire.
		pc.mu.Lock()
		inbound := pc.eventInbound
		parentId := pc.parentNodeId
		pc.mu.Unlock()
		if inbound != nil {
			inbound.HandleInbound(payload.EventForward)
			inbound.ForwardInboundToPeers(payload.EventForward, parentId)
		}
	}
	// SpawnRequest, Heartbeat, CapabilityQuery, etc. fall through -- the
	// top-of-function TouchPeer already refreshed the parent's liveness
	// and we have no local state to update.
}
