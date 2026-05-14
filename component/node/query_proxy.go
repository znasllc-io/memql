package node

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
)

var (
	extraOwnershipMu sync.Mutex
	extraOwnership   = map[string]NodeType{}
)

// RegisterConceptOwnership declares that a concept-id prefix is owned by
// the given node type (or handled locally when nodeType == ""). Called
// from init() in product branches so main doesn't have to know about
// product concept namespaces. Build tags on the caller control which
// binaries include the registration. Last registration wins for a
// given prefix.
func RegisterConceptOwnership(prefix string, nodeType NodeType) {
	extraOwnershipMu.Lock()
	defer extraOwnershipMu.Unlock()
	extraOwnership[prefix] = nodeType
}

// QueryProxy enables nodes to forward MemQL queries to nodes that own the
// relevant concept domain. This is transparent to clients -- they send
// queries to any node, and the node routes them to the owner.
type QueryProxy struct {
	identity         *Identity
	peerManager      *PeerManager
	capabilityRouter *CapabilityRouter
	logger           *slog.Logger

	// conceptOwnership maps concept domain prefixes to node types.
	// e.g.,   "v1:cognition:" -> NodeTypeCognition
	conceptOwnership map[string]NodeType
}

// NewQueryProxy creates a QueryProxy with default concept ownership mappings.
func NewQueryProxy(identity *Identity, peerManager *PeerManager, capabilityRouter *CapabilityRouter, logger *slog.Logger) *QueryProxy {
	return &QueryProxy{
		identity:         identity,
		peerManager:      peerManager,
		capabilityRouter: capabilityRouter,
		logger:           logger,
		conceptOwnership: defaultConceptOwnership(),
	}
}

// defaultConceptOwnership returns the effective ownership map: the built-in
// core prefixes merged with any registered by product plug-ins via
// RegisterConceptOwnership.
func defaultConceptOwnership() map[string]NodeType {
	core := map[string]NodeType{
		"v1:cognition:": NodeTypeCognition,
		"v1:identity:":  "", // handle locally (identity primitive)
		"v1:data:":      "", // handle locally
		"v1:memql:":     "", // handle locally
		"v1:cluster:":   "", // handle locally
		"v1:platform:":  "", // handle locally
	}

	extraOwnershipMu.Lock()
	defer extraOwnershipMu.Unlock()
	for k, v := range extraOwnership {
		core[k] = v
	}
	return core
}

// ResolveTarget determines which node type should handle a query
// based on the concept references in the query string.
func (qp *QueryProxy) ResolveTarget(query string) NodeType {
	// Simple heuristic: look for concept== references in the query
	for prefix, nodeType := range qp.conceptOwnership {
		if nodeType != "" && strings.Contains(query, prefix) {
			return nodeType
		}
	}
	return "" // empty means handle locally
}

// FindPeerForQuery returns a peer that can handle the given query,
// or nil if the query should be handled locally.
func (qp *QueryProxy) FindPeerForQuery(ctx context.Context, query string) (*PeerEntry, error) {
	targetType := qp.ResolveTarget(query)
	if targetType == "" {
		return nil, nil // handle locally
	}

	peers := qp.peerManager.ByType(targetType)
	if len(peers) == 0 {
		return nil, fmt.Errorf("no %s node available to handle query", targetType)
	}

	// Pick the first healthy peer
	for _, peer := range peers {
		if peer.Info.Health == 0 || peer.Info.Health == 1 { // UNSPECIFIED or HEALTHY
			return peer, nil
		}
	}

	// All degraded, use first anyway
	qp.logger.Warn("all peers degraded, using first available",
		"target_type", string(targetType),
		"peer_count", len(peers),
	)
	return peers[0], nil
}
