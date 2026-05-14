package node

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"time"

	"github.com/google/uuid"
	memqlengine "github.com/visionarys-io/memql/component/memql"
	nodev1 "github.com/visionarys-io/memql/component/node/gen"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// QueryExecutor is the interface for executing MemQL queries.
// Satisfied by *memqlengine.MemQLEngine.
type QueryExecutor interface {
	Execute(ctx context.Context, query string) (*memqlengine.ExecuteResult, error)
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

	logger            *slog.Logger
	identity          *Identity
	peerManager       *PeerManager
	queryExecutor     QueryExecutor         // set via SetQueryExecutor for cross-node query forwarding
	aiForwardHandler  AiForwardHandler      // worker-side handler; nil on BFF binaries
	aiForwardResponse AiForwardResponseSink // BFF-side response sink; nil on worker binaries
	eventInbound      EventInbound          // bridges peer-forwarded events onto the local bus
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
			MessageId: uuid.New().String(),
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
		MessageId:   uuid.New().String(),
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
	if err := stream.Send(buildServerHeartbeat()); err != nil {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			if err := stream.Send(buildServerHeartbeat()); err != nil {
				return
			}
		}
	}
}

// buildServerHeartbeat constructs a NodeServerMessage{Heartbeat} for the
// server-side heartbeat ticker. Mirrors buildHeartbeatMessage but with
// the server-direction envelope type.
func buildServerHeartbeat() *nodev1.NodeServerMessage {
	return &nodev1.NodeServerMessage{
		MessageId: uuid.New().String(),
		Payload: &nodev1.NodeServerMessage_Heartbeat{
			Heartbeat: &nodev1.NodeHeartbeat{
				Ts:     timestamppb.New(time.Now()),
				Health: nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY,
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
		MessageId:   uuid.New().String(),
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
		MessageId:   uuid.New().String(),
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
		MessageId:   uuid.New().String(),
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

// handleAiForwardRequest dispatches a forwarded AI/voice request to the
// registered handler and streams responses back on the peer's stream.
func (s *nodeService) handleAiForwardRequest(peerId string, req *nodev1.AiForwardRequest, stream nodev1.NodeService_StreamServer) {
	if s.aiForwardHandler == nil {
		s.logger.Warn("ai forward request received but no handler configured",
			"peer_id", peerId, "request_id", req.GetRequestId(),
		)
		_ = stream.Send(&nodev1.NodeServerMessage{
			MessageId:   uuid.New().String(),
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

	s.logger.Debug("executing forwarded query",
		"peer_id", peerId,
		"request_id", req.RequestId,
		"query", req.Query,
	)

	result, err := s.queryExecutor.Execute(context.Background(), req.Query)
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
		MessageId: uuid.New().String(),
		Payload: &nodev1.NodeClientMessage_Heartbeat{
			Heartbeat: &nodev1.NodeHeartbeat{
				Ts:     timestamppb.New(time.Now()),
				Health: health,
			},
		},
	}
}
