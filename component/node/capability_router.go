package node

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
)

const (
	capabilityQueryTimeout = 5 * time.Second
)

// CapabilityRouter resolves which node can handle a given capability.
// When a capability is not available locally, it queries peers via the
// NodeService CapabilityQuery/CapabilityResponse exchange.
type CapabilityRouter struct {
	identity    *Identity
	peerManager *PeerManager
	logger      *slog.Logger
}

// NewCapabilityRouter creates a CapabilityRouter.
func NewCapabilityRouter(identity *Identity, peerManager *PeerManager, logger *slog.Logger) *CapabilityRouter {
	return &CapabilityRouter{
		identity:    identity,
		peerManager: peerManager,
		logger:      logger,
	}
}

// RouteResult contains the outcome of a capability lookup.
type RouteResult struct {
	// Local is true if this node can handle the capability.
	Local bool

	// NodeId is the ID of the node that can handle the capability.
	NodeId string

	// Address is the gRPC address of the node.
	Address string
}

// Route finds a node that can handle the given capability FQN.
// It first checks local peers, then sends CapabilityQuery messages
// to connected peers if no local match is found.
func (r *CapabilityRouter) Route(ctx context.Context, capabilityFQN string) (*RouteResult, error) {
	// Check peers that advertise this capability
	peers := r.peerManager.ByCapability(capabilityFQN)
	if len(peers) > 0 {
		peer := peers[0] // Pick the first available peer
		return &RouteResult{
			Local:   false,
			NodeId:  peer.Info.NodeId,
			Address: peer.Info.Address,
		}, nil
	}

	// No peer found with this capability -- query connected peers
	result, err := r.queryPeers(ctx, capabilityFQN)
	if err != nil {
		return nil, err
	}
	if result != nil {
		return result, nil
	}

	return nil, fmt.Errorf("no node found for capability %q", capabilityFQN)
}

// queryPeers sends CapabilityQuery to all connected peers and returns
// the first positive response.
func (r *CapabilityRouter) queryPeers(ctx context.Context, capabilityFQN string) (*RouteResult, error) {
	allPeers := r.peerManager.AllPeers()
	if len(allPeers) == 0 {
		return nil, nil
	}

	requestId := uuid.New().String()
	msg := &nodev1.NodeClientMessage{
		MessageId: uuid.New().String(),
		Payload: &nodev1.NodeClientMessage_CapabilityQuery{
			CapabilityQuery: &nodev1.CapabilityQuery{
				RequestId:      requestId,
				CapabilityName: capabilityFQN,
			},
		},
	}

	// Send to all connected peers
	sent := 0
	for _, peer := range allPeers {
		if peer.Connection != nil {
			peer.Connection.Send(msg)
			sent++
		}
	}

	if sent == 0 {
		return nil, nil
	}

	r.logger.Debug("capability query sent",
		"capability", capabilityFQN,
		"request_id", requestId,
		"peers_queried", sent,
	)

	// Note: In a full implementation, we would wait for CapabilityResponse
	// messages on the stream. For now, the peer table lookup above handles
	// the common case. The CapabilityQuery mechanism enables discovery of
	// capabilities across the mesh that aren't directly registered.
	return nil, nil
}
