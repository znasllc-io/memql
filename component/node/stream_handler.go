package node

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	"github.com/znasllc-io/memql/core/id"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// QueryExecutor is the interface for executing MemQL queries.
// Satisfied by *memqlengine.MemQLEngine.
type QueryExecutor interface {
	Execute(ctx context.Context, query string) (*memqlengine.ExecuteResult, error)
}

// ForwardedAccessResolver turns a forwarded claims map into the caller's
// AccessContext by looking the principal up in the database. Satisfied by
// *auth.IdentityResolver.
//
// A forwarded query executes MemQL -- reads, mutations and logic -- under the
// originating caller's authority, and the claims arrive as an unsigned map on
// a peer message. Deriving authority from that map directly would let any peer
// assert cluster-owner for an arbitrary subject, so the receiving node
// re-resolves it against the graph instead. Whoever wires SetQueryExecutor
// must wire this too; without it every forward is refused (memql#2814).
type ForwardedAccessResolver interface {
	LoadFromClaims(ctx context.Context, claims map[string]any) (*auth.AccessContext, error)
}

// AiForwardHandler is the worker-side entry point for a forwarded AI /
// voice request. The nodeService invokes it when an inbound
// NodeClientMessage carries an AiForwardRequest. The handler reads the
// request, dispatches to the appropriate local AI handler, and uses
// `send` to deliver each response back to the originating BFF wrapped
// in an AiForwardResponse.
//
// Left as an interface (rather than concrete type) to keep component/node/
// independent of component/grpc/. The gRPC service registers a concrete
// implementation via SetAiForwardHandler during bootstrap.
type AiForwardHandler interface {
	HandleForwardedRequest(ctx context.Context, req *nodev1.AiForwardRequest, send func(*nodev1.NodeServerMessage) error)
	CancelForwardedRequest(ctx context.Context, requestId string)
}

// AiForwardResponseSink is the BFF-side receiver for responses to
// forwards originated here. Each inbound AiForwardResponse on a peer
// connection is routed through this sink (registered via
// SetAiForwardResponseSink). On the gRPC side an AiForwardRouter
// satisfies this contract.
type AiForwardResponseSink interface {
	Dispatch(resp *nodev1.AiForwardResponse)
}

// WorkbenchForwardHandler is the workbench-node-side entry point
// for a forwarded workbench dispatch. The nodeService invokes it
// when an inbound NodeClientMessage carries a WorkbenchForwardRequest.
// Single round-trip semantics (no streaming): the handler executes
// the action, builds a response, and uses `send` to deliver exactly
// one WorkbenchForwardResponse back to the originating agent node.
//
// Left as an interface so component/node/ stays independent of the
// concrete workbench integration. The workbench node-type build's
// transport wires a concrete implementation via SetWorkbenchHandler.
type WorkbenchForwardHandler interface {
	HandleForwardedRequest(ctx context.Context, req *nodev1.WorkbenchForwardRequest, send func(*nodev1.NodeServerMessage) error)
	CancelForwardedRequest(ctx context.Context, requestId string)
}

// WorkbenchForwardResponseSink is the agent-side receiver for
// responses to workbench dispatches originated here. Each inbound
// WorkbenchForwardResponse on a peer connection is routed through
// this sink. The agent-side WorkbenchForwardRouter satisfies this.
type WorkbenchForwardResponseSink interface {
	Dispatch(resp *nodev1.WorkbenchForwardResponse)
}

// EventInbound accepts a peer-forwarded event, dedups it against
// recently-seen event IDs + TTL, and republishes it on the local event
// bus so local subscribers (integrations, automations, gRPC subscribers)
// see it. *EventBridge satisfies this contract. Without an implementation
// wired in, nodeService.handleEventForward logs and ACKs but never
// publishes, which silently breaks every cross-node subscriber.
type EventInbound interface {
	HandleInbound(evt *nodev1.EventForward)
	ForwardInboundToPeers(evt *nodev1.EventForward, excludeNodeId string)
}

// nodeService implements the NodeService gRPC server.
type nodeService struct {
	nodev1.UnimplementedNodeServiceServer

	logger                   *slog.Logger
	identity                 *Identity
	peerManager              *PeerManager
	queryExecutor            QueryExecutor                // set via SetQueryExecutor for cross-node query forwarding
	accessResolver           ForwardedAccessResolver      // set via SetIdentityResolver; required for forwarded queries
	aiForwardHandler         AiForwardHandler             // worker-side handler; nil on BFF binaries
	aiForwardResponse        AiForwardResponseSink        // BFF-side response sink; nil on worker binaries
	workbenchForwardHandler  WorkbenchForwardHandler      // workbench-side handler; nil on non-workbench binaries
	workbenchForwardResponse WorkbenchForwardResponseSink // agent-side response sink; nil on non-agent binaries
	eventInbound             EventInbound                 // bridges peer-forwarded events onto the local bus
}

// Stream handles a bidirectional streaming connection from a peer node.
func (s *nodeService) Stream(stream nodev1.NodeService_StreamServer) error {
	// First message must be NodeHello
	firstMsg, err := stream.Recv()
	if err != nil {
		return err
	}

	hello := firstMsg.GetNodeHello()
	if hello == nil {
		return stream.Send(&nodev1.NodeServerMessage{
			MessageId: id.NewShortId(),
			Payload: &nodev1.NodeServerMessage_NodeShutdown{
				NodeShutdown: &nodev1.NodeShutdown{
					Reason:       "first message must be NodeHello",
					GraceSeconds: 0,
				},
			},
		})
	}

	peerId := hello.NodeId
	peerType := hello.NodeType

	// Cross-check NodeHello against the verified node-class token's
	// binding (#105). When the auth interceptor is installed, every
	// stream carries a (boundNodeId, boundNodeType) tuple extracted
	// from the JWT's `node_id` / `node_type` claims; the NodeHello
	// the peer announces must match. A token minted for nodeId=A
	// cannot drive a stream that NodeHello's as nodeId=B. When the
	// interceptor isn't wired the binding is absent and we accept
	// whatever the peer claims (legacy behavior).
	if boundId, boundType, ok := NodeBindingFromContext(stream.Context()); ok {
		if peerId != boundId || peerType != boundType {
			s.logger.Warn("node hello rejected: token binding mismatch",
				"hello_id", peerId,
				"hello_type", peerType,
				"bound_id", boundId,
				"bound_type", boundType,
			)
			return stream.Send(&nodev1.NodeServerMessage{
				MessageId: id.NewShortId(),
				Payload: &nodev1.NodeServerMessage_NodeShutdown{
					NodeShutdown: &nodev1.NodeShutdown{
						Reason:       "NodeHello.NodeId / NodeType does not match the verified token's binding",
						GraceSeconds: 0,
					},
				},
			})
		}
	}

	s.logger.Info("peer connected",
		"peer_id", peerId,
		"peer_type", peerType,
		"peer_address", hello.Address,
		"peer_version", hello.Version,
	)

	// Register the peer. This is a direct inbound stream, so the peer is
	// monitored for liveness: this node's checkLiveness will trip
	// degraded/offline if heartbeats stop arriving.
	peerInfo := &nodev1.PeerInfo{
		NodeId:       peerId,
		NodeType:     peerType,
		Address:      hello.Address,
		Health:       nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY,
		Capabilities: hello.Capabilities,
		Labels:       hello.Labels,
	}
	s.peerManager.RegisterMonitored(peerInfo)

	// Send NodeWelcome with the server's own identity + peer table.
	// NodeWelcome.node_id is the SERVER's ID -- the client already
	// knows its own, but it needs the server's so it can register the
	// server as a peer (see parent_connector / worker_dialer's
	// NodeWelcome handling).
	welcome := &nodev1.NodeServerMessage{
		MessageId:   id.NewShortId(),
		CorrelateTo: firstMsg.MessageId,
		Payload: &nodev1.NodeServerMessage_NodeWelcome{
			NodeWelcome: &nodev1.NodeWelcome{
				NodeId:   s.identity.ID,
				NodeType: string(s.identity.Type),
				Peers:    s.peerManager.PeerInfoList(),
			},
		},
	}
	if err := stream.Send(welcome); err != nil {
		// NodeWelcome failed before the peer was fully integrated. This is
		// the only case where immediate Remove is correct: the handshake
		// never completed, so other peers don't know about this one yet.
		s.peerManager.Remove(peerId)
		return err
	}

	// Announce the new peer to existing peers via PeerIntroduction
	s.broadcastPeerIntroduction([]*nodev1.PeerInfo{peerInfo}, false)

	// Per-stream heartbeat: the client sends heartbeats to us via its
	// peerConnection.heartbeatLoop, but there's no equivalent outbound
	// ticker on the server side. Without one, the client's PeerManager
	// sees a silent server and degrades it to OFFLINE. Fire a stop
	// channel so the goroutine exits when this handler returns.
	stopHB := make(chan struct{})
	defer close(stopHB)
	go s.serverHeartbeatLoop(stream, stopHB)

	// Enter bidirectional message loop.
	//
	// On stream exit we deliberately do NOT Remove the peer and do NOT
	// broadcast a PeerIntroduction(removal=true). Transient stream drops
	// (network hiccup, gRPC keepalive timeout, client restart) are
	// indistinguishable from real departures at this point, and eagerly
	// removing on every drop creates three concrete problems:
	//
	//  1. The peer entry vanishes from THIS node's routing table, so any
	//     event forwarded via AllPeers()/ByType() during the reconnect
	//     window silently skips the peer. Observed live: cognition-emitted
	//     v1:cognition:client:tool:request events never reached BFF when
	//     cognition's ParentConnector stream flapped, even though BFF's
	//     own outbound WorkerDialer connection to cognition was fine --
	//     BFF had removed its cognition entry based on the inbound drop.
	//  2. The PeerIntroduction(removal=true) broadcast cascades the same
	//     deletion to every sibling, so a transient drop ripples through
	//     the mesh and every node loses its routing entry until the peer
	//     finishes reconnecting.
	//  3. If BFF has an independent outbound connection to the dropped
	//     peer (worker_dialer), that connection stays alive but unusable
	//     because the peer entry is gone.
	//
	// Correct behavior: leave the entry. The liveness checker (peer.go
	// checkLiveness) owns the "really gone" decision and will transition
	// the peer to OFFLINE + remove it after offlineTimeout if heartbeats
	// stop arriving. That's the single authoritative source of truth for
	// peer lifecycle, instead of every transient stream exit racing it.
	defer func() {
		s.logger.Info("peer disconnected",
			"peer_id", peerId,
			"peer_type", peerType,
			"note", "entry retained; liveness checker owns eventual removal",
		)
	}()

	for {
		msg, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}

		s.handleMessage(peerId, msg, stream)
	}
}

// serverHeartbeatLoop emits a NodeServerMessage{Heartbeat} on the
// configured interval for the lifetime of a peer stream. Returns when
// stop is closed (the outer Stream handler returns) or the send fails.
// Send errors are tolerated -- the client-side recv will see the same
// error and trigger its own reconnect.
func (s *nodeService) serverHeartbeatLoop(stream nodev1.NodeService_StreamServer, stop <-chan struct{}) {
	interval := s.peerManager.HeartbeatInterval()
	if interval <= 0 {
		return
	}
	// Send one immediately so the peer's LastSeen is refreshed on
	// NodeWelcome and every interval thereafter.
	if err := stream.Send(buildServerHeartbeat(s.selfHealth())); err != nil {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			if err := stream.Send(buildServerHeartbeat(s.selfHealth())); err != nil {
				return
			}
		}
	}
}

// selfHealth returns this node's self-asserted lifecycle health for gossip
// (memql#1268). Defaults to HEALTHY when no PeerManager/lifecycle is wired so
// the server heartbeat keeps its pre-lifecycle behaviour.
func (s *nodeService) selfHealth() nodev1.NodeHealthStatus {
	if s.peerManager == nil {
		return nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY
	}
	if lc := s.peerManager.Lifecycle(); lc != nil {
		return lc.Health()
	}
	return nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY
}

// buildServerHeartbeat constructs a NodeServerMessage{Heartbeat} for the
// server-side heartbeat ticker. Mirrors buildHeartbeatMessage but with
// the server-direction envelope type. health carries this node's
// self-asserted lifecycle health (memql#1268).
func buildServerHeartbeat(health nodev1.NodeHealthStatus) *nodev1.NodeServerMessage {
	if health == nodev1.NodeHealthStatus_NODE_HEALTH_UNSPECIFIED {
		health = nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY
	}
	return &nodev1.NodeServerMessage{
		MessageId: id.NewShortId(),
		Payload: &nodev1.NodeServerMessage_Heartbeat{
			Heartbeat: &nodev1.NodeHeartbeat{
				Ts:     timestamppb.New(time.Now()),
				Health: health,
			},
		},
	}
}

// handleMessage dispatches an incoming message by payload type.
func (s *nodeService) handleMessage(peerId string, msg *nodev1.NodeClientMessage, stream nodev1.NodeService_StreamServer) {
	switch payload := msg.Payload.(type) {

	case *nodev1.NodeClientMessage_Heartbeat:
		s.handleHeartbeat(peerId, payload.Heartbeat)

	case *nodev1.NodeClientMessage_PeerIntro:
		s.handlePeerIntroduction(payload.PeerIntro)

	case *nodev1.NodeClientMessage_SpawnRequest:
		s.handleSpawnRequest(peerId, payload.SpawnRequest, stream)

	case *nodev1.NodeClientMessage_EventForward:
		s.handleEventForward(peerId, payload.EventForward, stream)

	case *nodev1.NodeClientMessage_CapabilityQuery:
		s.handleCapabilityQuery(peerId, payload.CapabilityQuery, stream)

	case *nodev1.NodeClientMessage_QueryForward:
		s.handleQueryForward(peerId, payload.QueryForward, stream)

	case *nodev1.NodeClientMessage_AiForwardRequest:
		s.handleAiForwardRequest(peerId, payload.AiForwardRequest, stream)

	case *nodev1.NodeClientMessage_AiForwardCancel:
		s.handleAiForwardCancel(peerId, payload.AiForwardCancel)

	case *nodev1.NodeClientMessage_WorkbenchForwardRequest:
		s.handleWorkbenchForwardRequest(peerId, payload.WorkbenchForwardRequest, stream)

	case *nodev1.NodeClientMessage_WorkbenchForwardCancel:
		s.handleWorkbenchForwardCancel(peerId, payload.WorkbenchForwardCancel)

	default:
		s.logger.Debug("unhandled message type from peer",
			"peer_id", peerId,
			"message_id", msg.MessageId,
		)
	}
}

// handleHeartbeat updates the peer's liveness timestamp and health status.
// TouchPeer handles the DEGRADED/CONNECTING -> HEALTHY transition; any
// other peer-reported status (e.g. DRAINING) goes through UpdatePeerHealth
// so the transition fires through the StatusChangeHandler.
func (s *nodeService) handleHeartbeat(peerId string, hb *nodev1.NodeHeartbeat) {
	s.peerManager.TouchPeer(peerId)
	s.peerManager.UpdatePeerHealth(peerId, hb.Health)
}

// handlePeerIntroduction processes a peer table update from another node.
func (s *nodeService) handlePeerIntroduction(intro *nodev1.PeerIntroduction) {
	for _, peer := range intro.Peers {
		if peer.NodeId == s.identity.ID {
			continue // skip self
		}
		if intro.Removal {
			s.peerManager.Remove(peer.NodeId)
		} else {
			s.peerManager.Register(peer)
		}
	}
}

// handleSpawnRequest responds to a spawn request with an unsupported error.
// Spawn requests are no longer handled; container lifecycle is managed externally.
func (s *nodeService) handleSpawnRequest(peerId string, req *nodev1.SpawnRequest, stream nodev1.NodeService_StreamServer) {
	s.logger.Warn("spawn request not supported",
		"peer_id", peerId,
		"request_id", req.RequestId,
		"node_type", req.NodeType,
	)

	_ = stream.Send(&nodev1.NodeServerMessage{
		MessageId:   id.NewShortId(),
		CorrelateTo: req.RequestId,
		Payload: &nodev1.NodeServerMessage_SpawnResult{
			SpawnResult: &nodev1.SpawnResult{
				RequestId: req.RequestId,
				Success:   false,
				Error:     "spawn requests are not supported",
			},
		},
	})
}

// handleEventForward processes a forwarded event from another node. The
// event is handed to the EventInbound implementation (EventBridge), which
// dedups against recently-seen IDs, enforces TTL, and publishes to the
// local event bus. It is then re-forwarded to other peers (excluding the
// sender) so a mesh topology still propagates events across the cluster;
// in a star topology this is a no-op because workers have no other
// outbound peer connections. Finally we ACK the event.
//
// Without the HandleInbound call, cross-node subscribers on this node
// never fire -- the event arrives in the networking layer and stops
// there. That was the concrete cause of "cognition receives the
// utterance event but handleUtteranceForCognition never runs."
func (s *nodeService) handleEventForward(peerId string, evt *nodev1.EventForward, stream nodev1.NodeService_StreamServer) {
	s.logger.Debug("event forwarded from peer",
		"peer_id", peerId,
		"event_id", evt.EventId,
		"topic", evt.Topic,
		"ttl", evt.Ttl,
	)

	if s.eventInbound != nil {
		s.eventInbound.HandleInbound(evt)
		s.eventInbound.ForwardInboundToPeers(evt, peerId)
	}

	// ACK the event
	_ = stream.Send(&nodev1.NodeServerMessage{
		MessageId:   id.NewShortId(),
		CorrelateTo: evt.EventId,
		Payload: &nodev1.NodeServerMessage_EventAck{
			EventAck: &nodev1.EventAck{
				EventId: evt.EventId,
			},
		},
	})
}

// handleCapabilityQuery responds to a capability lookup request.
func (s *nodeService) handleCapabilityQuery(peerId string, query *nodev1.CapabilityQuery, stream nodev1.NodeService_StreamServer) {
	peers := s.peerManager.ByCapability(query.CapabilityName)

	resp := &nodev1.CapabilityResponse{
		RequestId: query.RequestId,
		Available: len(peers) > 0,
	}

	if len(peers) > 0 {
		resp.NodeId = peers[0].Info.NodeId
		resp.Address = peers[0].Info.Address
	}

	_ = stream.Send(&nodev1.NodeServerMessage{
		MessageId:   id.NewShortId(),
		CorrelateTo: query.RequestId,
		Payload: &nodev1.NodeServerMessage_CapabilityResponse{
			CapabilityResponse: resp,
		},
	})
}

// broadcastPeerIntroduction sends a PeerIntroduction to all connected child nodes.
func (s *nodeService) broadcastPeerIntroduction(peers []*nodev1.PeerInfo, removal bool) {
	// This will be enhanced in later phases to broadcast to all connected peers
	_ = &nodev1.PeerIntroduction{
		Peers:   peers,
		Removal: removal,
	}
}

// buildHeartbeatMessage constructs a NodeHeartbeat envelope for the given
// SetQueryExecutor configures the engine used to execute forwarded queries.
// Called during bootstrap to wire the engine into the NodeService handler.
func (s *nodeService) SetQueryExecutor(executor QueryExecutor) {
	s.queryExecutor = executor
}

// SetIdentityResolver installs the database-backed resolver used to turn a
// forwarded claims map into the caller's AccessContext. Required alongside
// SetQueryExecutor: without it every forwarded query is refused, because the
// alternative -- trusting the claims on the wire -- would let a peer assert
// cluster-owner for an arbitrary subject (memql#2814).
func (s *nodeService) SetIdentityResolver(r ForwardedAccessResolver) {
	s.accessResolver = r
}

// resolveForwardedAccess resolves the forwarded claims to a real principal,
// or returns an error. It never falls back to claim-derived authority: on this
// path the claims are an unsigned assertion from a peer, not a verified token
// on the caller's own stream.
func (s *nodeService) resolveForwardedAccess(ctx context.Context, claims map[string]any) (*auth.AccessContext, error) {
	if len(claims) == 0 {
		return nil, errors.New("forwarded query carries no auth claims")
	}
	if s.accessResolver == nil {
		return nil, errors.New("no identity resolver wired (SetIdentityResolver); refusing to derive authority from forwarded claims")
	}
	access, err := s.accessResolver.LoadFromClaims(ctx, claims)
	if err != nil {
		return nil, err
	}
	if access == nil || strings.TrimSpace(access.UserId) == "" {
		return nil, errors.New("identity resolver returned no principal")
	}
	return access, nil
}

// SetAiForwardHandler installs the worker-side handler invoked for
// inbound AiForwardRequest messages. Left nil on BFF binaries (they
// don't execute forwards locally; they only originate them).
func (s *nodeService) SetAiForwardHandler(h AiForwardHandler) {
	s.aiForwardHandler = h
}

// SetAiForwardResponseSink installs the BFF-side sink for inbound
// AiForwardResponse messages. Left nil on worker binaries (they never
// receive responses; they only originate them).
func (s *nodeService) SetAiForwardResponseSink(sink AiForwardResponseSink) {
	s.aiForwardResponse = sink
}

// SetWorkbenchForwardHandler installs the workbench-side handler
// invoked for inbound WorkbenchForwardRequest messages. Left nil on
// non-workbench binaries.
func (s *nodeService) SetWorkbenchForwardHandler(h WorkbenchForwardHandler) {
	s.workbenchForwardHandler = h
}

// SetWorkbenchForwardResponseSink installs the agent-side sink for
// inbound WorkbenchForwardResponse messages. Left nil on
// non-agent / non-router binaries.
func (s *nodeService) SetWorkbenchForwardResponseSink(sink WorkbenchForwardResponseSink) {
	s.workbenchForwardResponse = sink
}

// handleAiForwardRequest dispatches a forwarded AI/voice request to the
// registered handler and streams responses back on the peer's stream.
func (s *nodeService) handleAiForwardRequest(peerId string, req *nodev1.AiForwardRequest, stream nodev1.NodeService_StreamServer) {
	if s.aiForwardHandler == nil {
		s.logger.Warn("ai forward request received but no handler configured",
			"peer_id", peerId, "request_id", req.GetRequestId(),
		)
		_ = stream.Send(&nodev1.NodeServerMessage{
			MessageId:   id.NewShortId(),
			CorrelateTo: req.GetRequestId(),
			Payload: &nodev1.NodeServerMessage_AiForwardResponse{
				AiForwardResponse: &nodev1.AiForwardResponse{
					RequestId: req.GetRequestId(),
					Done:      true,
				},
			},
		})
		return
	}
	// Dispatch synchronously on the stream read loop so multi-envelope
	// flows (streaming transcription: Start / Chunk / End all share the
	// same request_id) see consistent ordering. The AI handlers spawn
	// their own worker goroutines for long-running work, so this does
	// not block the receive path for more than session setup (a few
	// tens of ms at worst).
	s.aiForwardHandler.HandleForwardedRequest(stream.Context(), req, stream.Send)
}

// handleAiForwardCancel forwards a cancel notification to the worker-side
// handler so it can release resources held for an in-flight request.
func (s *nodeService) handleAiForwardCancel(peerId string, cancel *nodev1.AiForwardCancel) {
	if cancel == nil || s.aiForwardHandler == nil {
		return
	}
	s.aiForwardHandler.CancelForwardedRequest(context.Background(), cancel.GetRequestId())
}

// handleWorkbenchForwardRequest dispatches an inbound workbench
// dispatch envelope to the configured local handler. Single
// request -> single response: the handler emits exactly one
// WorkbenchForwardResponse via `send` and returns. When no handler
// is configured (e.g. mis-typed node, missing wiring), responds
// with a `not_configured` error so the agent's tool loop fails
// fast instead of timing out.
func (s *nodeService) handleWorkbenchForwardRequest(peerId string, req *nodev1.WorkbenchForwardRequest, stream nodev1.NodeService_StreamServer) {
	if s.workbenchForwardHandler == nil {
		s.logger.Warn("workbench forward request received but no handler configured",
			"peer_id", peerId, "request_id", req.GetRequestId(),
		)
		_ = stream.Send(&nodev1.NodeServerMessage{
			MessageId:   id.NewShortId(),
			CorrelateTo: req.GetRequestId(),
			Payload: &nodev1.NodeServerMessage_WorkbenchForwardResponse{
				WorkbenchForwardResponse: &nodev1.WorkbenchForwardResponse{
					RequestId:    req.GetRequestId(),
					ErrorCode:    "not_configured",
					ErrorMessage: "workbench handler not configured on this node",
				},
			},
		})
		return
	}
	s.workbenchForwardHandler.HandleForwardedRequest(stream.Context(), req, stream.Send)
}

// handleWorkbenchForwardCancel forwards a cancel notification to the
// workbench-side handler so it can stop in-flight work for the
// request_id (kill exec, abort http_fetch).
func (s *nodeService) handleWorkbenchForwardCancel(peerId string, cancel *nodev1.WorkbenchForwardCancel) {
	if cancel == nil || s.workbenchForwardHandler == nil {
		return
	}
	s.workbenchForwardHandler.CancelForwardedRequest(context.Background(), cancel.GetRequestId())
}

// handleQueryForward executes a forwarded query locally and sends the
// result back to the requesting node. This is the server-side handler
// for cross-node query routing: the requesting node's QueryProxy
// determined that this node owns the target concept, forwarded the
// query, and now we execute it and return the result.
func (s *nodeService) handleQueryForward(peerId string, req *nodev1.QueryForward, stream nodev1.NodeService_StreamServer) {
	if s.queryExecutor == nil {
		s.logger.Warn("query forward received but no query executor configured",
			"peer_id", peerId,
			"request_id", req.RequestId,
		)
		_ = stream.Send(&nodev1.NodeServerMessage{
			CorrelateTo: req.RequestId,
			Payload: &nodev1.NodeServerMessage_QueryResponse{
				QueryResponse: &nodev1.QueryResponse{
					RequestId: req.RequestId,
					Success:   false,
					Error:     "query executor not configured on this node",
				},
			},
		})
		return
	}

	// Rebuild the originating caller's identity before executing anything.
	// Identity does NOT travel implicitly across a mesh hop: this handler
	// used to run forwarded DSL on context.Background(), so the query
	// executed with NO actor at all (memql#2814). Combined with the
	// deny-on-nil default (#2801) that silently returns zero rows for any
	// actor-gated construct; before that default it failed the other way,
	// resolving isClusterOwner=true and serving every partition's rows.
	//
	// TWO context keys are required, and attaching only the first is the
	// trap: ContextWithForwardedClaims sets TokenInfo + Claims, but every
	// engine actor surface (resolveActorPath, the spec evaluator, mutation
	// templates, result-cache policy, the automation actor binding) reads
	// auth.AccessFromContext. Claims alone still yield the deny-all envelope
	// -- userId "", isClusterOwner false -- i.e. the exact zero-row symptom
	// above.
	//
	// The AccessContext is resolved AGAINST THE DATABASE, never derived from
	// the forwarded claims. That distinction is the security boundary here:
	// auth.FallbackFromClaims would take `role` straight off the wire, so a
	// peer sending {"sub":"anyone","role":"owner"} would execute as cluster
	// owner (IsClusterOwner() is just Role==RoleOwner) against a surface that
	// dispatches mutations and logic, not just reads. The resolver enforces
	// what the claims cannot: a canonical v1:identity:user:* subject, an
	// existing user row, and the role as stored. This is the resolver half of
	// component/grpc/server.go's ensureAccess bridge (memql#216) -- but NOT
	// its claims fallback, which exists there to survive a brand-new user
	// racing the magic-link insert on the caller's OWN stream. Across a mesh
	// hop that fallback is just an unsigned assertion, so this path fails
	// closed instead.
	//
	// Claims are read from req.GetAuth() DIRECTLY. Round-tripping them
	// through the context would silently fall back to whatever identity the
	// peer's own stream carries when the envelope's map is empty -- a forward
	// with no auth would then execute as the sending NODE.
	claims := make(map[string]any, len(req.GetAuth()))
	for k, v := range req.GetAuth() {
		claims[k] = v
	}

	access, resolveErr := s.resolveForwardedAccess(stream.Context(), claims)
	if resolveErr != nil {
		s.logger.Warn("refusing forwarded query: caller identity did not resolve",
			"peer_id", peerId,
			"request_id", req.GetRequestId(),
			"has_auth_claims", len(req.GetAuth()) > 0,
			"error", resolveErr,
		)
		_ = stream.Send(&nodev1.NodeServerMessage{
			CorrelateTo: req.GetRequestId(),
			Payload: &nodev1.NodeServerMessage_QueryResponse{
				QueryResponse: &nodev1.QueryResponse{
					RequestId: req.GetRequestId(),
					Success:   false,
					// Deliberately does not echo resolveErr: the peer learns
					// that identity failed, not whether a given subject exists.
					Error: "forwarded query has no resolvable caller identity: set QueryForward.auth from auth.ForwardedClaimsFromIdentity on the forwarding node, and wire SetIdentityResolver on this one (memql#2814)",
				},
			},
		})
		return
	}

	// stream.Context(), not context.Background(): the forwarded execution
	// should die with the peer stream that asked for it.
	ctx := auth.ContextWithForwardedClaims(stream.Context(), req.GetAuth())
	ctx = auth.ContextWithAccess(ctx, access)

	s.logger.Debug("executing forwarded query",
		"peer_id", peerId,
		"request_id", req.RequestId,
		"query", req.Query,
	)

	result, err := s.queryExecutor.Execute(ctx, req.Query)
	if err != nil {
		_ = stream.Send(&nodev1.NodeServerMessage{
			CorrelateTo: req.RequestId,
			Payload: &nodev1.NodeServerMessage_QueryResponse{
				QueryResponse: &nodev1.QueryResponse{
					RequestId: req.RequestId,
					Success:   false,
					Error:     err.Error(),
				},
			},
		})
		return
	}

	// Serialize the result to JSON for transport.
	var resultJSON []byte
	if result != nil && result.Bundle != nil {
		resultJSON, _ = json.Marshal(result.Bundle)
	}

	_ = stream.Send(&nodev1.NodeServerMessage{
		CorrelateTo: req.RequestId,
		Payload: &nodev1.NodeServerMessage_QueryResponse{
			QueryResponse: &nodev1.QueryResponse{
				RequestId:  req.RequestId,
				Success:    true,
				ResultJson: resultJSON,
			},
		},
	})
}

// health status, stamped with the current time. Used by the connection's
// heartbeat loop to enqueue heartbeats on the existing send channel.
func buildHeartbeatMessage(health nodev1.NodeHealthStatus) *nodev1.NodeClientMessage {
	return &nodev1.NodeClientMessage{
		MessageId: id.NewShortId(),
		Payload: &nodev1.NodeClientMessage_Heartbeat{
			Heartbeat: &nodev1.NodeHeartbeat{
				Ts:     timestamppb.New(time.Now()),
				Health: health,
			},
		},
	}
}
