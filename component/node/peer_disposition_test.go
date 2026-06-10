package node

import (
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
)

// fakeClock is a manually-advanced clock for deterministic connecting-grace
// tests (no sleeps).
type fakeClock struct {
	t time.Time
}

func (c *fakeClock) now() time.Time { return c.t }
func (c *fakeClock) advance(d time.Duration) {
	c.t = c.t.Add(d)
}

// newGraceTestPM builds a PeerManager with an injected clock and a short
// connecting grace so the dead-for-delivery classification is exercised
// without real time passing.
func newGraceTestPM(clk *fakeClock, grace time.Duration) *PeerManager {
	pm := NewPeerManager(testIdentity(), testLogger())
	pm.setNow(clk.now)
	pm.SetConnectingGrace(grace)
	return pm
}

// TestDisposition_ConnectedPeerSends: a peer with a live connection is a
// direct send target.
func TestDisposition_ConnectedPeerSends(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_000_000, 0)}
	pm := newGraceTestPM(clk, 10*time.Second)

	const nodeId = "bff-live"
	pm.RegisterMonitored(&nodev1.PeerInfo{NodeId: nodeId, NodeType: "bff", Address: "bff:50052"})
	pc := newPeerConnection(testIdentity(), nodeId, "bff:50052", testLogger())
	pm.AttachConnection(nodeId, pc)

	if got := pm.DispositionFor(pm.Get(nodeId)); got != dispositionSend {
		t.Fatalf("connected peer: want dispositionSend, got %v", got)
	}
}

// TestDisposition_ConnectingPeerBuffers: a freshly-registered Connection==nil
// peer (within the grace window) is a buffering target -- preserves #1232.
func TestDisposition_ConnectingPeerBuffers(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_000_000, 0)}
	pm := newGraceTestPM(clk, 10*time.Second)

	const nodeId = "bff-connecting"
	pm.Register(&nodev1.PeerInfo{NodeId: nodeId, NodeType: "bff", Address: "bff:50052"})

	// Still inside the grace window.
	clk.advance(3 * time.Second)
	if got := pm.DispositionFor(pm.Get(nodeId)); got != dispositionBuffer {
		t.Fatalf("connecting peer within grace: want dispositionBuffer, got %v", got)
	}
}

// TestDisposition_PersistentlyUnconnectedPeerSkips: a Connection==nil peer
// past the grace window is dead-for-delivery -- skipped without buffering.
// This is the memql#1245 core: the old blue-green pod that keeps
// heartbeating but is never dialed.
func TestDisposition_PersistentlyUnconnectedPeerSkips(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_000_000, 0)}
	pm := newGraceTestPM(clk, 10*time.Second)

	const nodeId = "bff-dead"
	pm.Register(&nodev1.PeerInfo{NodeId: nodeId, NodeType: "bff", Address: "bff:50052"})

	// Past the grace window with no connection ever attached.
	clk.advance(11 * time.Second)
	if got := pm.DispositionFor(pm.Get(nodeId)); got != dispositionSkip {
		t.Fatalf("persistently-unconnected peer: want dispositionSkip, got %v", got)
	}
}

// TestDisposition_ReconnectedPeerResumes: a peer that lapses into
// dead-for-delivery and then (re)attaches a connection becomes a live send
// target again -- the gate is reversible, never a permanent blacklist.
func TestDisposition_ReconnectedPeerResumes(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_000_000, 0)}
	pm := newGraceTestPM(clk, 10*time.Second)

	const nodeId = "bff-recover"
	pm.Register(&nodev1.PeerInfo{NodeId: nodeId, NodeType: "bff", Address: "bff:50052"})

	clk.advance(11 * time.Second)
	if got := pm.DispositionFor(pm.Get(nodeId)); got != dispositionSkip {
		t.Fatalf("pre-attach: want dispositionSkip, got %v", got)
	}

	pc := newPeerConnection(testIdentity(), nodeId, "bff:50052", testLogger())
	pm.AttachConnection(nodeId, pc)
	if got := pm.DispositionFor(pm.Get(nodeId)); got != dispositionSend {
		t.Fatalf("after reattach: want dispositionSend, got %v", got)
	}
}

// TestDisposition_TransientBlipStillBuffers: a live peer whose connection
// drops (DetachConnection) opens a FRESH grace window, so a transient blip
// still buffers + flushes -- #1232 must not regress.
func TestDisposition_TransientBlipStillBuffers(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_000_000, 0)}
	pm := newGraceTestPM(clk, 10*time.Second)

	const nodeId = "bff-blip"
	pm.Register(&nodev1.PeerInfo{NodeId: nodeId, NodeType: "bff", Address: "bff:50052"})
	pc := newPeerConnection(testIdentity(), nodeId, "bff:50052", testLogger())
	pm.AttachConnection(nodeId, pc)

	// Live for a long time, well past the original grace window.
	clk.advance(1 * time.Hour)
	if got := pm.DispositionFor(pm.Get(nodeId)); got != dispositionSend {
		t.Fatalf("long-lived connected peer: want dispositionSend, got %v", got)
	}

	// Connection drops -- a transient blip. A fresh grace window opens from
	// the detach moment, so the peer buffers (not skips) immediately after.
	pm.DetachConnection(nodeId)
	clk.advance(2 * time.Second)
	if got := pm.DispositionFor(pm.Get(nodeId)); got != dispositionBuffer {
		t.Fatalf("transient blip within fresh grace: want dispositionBuffer, got %v", got)
	}
}

// TestForwardToPeers_SkipsDeadButBuffersConnecting is the end-to-end check on
// the EventBridge forward path (memql#1245): a broadcast with a mix of one
// live bff, one connecting bff, and one dead-for-delivery bff sends to the
// live peer, buffers for the connecting peer, and skips the dead peer WITHOUT
// growing its outbox (no buffer/expiry churn).
func TestForwardToPeers_SkipsDeadButBuffersConnecting(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_000_000, 0)}
	pm := newGraceTestPM(clk, 10*time.Second)

	bus := events.NewBus(events.WithLogger(testLogger()))
	defer bus.Close()
	eb := NewEventBridge(testIdentity(), bus, pm, testLogger())

	// Live peer: registered + connection attached.
	const liveId = "bff-live"
	pm.RegisterMonitored(&nodev1.PeerInfo{NodeId: liveId, NodeType: "bff", Address: "bff-live:50052"})
	livePC := newPeerConnection(testIdentity(), liveId, "bff-live:50052", testLogger())
	pm.AttachConnection(liveId, livePC)

	// Dead-for-delivery peer: registered long ago, never connected.
	const deadId = "bff-dead"
	pm.Register(&nodev1.PeerInfo{NodeId: deadId, NodeType: "bff", Address: "bff-dead:50052"})

	// Advance past the grace window. Both the dead peer (registered now) and
	// any future connecting peer's relative timing key off this clock.
	clk.advance(11 * time.Second)

	// Connecting peer: just registered, inside a fresh grace window.
	const connId = "bff-connecting"
	pm.Register(&nodev1.PeerInfo{NodeId: connId, NodeType: "bff", Address: "bff-conn:50052"})

	decision := routingDecision{Forward: true, Broadcast: true}
	forward := &nodev1.EventForward{EventId: "utt-1", Topic: "graph.node.created.v1:cognition:utterance", Ttl: 3}
	eb.forwardToPeers(forward, decision)

	// Live peer received exactly one message.
	if got := drainSendCh(livePC); len(got) != 1 {
		t.Fatalf("live peer: want 1 sent message, got %d", len(got))
	}
	// Connecting peer buffered exactly one event.
	if d := pm.outbox.depth(connId); d != 1 {
		t.Fatalf("connecting peer: want outbox depth 1, got %d", d)
	}
	// Dead-for-delivery peer buffered NOTHING -- no churn.
	if d := pm.outbox.depth(deadId); d != 0 {
		t.Fatalf("dead peer: want outbox depth 0 (skipped, no buffering), got %d", d)
	}
}

// TestForwardToPeers_NoReBufferChurnToDeadPeer proves the steady-state win:
// over many forwarded events to a dead-for-delivery peer, the outbox never
// grows -- i.e. the node stops spending the buffer + TTL on a pod it will
// never reach (the exact "buffered:N then expired:N" loop from #1245).
func TestForwardToPeers_NoReBufferChurnToDeadPeer(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_000_000, 0)}
	pm := newGraceTestPM(clk, 10*time.Second)

	bus := events.NewBus(events.WithLogger(testLogger()))
	defer bus.Close()
	eb := NewEventBridge(testIdentity(), bus, pm, testLogger())

	const deadId = "bff-old-replicaset"
	pm.Register(&nodev1.PeerInfo{NodeId: deadId, NodeType: "bff", Address: "old:50052"})
	clk.advance(11 * time.Second) // lapse the grace window

	decision := routingDecision{Forward: true, Broadcast: true}
	for i := 0; i < 50; i++ {
		eb.forwardToPeers(&nodev1.EventForward{
			EventId: "utt", Topic: "graph.node.created.v1:cognition:utterance", Ttl: 3,
		}, decision)
		clk.advance(1 * time.Second)
	}

	if d := pm.outbox.depth(deadId); d != 0 {
		t.Fatalf("dead peer accumulated %d buffered events; expected 0 (no re-buffer churn)", d)
	}
}

// TestForwardInboundToPeers_SkipsDeadPeer mirrors the dead-peer skip on the
// mesh-relay path (ForwardInboundToPeers), which surfaced the original
// "mesh relay buffered for peers pending connection" log in #1245.
func TestForwardInboundToPeers_SkipsDeadPeer(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_000_000, 0)}
	pm := newGraceTestPM(clk, 10*time.Second)

	bus := events.NewBus(events.WithLogger(testLogger()))
	defer bus.Close()
	eb := NewEventBridge(testIdentity(), bus, pm, testLogger())

	const deadId = "bff-dead-relay"
	pm.Register(&nodev1.PeerInfo{NodeId: deadId, NodeType: "bff", Address: "dead:50052"})
	clk.advance(11 * time.Second)

	eb.ForwardInboundToPeers(&nodev1.EventForward{
		EventId: "relay-1", Topic: "graph.node.created.v1:cognition:utterance", Ttl: 3,
	}, "some-origin")

	if d := pm.outbox.depth(deadId); d != 0 {
		t.Fatalf("mesh relay buffered to dead peer (depth %d); expected skip with 0", d)
	}
}

// TestOfflineRemovedPeerOutboxDiscarded: when a monitored peer trips OFFLINE
// in checkLiveness and is removed from the routing table, its outbox is
// discarded so no buffered events linger for a peer that's gone.
func TestOfflineRemovedPeerOutboxDiscarded(t *testing.T) {
	pm := NewPeerManager(testIdentity(), testLogger())
	// Tight timings so a single checkLiveness pass trips OFFLINE.
	pm.SetTimings(time.Millisecond, time.Millisecond, time.Millisecond)

	const nodeId = "bff-offline"
	pm.RegisterMonitored(&nodev1.PeerInfo{NodeId: nodeId, NodeType: "bff", Address: "bff:50052"})
	// Buffer some events for it (simulating the connecting window).
	pm.EnqueuePending(nodeId, newTestForward("e1"))
	pm.EnqueuePending(nodeId, newTestForward("e2"))
	if d := pm.outbox.depth(nodeId); d != 2 {
		t.Fatalf("precondition: want outbox depth 2, got %d", d)
	}

	// Age it past offlineTimeout and run the liveness check.
	time.Sleep(5 * time.Millisecond)
	pm.checkLiveness()

	if pm.Get(nodeId) != nil {
		t.Fatalf("expected peer removed from table after OFFLINE")
	}
	if d := pm.outbox.depth(nodeId); d != 0 {
		t.Fatalf("expected offline peer outbox discarded, got depth %d", d)
	}
}
