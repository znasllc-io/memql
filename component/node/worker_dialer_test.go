package node

import (
	"testing"

	nodev1 "github.com/znasllc-io/memql/component/node/gen"
)

func TestParseWorkerPeers_Empty(t *testing.T) {
	if got := ParseWorkerPeers(""); got != nil {
		t.Errorf("expected nil for empty input, got %v", got)
	}
	if got := ParseWorkerPeers("   "); got != nil {
		t.Errorf("expected nil for whitespace-only input, got %v", got)
	}
}

func TestParseWorkerPeers_Basic(t *testing.T) {
	in := "voice=voice:50059,agent=agent:50055,cognition=cognition:50054,planner=planner:50056"
	got := ParseWorkerPeers(in)
	if len(got) != 4 {
		t.Fatalf("expected 4 targets, got %d: %v", len(got), got)
	}
	wantByType := map[NodeType]string{
		NodeTypeVoice:     "voice:50059",
		NodeTypeAgent:     "agent:50055",
		NodeTypeCognition: "cognition:50054",
		NodeTypePlanner:   "planner:50056",
	}
	for _, target := range got {
		if wantByType[target.NodeType] != target.Address {
			t.Errorf("type %s: want %s, got %s", target.NodeType, wantByType[target.NodeType], target.Address)
		}
	}
}

func TestParseWorkerPeers_Whitespace(t *testing.T) {
	in := "  agent = agent:50055 ,  voice=voice:50059  "
	got := ParseWorkerPeers(in)
	if len(got) != 2 {
		t.Fatalf("expected 2 targets, got %d: %v", len(got), got)
	}
}

func TestParseWorkerPeers_CaseInsensitiveType(t *testing.T) {
	got := ParseWorkerPeers("AGENT=agent:50055,Voice=voice:50059")
	if len(got) != 2 {
		t.Fatalf("expected 2 targets, got %d: %v", len(got), got)
	}
	for _, target := range got {
		if target.NodeType != NodeTypeAgent && target.NodeType != NodeTypeVoice {
			t.Errorf("unexpected node type: %s", target.NodeType)
		}
	}
}

func TestParseWorkerPeers_SkipsUnknownOrBFF(t *testing.T) {
	// BFF is a valid node type but not a worker type (we'd never forward
	// to it), so it must be rejected even though ValidNodeTypes says yes.
	got := ParseWorkerPeers("bff=bff:50058,nonsense=whatever:50060,agent=agent:50055")
	if len(got) != 1 {
		t.Fatalf("expected 1 target (only agent), got %d: %v", len(got), got)
	}
	if got[0].NodeType != NodeTypeAgent {
		t.Errorf("expected NodeTypeAgent, got %s", got[0].NodeType)
	}
}

func TestParseWorkerPeers_SkipsMalformed(t *testing.T) {
	got := ParseWorkerPeers(",,=addr,type=,agent=agent:50055,garbage")
	if len(got) != 1 {
		t.Fatalf("expected 1 target, got %d: %v", len(got), got)
	}
	if got[0].Address != "agent:50055" {
		t.Errorf("expected agent:50055, got %s", got[0].Address)
	}
}

func TestParseWorkerPeers_DedupesByTypeAndAddress(t *testing.T) {
	got := ParseWorkerPeers("agent=agent:50055,agent=agent:50055,agent=agent-2:50055")
	if len(got) != 2 {
		t.Fatalf("expected 2 unique targets, got %d: %v", len(got), got)
	}
}

func TestIsWorkerType(t *testing.T) {
	cases := []struct {
		in   NodeType
		want bool
	}{
		{NodeTypeAgent, true},
		{NodeTypeVoice, true},
		{NodeTypeCognition, true},
		{NodeTypePlanner, true},
		{NodeTypeBFF, false},
		{NodeType("nonsense"), false},
		{NodeType(""), false},
	}
	for _, tc := range cases {
		if got := isWorkerType(tc.in); got != tc.want {
			t.Errorf("isWorkerType(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestTargetKey_DisambiguatesByType(t *testing.T) {
	a := WorkerTarget{NodeType: NodeTypeAgent, Address: "same:50055"}
	b := WorkerTarget{NodeType: NodeTypeVoice, Address: "same:50055"}
	if targetKey(a) == targetKey(b) {
		t.Errorf("targetKey should disambiguate by type: %s == %s", targetKey(a), targetKey(b))
	}
}

func TestNewWorkerDialer_NilWhenNothingToDrive(t *testing.T) {
	// No engine, no eventBus, no seeds -> nil.
	wd := NewWorkerDialer(testIdentity(), NewPeerManager(testIdentity(), testLogger()), nil, nil, nil, testLogger())
	if wd != nil {
		t.Errorf("expected nil dialer with nothing to drive reconciliation")
	}
}

func TestNewWorkerDialer_NilWhenIdentityOrPeerMgrMissing(t *testing.T) {
	pm := NewPeerManager(testIdentity(), testLogger())
	seeds := []WorkerTarget{{NodeType: NodeTypeAgent, Address: "agent:50055"}}

	if wd := NewWorkerDialer(nil, pm, nil, nil, seeds, testLogger()); wd != nil {
		t.Errorf("expected nil dialer when identity is nil")
	}
	if wd := NewWorkerDialer(testIdentity(), nil, nil, nil, seeds, testLogger()); wd != nil {
		t.Errorf("expected nil dialer when peerMgr is nil")
	}
	if wd := NewWorkerDialer(testIdentity(), pm, nil, nil, seeds, nil); wd != nil {
		t.Errorf("expected nil dialer when logger is nil")
	}
}

func TestNewWorkerDialer_ConstructsWithSeedsOnly(t *testing.T) {
	pm := NewPeerManager(testIdentity(), testLogger())
	seeds := []WorkerTarget{{NodeType: NodeTypeAgent, Address: "agent:50055"}}
	wd := NewWorkerDialer(testIdentity(), pm, nil, nil, seeds, testLogger())
	if wd == nil {
		t.Fatalf("expected dialer to be constructed with seeds only")
	}
	if got := wd.ActiveAddresses(); len(got) != 0 {
		t.Errorf("expected no active addresses before Start, got %v", got)
	}
}

func TestPeerManager_AttachDetachConnection(t *testing.T) {
	pm := NewPeerManager(testIdentity(), testLogger())

	// AttachConnection on a missing entry must be a no-op, not panic.
	pm.AttachConnection("ghost", &peerConnection{})
	if pm.Get("ghost") != nil {
		t.Errorf("AttachConnection on missing node must not create an entry")
	}

	pm.RegisterMonitored(&nodev1.PeerInfo{
		NodeId:   "peer-1",
		NodeType: string(NodeTypeAgent),
		Address:  "peer1:50055",
		Health:   nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY,
	})
	entry := pm.Get("peer-1")
	if entry == nil {
		t.Fatalf("expected entry after RegisterMonitored")
	}
	if entry.Connection != nil {
		t.Errorf("Connection should start nil")
	}

	conn := &peerConnection{}
	pm.AttachConnection("peer-1", conn)
	if pm.Get("peer-1").Connection != conn {
		t.Errorf("AttachConnection failed to bind connection")
	}

	pm.DetachConnection("peer-1")
	if pm.Get("peer-1").Connection != nil {
		t.Errorf("DetachConnection failed to clear connection")
	}

	// Detaching a missing node must not panic.
	pm.DetachConnection("ghost")
}
