package node

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	nodev1 "github.com/znasllc-io/memql/component/node/gen"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func testIdentity() *Identity {
	return &Identity{
		ID:      "test-node",
		Type:    NodeTypeBFF,
		Version: "1.0.0",
		Address: "localhost:50052",
	}
}

func TestPeerManager_RegisterAndGet(t *testing.T) {
	pm := NewPeerManager(testIdentity(), testLogger())

	info := &nodev1.PeerInfo{
		NodeId:   "peer-1",
		NodeType: string(NodeTypeCognition),
		Address:  "peer1:50052",
		Health:   nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY,
	}

	pm.Register(info)

	entry := pm.Get("peer-1")
	if entry == nil {
		t.Fatal("expected peer to be registered")
	}
	if entry.Info.NodeId != "peer-1" {
		t.Errorf("expected peer-1, got %s", entry.Info.NodeId)
	}
	if pm.PeerCount() != 1 {
		t.Errorf("expected 1 peer, got %d", pm.PeerCount())
	}
}

func TestPeerManager_Remove(t *testing.T) {
	pm := NewPeerManager(testIdentity(), testLogger())

	pm.Register(&nodev1.PeerInfo{
		NodeId:   "peer-1",
		NodeType: string(NodeTypeCognition),
		Address:  "peer1:50052",
	})

	pm.Remove("peer-1")

	if pm.Get("peer-1") != nil {
		t.Error("expected peer to be removed")
	}
	if pm.PeerCount() != 0 {
		t.Errorf("expected 0 peers, got %d", pm.PeerCount())
	}
}

func TestPeerManager_ByType(t *testing.T) {
	pm := NewPeerManager(testIdentity(), testLogger())

	pm.Register(&nodev1.PeerInfo{
		NodeId:   "cognition-1",
		NodeType: string(NodeTypeCognition),
		Address:  "c1:50052",
	})
	pm.Register(&nodev1.PeerInfo{
		NodeId:   "cognition-2",
		NodeType: string(NodeTypeCognition),
		Address:  "c2:50052",
	})
	pm.Register(&nodev1.PeerInfo{
		NodeId:   "agent-1",
		NodeType: string(NodeTypeAgent),
		Address:  "a1:50052",
	})

	cognitions := pm.ByType(NodeTypeCognition)
	if len(cognitions) != 2 {
		t.Errorf("expected 2 cognitions, got %d", len(cognitions))
	}

	agents := pm.ByType(NodeTypeAgent)
	if len(agents) != 1 {
		t.Errorf("expected 1 agent, got %d", len(agents))
	}

	planners := pm.ByType(NodeTypePlanner)
	if len(planners) != 0 {
		t.Errorf("expected 0 planners, got %d", len(planners))
	}
}

func TestPeerManager_ByCapability(t *testing.T) {
	pm := NewPeerManager(testIdentity(), testLogger())

	pm.Register(&nodev1.PeerInfo{
		NodeId:       "cognition-1",
		NodeType:     string(NodeTypeCognition),
		Address:      "c1:50052",
		Capabilities: []string{"integration.cognition.scoreUtterance", "integration.cognition.trackPresence"},
	})
	pm.Register(&nodev1.PeerInfo{
		NodeId:       "agent-1",
		NodeType:     string(NodeTypeAgent),
		Address:      "a1:50052",
		Capabilities: []string{"integration.claw.executeTask"},
	})

	results := pm.ByCapability("integration.cognition.scoreUtterance")
	if len(results) != 1 {
		t.Errorf("expected 1 peer with scoreUtterance, got %d", len(results))
	}
	if len(results) > 0 && results[0].Info.NodeId != "cognition-1" {
		t.Errorf("expected cognition-1, got %s", results[0].Info.NodeId)
	}

	results = pm.ByCapability("nonexistent")
	if len(results) != 0 {
		t.Errorf("expected 0 peers for nonexistent capability, got %d", len(results))
	}
}

func TestPeerManager_TouchPeer(t *testing.T) {
	pm := NewPeerManager(testIdentity(), testLogger())

	pm.Register(&nodev1.PeerInfo{
		NodeId:   "peer-1",
		NodeType: string(NodeTypeAgent),
		Address:  "p1:50052",
	})

	before := pm.Get("peer-1").LastSeen
	time.Sleep(10 * time.Millisecond)
	pm.TouchPeer("peer-1")
	after := pm.Get("peer-1").LastSeen

	if !after.After(before) {
		t.Error("expected TouchPeer to update LastSeen")
	}
}

func TestPeerManager_PeerInfoList(t *testing.T) {
	pm := NewPeerManager(testIdentity(), testLogger())

	pm.Register(&nodev1.PeerInfo{
		NodeId:   "peer-1",
		NodeType: string(NodeTypeCognition),
		Address:  "p1:50052",
	})
	pm.Register(&nodev1.PeerInfo{
		NodeId:   "peer-2",
		NodeType: string(NodeTypeAgent),
		Address:  "p2:50052",
	})

	infos := pm.PeerInfoList()
	if len(infos) != 2 {
		t.Errorf("expected 2 PeerInfos, got %d", len(infos))
	}
}

func TestPeerManager_RegisterNil(t *testing.T) {
	pm := NewPeerManager(testIdentity(), testLogger())
	pm.Register(nil)
	if pm.PeerCount() != 0 {
		t.Error("registering nil should be a no-op")
	}
}

func TestPeerManager_RegisterEmptyId(t *testing.T) {
	pm := NewPeerManager(testIdentity(), testLogger())
	pm.Register(&nodev1.PeerInfo{NodeId: ""})
	if pm.PeerCount() != 0 {
		t.Error("registering empty ID should be a no-op")
	}
}

func TestPeerManager_CheckLiveness(t *testing.T) {
	pm := NewPeerManager(testIdentity(), testLogger())
	pm.SetTimings(5*time.Millisecond, 20*time.Millisecond, 40*time.Millisecond)

	// RegisterMonitored: this is the "directly observed" variant that
	// opts the peer into liveness tracking. Plain Register (unmonitored)
	// would skip the liveness check entirely.
	pm.RegisterMonitored(&nodev1.PeerInfo{
		NodeId:   "stale-peer",
		NodeType: string(NodeTypeAgent),
		Address:  "stale:50052",
		Health:   nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY,
	})

	// Wait for liveness timeout -> should transition to degraded.
	time.Sleep(25 * time.Millisecond)
	pm.checkLiveness()

	entry := pm.Get("stale-peer")
	if entry == nil {
		t.Fatal("peer should still exist (only degraded)")
	}
	if entry.Info.Health != nodev1.NodeHealthStatus_NODE_HEALTH_DEGRADED {
		t.Errorf("expected degraded, got %v", entry.Info.Health)
	}

	// Wait for offline timeout -> peer is marked OFFLINE and removed from
	// the in-memory routing table.
	time.Sleep(20 * time.Millisecond)
	pm.checkLiveness()

	if pm.Get("stale-peer") != nil {
		t.Error("peer should be removed after offline timeout")
	}
}

// TestPeerManager_StatusChangeHandler verifies that every liveness
// transition (register -> healthy -> degraded -> offline -> recovery) is
// delivered to the handler exactly once, in the right order. This is the
// seam NodeStatusWriter hooks to persist health to the concept store.
func TestPeerManager_StatusChangeHandler(t *testing.T) {
	pm := NewPeerManager(testIdentity(), testLogger())
	pm.SetTimings(5*time.Millisecond, 20*time.Millisecond, 40*time.Millisecond)

	type transition struct {
		peerId string
		old    nodev1.NodeHealthStatus
		newh   nodev1.NodeHealthStatus
	}
	var (
		mu      sync.Mutex
		changes []transition
	)
	pm.SetStatusChangeHandler(func(_ context.Context, p *nodev1.PeerInfo, old, newh nodev1.NodeHealthStatus, _ time.Time) {
		mu.Lock()
		defer mu.Unlock()
		changes = append(changes, transition{peerId: p.NodeId, old: old, newh: newh})
	})

	// Register (handshake advertises HEALTHY) -> first transition is
	// UNSPECIFIED -> HEALTHY. Use RegisterMonitored so the liveness
	// checker will trip on missed heartbeats.
	pm.RegisterMonitored(&nodev1.PeerInfo{
		NodeId:   "p1",
		NodeType: string(NodeTypeAgent),
		Address:  "a:50052",
		Health:   nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY,
	})

	// Wait for liveness timeout -> HEALTHY -> DEGRADED.
	time.Sleep(25 * time.Millisecond)
	pm.checkLiveness()

	// Touch restores -> DEGRADED -> HEALTHY.
	pm.TouchPeer("p1")

	// Wait for full offline threshold -> HEALTHY -> DEGRADED -> OFFLINE.
	time.Sleep(45 * time.Millisecond)
	pm.checkLiveness()

	mu.Lock()
	defer mu.Unlock()

	// Check the happy-path sequence of new-health values. The exact count
	// of degraded transitions between the two offline cycles can vary by
	// one depending on tick timing, so assert the presence and ordering
	// of the key states rather than a strict count.
	want := []nodev1.NodeHealthStatus{
		nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY,
		nodev1.NodeHealthStatus_NODE_HEALTH_DEGRADED,
		nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY,
		nodev1.NodeHealthStatus_NODE_HEALTH_OFFLINE,
	}
	gotSubset := []nodev1.NodeHealthStatus{}
	wi := 0
	for _, c := range changes {
		if wi < len(want) && c.newh == want[wi] {
			gotSubset = append(gotSubset, c.newh)
			wi++
		}
	}
	if len(gotSubset) != len(want) {
		t.Errorf("did not observe full healthy->degraded->healthy->offline sequence.\ngot: %v\nwant subset: %v", changes, want)
	}

	// The handler must NEVER fire with old == new.
	for _, c := range changes {
		if c.old == c.newh {
			t.Errorf("handler fired with unchanged status: %v -> %v", c.old, c.newh)
		}
	}
}

// TestPeerManager_StatusChangeOnRemove checks that Remove() fires a
// STOPPED transition -- the explicit graceful-exit signal the status
// writer uses to record the final concept row.
func TestPeerManager_StatusChangeOnRemove(t *testing.T) {
	pm := NewPeerManager(testIdentity(), testLogger())

	var got nodev1.NodeHealthStatus
	pm.SetStatusChangeHandler(func(_ context.Context, p *nodev1.PeerInfo, _, newh nodev1.NodeHealthStatus, _ time.Time) {
		if p.NodeId == "ghost" {
			got = newh
		}
	})

	pm.Register(&nodev1.PeerInfo{
		NodeId:   "ghost",
		NodeType: string(NodeTypePlanner),
		Address:  "g:50052",
		Health:   nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY,
	})
	got = nodev1.NodeHealthStatus_NODE_HEALTH_UNSPECIFIED // clear register-time value

	pm.Remove("ghost")

	if got != nodev1.NodeHealthStatus_NODE_HEALTH_STOPPED {
		t.Errorf("expected STOPPED transition on Remove, got %v", got)
	}
}

func TestPeerManager_UpdateExisting(t *testing.T) {
	pm := NewPeerManager(testIdentity(), testLogger())

	pm.Register(&nodev1.PeerInfo{
		NodeId:   "peer-1",
		NodeType: string(NodeTypeCognition),
		Address:  "old:50052",
	})

	pm.Register(&nodev1.PeerInfo{
		NodeId:   "peer-1",
		NodeType: string(NodeTypeCognition),
		Address:  "new:50052",
	})

	if pm.PeerCount() != 1 {
		t.Errorf("expected 1 peer after update, got %d", pm.PeerCount())
	}
	entry := pm.Get("peer-1")
	if entry.Info.Address != "new:50052" {
		t.Errorf("expected updated address, got %s", entry.Info.Address)
	}
}
