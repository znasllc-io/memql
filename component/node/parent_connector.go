package node

import (
	"context"
	"log/slog"
	"sync"
	"time"

	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/component"
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
	aiForwardSink        AiForwardResponseSink        // optional, set on BFF binaries
	workbenchForwardSink WorkbenchForwardResponseSink // optional, set on agent binaries in cluster mode
	workerForwardSink    WorkerForwardResponseSink    // optional, set on agent binaries (memql#4352)
	// deployControlSink routes replies to deploy-control forwards this node
	// started (memql#3380). Set on the bff, which is the node that serves the
	// portal; nil elsewhere. Registered here as well as on the WorkerDialer
	// because peer discovery can make the identity node this node's PARENT,
	// in which case the reply arrives on this stream and nowhere else.
	deployControlSink DeployControlForwardResponseSink
}

// SetAiForwardResponseSink registers a sink for AiForwardResponse
// messages received over the parent connection. Called during bootstrap
// on BFF binaries. Workers leave this nil.
func (pc *ParentConnector) SetAiForwardResponseSink(sink AiForwardResponseSink) {
	if pc == nil {
		return
	}
	pc.mu.Lock()
	defer pc.mu.Unlock()
	pc.aiForwardSink = sink
}

// SetWorkerForwardResponseSink registers a sink for inbound
// WorkerForwardResponse / WorkerForwardStream messages arriving over the
// parent connection (memql#4352). Set on agent binaries; nil elsewhere.
func (pc *ParentConnector) SetWorkerForwardResponseSink(sink WorkerForwardResponseSink) {
	if pc == nil {
		return
	}
	pc.mu.Lock()
	pc.workerForwardSink = sink
	pc.mu.Unlock()
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

// SetDeployControlForwardResponseSink registers a sink for inbound
// DeployControlForwardResponse messages received over the parent connection
// (memql#3380). Called during bff bootstrap; other node types leave it nil.
func (pc *ParentConnector) SetDeployControlForwardResponseSink(sink DeployControlForwardResponseSink) {
	if pc == nil {
		return
	}
	pc.mu.Lock()
	defer pc.mu.Unlock()
	pc.deployControlSink = sink
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
	// path where Connect itself returns. A peer removal permanently closes its
	// transport, so each outer attempt must create a fresh connection.
	for {
		conn := newPeerConnection(pc.identity, "", pc.identity.ParentAddress, pc.logger)
		conn.SetHeartbeatInterval(pc.peerMgr.HeartbeatInterval())
		// Advertise this node's lifecycle health on every heartbeat to the parent
		// (memql#1268) so the parent routes around us the instant we drain.
		conn.SetHealthFn(pc.peerMgr.Lifecycle().Health)
		// Re-mint our node token if the parent rejects it after an identity key
		// rotation (memql#1521), so a leaf recovers on its own instead of looping
		// forever on a dead token. Only wired when self-bootstrap is configured --
		// an out-of-band MEMQL_NODE_TOKEN cannot be re-minted.
		if pc.identity.CanRemintBearerToken() {
			conn.SetReauthFn(func(ctx context.Context) (string, error) {
				return pc.identity.RemintBearerToken(ctx, pc.logger)
			})
		}

		pc.mu.Lock()
		pc.conn = conn
		pc.mu.Unlock()

		pc.peerMgr.SetParentConnection(conn)

		err := conn.Connect(ctx, pc.handleServerMessage)
		conn.Close()
		pc.mu.Lock()
		parentID := pc.parentNodeId
		pc.parentNodeId = ""
		pc.conn = nil
		pc.mu.Unlock()
		pc.peerMgr.detachConnectionIf(parentID, conn)

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
	// The parent also emits NodeHeartbeat on the server ticker; treating
	// every message as a touch still covers the welcome / intro window
	// before the first beat and keeps LastSeen honest under load.
	pc.mu.Lock()
	parentID := pc.parentNodeId
	pc.mu.Unlock()
	if parentID != "" {
		pc.peerMgr.TouchPeer(parentID)
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
			// EventBridge.sendToPeers can Send on it. Without this the
			// connection lives on PeerManager.parentConn but every downstream
			// iterator over AllPeers()/ByType() sees peer.Connection == nil
			// and silently skips it -- events originated on this node never
			// reach the parent.
			pc.mu.Lock()
			conn := pc.conn
			previousParent := pc.parentNodeId
			pc.parentNodeId = welcome.NodeId
			pc.mu.Unlock()
			if previousParent != welcome.NodeId {
				// A service address may reconnect to a different BFF replica.
				// Reaping its old identity must not close the new live stream.
				pc.peerMgr.detachConnectionIf(previousParent, conn)
			}
			if conn != nil {
				conn.SetNodeId(welcome.NodeId)
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

	case *nodev1.NodeServerMessage_AiForwardResponse:
		// Responses to forwards originated on this node. Route through
		// the registered sink (set on BFF binaries; no-op otherwise).
		pc.mu.Lock()
		sink := pc.aiForwardSink
		pc.mu.Unlock()
		if sink != nil {
			sink.Dispatch(payload.AiForwardResponse)
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

	case *nodev1.NodeServerMessage_WorkerForwardResponse:
		// Reply to a machine dispatch this node forwarded (memql#4352).
		pc.mu.Lock()
		wsink := pc.workerForwardSink
		pc.mu.Unlock()
		if wsink != nil {
			wsink.Dispatch(payload.WorkerForwardResponse)
		}

	case *nodev1.NodeServerMessage_WorkerForwardStream:
		// Streamed stdout / stderr from that machine, relayed across the hop.
		pc.mu.Lock()
		ssink := pc.workerForwardSink
		pc.mu.Unlock()
		if ssink != nil {
			ssink.DispatchStream(payload.WorkerForwardStream)
		}

	case *nodev1.NodeServerMessage_ModelForwardResponse:
		// Terminal answer for a model call this node forwarded (memql#4676).
		pc.mu.Lock()
		msink := pc.workerForwardSink
		pc.mu.Unlock()
		if msink != nil {
			msink.DispatchModel(payload.ModelForwardResponse)
		}

	case *nodev1.NodeServerMessage_ModelForwardDelta:
		// Generated tokens from that machine, relayed across the hop.
		pc.mu.Lock()
		dsink := pc.workerForwardSink
		pc.mu.Unlock()
		if dsink != nil {
			dsink.DispatchModelDelta(payload.ModelForwardDelta)
		}

	case *nodev1.NodeServerMessage_ModelPullForwardResponse:
		// Terminal answer for a model pull this node forwarded (memql#5103).
		pc.mu.Lock()
		psink := pc.workerForwardSink
		pc.mu.Unlock()
		if psink != nil {
			psink.DispatchModelPull(payload.ModelPullForwardResponse)
		}

	case *nodev1.NodeServerMessage_ModelPullForwardProgress:
		// Download progress from that machine, relayed across the hop.
		pc.mu.Lock()
		ppsink := pc.workerForwardSink
		pc.mu.Unlock()
		if ppsink != nil {
			ppsink.DispatchModelPullProgress(payload.ModelPullForwardProgress)
		}

	case *nodev1.NodeServerMessage_ModelProbeForwardResponse:
		// Terminal answer for a model probe this node forwarded (epic
		// memql#5146). It carries the FIGURES, so a dropped one is not a
		// missing status -- it is a measurement that ran and was lost.
		pc.mu.Lock()
		qsink := pc.workerForwardSink
		pc.mu.Unlock()
		if qsink != nil {
			qsink.DispatchModelProbe(payload.ModelProbeForwardResponse)
		}

	case *nodev1.NodeServerMessage_ModelProbeForwardProgress:
		// One finished case, relayed across the hop.
		pc.mu.Lock()
		qpsink := pc.workerForwardSink
		pc.mu.Unlock()
		if qpsink != nil {
			qpsink.DispatchModelProbeProgress(payload.ModelProbeForwardProgress)
		}

	case *nodev1.NodeServerMessage_DeployControlForwardResponse:
		// Reply to a deploy-control forward this node originated. Set on the
		// bff; no-op otherwise.
		pc.mu.Lock()
		sink := pc.deployControlSink
		pc.mu.Unlock()
		if sink != nil {
			sink.Dispatch(payload.DeployControlForwardResponse)
		}

	case *nodev1.NodeServerMessage_EventForward:
		// An event the parent pushed down this stream (memql#5338, D1) takes
		// the same arrival path as one a peer sent up a stream this node
		// accepted: published once, relayed once, never back to the parent.
		pc.mu.Lock()
		parentId := pc.parentNodeId
		pc.mu.Unlock()
		pc.peerMgr.receiveEvent(payload.EventForward, parentId)
	}
	// SpawnRequest, Heartbeat, CapabilityQuery, etc. fall through -- the
	// top-of-function TouchPeer already refreshed the parent's liveness
	// and we have no local state to update.
}
