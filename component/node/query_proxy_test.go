package node

import (
	"testing"

	nodev1 "github.com/visionarys-io/memql/component/node/gen"
)

func nodev1PeerInfo(id, nodeType, address string) *nodev1.PeerInfo {
	return &nodev1.PeerInfo{
		NodeId:   id,
		NodeType: nodeType,
		Address:  address,
		Health:   nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY,
	}
}

func TestQueryProxy_ResolveTarget(t *testing.T) {
	pm := NewPeerManager(testIdentity(), testLogger())
	cr := NewCapabilityRouter(testIdentity(), pm, testLogger())
	qp := NewQueryProxy(testIdentity(), pm, cr, testLogger())

	tests := []struct {
		query    string
		expected NodeType
	}{
		{"concept==v1:cognition:participant; ?.payload.spaceId==\"abc\"", NodeTypeCognition},
		{"concept==v1:cognition:utterance", NodeTypeCognition},
		{"concept==v1:identity:user", ""},  // handle locally
		{"concept==v1:data:record", ""},     // handle locally
		{"concept==v1:agents:agent", ""},   // handle locally
		{"concept==v1:cluster:node", ""},    // handle locally
		{"some random query", ""},           // no concept, handle locally
	}

	for _, tt := range tests {
		result := qp.ResolveTarget(tt.query)
		if result != tt.expected {
			t.Errorf("ResolveTarget(%q) = %q, want %q", tt.query, result, tt.expected)
		}
	}
}

func TestQueryProxy_FindPeerForQuery_Local(t *testing.T) {
	pm := NewPeerManager(testIdentity(), testLogger())
	cr := NewCapabilityRouter(testIdentity(), pm, testLogger())
	qp := NewQueryProxy(testIdentity(), pm, cr, testLogger())

	// Query with no specific domain should return nil (handle locally)
	peer, err := qp.FindPeerForQuery(nil, "concept==v1:agents:agent")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if peer != nil {
		t.Error("expected nil peer for local query")
	}
}

func TestQueryProxy_FindPeerForQuery_NoPeer(t *testing.T) {
	pm := NewPeerManager(testIdentity(), testLogger())
	cr := NewCapabilityRouter(testIdentity(), pm, testLogger())
	qp := NewQueryProxy(testIdentity(), pm, cr, testLogger())

	// Query targeting cognition but no cognition peers registered
	_, err := qp.FindPeerForQuery(nil, "concept==v1:cognition:participant")
	if err == nil {
		t.Error("expected error when no cognition peer available")
	}
}

func TestQueryProxy_FindPeerForQuery_WithPeer(t *testing.T) {
	pm := NewPeerManager(testIdentity(), testLogger())
	cr := NewCapabilityRouter(testIdentity(), pm, testLogger())
	qp := NewQueryProxy(testIdentity(), pm, cr, testLogger())

	// Register a cognition peer
	pm.Register(nodev1PeerInfo("cognition-1", string(NodeTypeCognition), "c1:50052"))

	peer, err := qp.FindPeerForQuery(nil, "concept==v1:cognition:participant")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if peer == nil {
		t.Fatal("expected peer for cognition query")
	}
	if peer.Info.NodeId != "cognition-1" {
		t.Errorf("expected cognition-1, got %s", peer.Info.NodeId)
	}
}

func TestBootstrapFor(t *testing.T) {
	tests := []struct {
		nodeType    NodeType
		description string
	}{
		{NodeTypeCognition, "cognition"},
		{NodeTypeAgent, "agent"},
		{NodeTypePlanner, "planner"},
		{NodeTypeBFF, "bff"},
	}

	for _, tt := range tests {
		b := BootstrapFor(tt.nodeType)
		if b == nil {
			t.Fatalf("BootstrapFor(%q) returned nil", tt.nodeType)
		}
		desc := b.Description()
		if desc == "" {
			t.Errorf("BootstrapFor(%q).Description() is empty", tt.nodeType)
		}
	}
}
