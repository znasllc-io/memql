package node

import (
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
)

// These tests cover the EventBridge forward disposition after memql#1271
// reverted the dead-for-delivery "skip" (memql#1245). The forward path now
// has exactly two dispositions: a peer with a live outbound connection is
// SENT to; a Connection==nil peer is always BUFFERED (the #1232 outbox), and
// is NEVER silently skipped -- the durable substrate (memql#1264), not the
// best-effort mesh fast-path, is the cross-replica delivery guarantee.

// TestDisposition_ConnectedPeerSends: a peer with a live connection is a
// direct send target.
func TestDisposition_ConnectedPeerSends(t *testing.T) {
	pm := NewPeerManager(testIdentity(), testLogger())

	const nodeId = "bff-live"
	pm.RegisterMonitored(&nodev1.PeerInfo{NodeId: nodeId, NodeType: "bff", Address: "bff:50052"})
	pc := newPeerConnection(testIdentity(), nodeId, "bff:50052", testLogger())
	pm.AttachConnection(nodeId, pc)

	if got := pm.DispositionFor(pm.Get(nodeId)); got != dispositionSend {
		t.Fatalf("connected peer: want dispositionSend, got %v", got)
	}
}

// TestDisposition_UnconnectedPeerBuffers: a Connection==nil peer is a
// buffering target -- preserves #1232 and, post-#1271, NEVER skips. This is
// the core revert: a peer learned only via gossip (Connection==nil on this
// node) is a live non-parent replica that may own the user's WebSocket, so
// the fast-path must buffer for it, not drop the event.
func TestDisposition_UnconnectedPeerBuffers(t *testing.T) {
	pm := NewPeerManager(testIdentity(), testLogger())

	const nodeId = "bff-gossiped"
	pm.Register(&nodev1.PeerInfo{NodeId: nodeId, NodeType: "bff", Address: "bff:50052"})

	if got := pm.DispositionFor(pm.Get(nodeId)); got != dispositionBuffer {
		t.Fatalf("unconnected peer: want dispositionBuffer, got %v", got)
	}
}

// TestDisposition_LongLivedUnconnectedPeerStillBuffers proves the #1271
// revert at its sharpest: a peer that has been Connection==nil for a long
// time (the exact condition #1245 classified "dead-for-delivery" and SKIPPED)
// is now still a BUFFER target, never skipped. A node that never dials us but
// keeps appearing via gossip is precisely the live non-parent bff replica
// that owns a WebSocket on the star topology -- dropping its events was the
// #1259 cross-replica delivery bug.
func TestDisposition_LongLivedUnconnectedPeerStillBuffers(t *testing.T) {
	pm := NewPeerManager(testIdentity(), testLogger())

	const nodeId = "bff-nonparent-replica"
	pm.Register(&nodev1.PeerInfo{NodeId: nodeId, NodeType: "bff", Address: "bff:50052"})

	// No amount of "time unconnected" downgrades a Connection==nil peer to a
	// skip anymore -- disposition is purely a function of Connection presence.
	for i := 0; i < 100; i++ {
		if got := pm.DispositionFor(pm.Get(nodeId)); got != dispositionBuffer {
			t.Fatalf("long-lived unconnected peer (iter %d): want dispositionBuffer, got %v", i, got)
		}
	}
}

// TestDisposition_ReconnectedPeerResumes: a peer that attaches a connection
// becomes a live send target; detaching it drops it back to a buffer target.
func TestDisposition_ReconnectedPeerResumes(t *testing.T) {
	pm := NewPeerManager(testIdentity(), testLogger())

	const nodeId = "bff-recover"
	pm.Register(&nodev1.PeerInfo{NodeId: nodeId, NodeType: "bff", Address: "bff:50052"})

	if got := pm.DispositionFor(pm.Get(nodeId)); got != dispositionBuffer {
		t.Fatalf("pre-attach: want dispositionBuffer, got %v", got)
	}

	pc := newPeerConnection(testIdentity(), nodeId, "bff:50052", testLogger())
	pm.AttachConnection(nodeId, pc)
	if got := pm.DispositionFor(pm.Get(nodeId)); got != dispositionSend {
		t.Fatalf("after attach: want dispositionSend, got %v", got)
	}

	pm.DetachConnection(nodeId)
	if got := pm.DispositionFor(pm.Get(nodeId)); got != dispositionBuffer {
		t.Fatalf("after detach: want dispositionBuffer, got %v", got)
	}
}

// TestForwardToPeers_BuffersUnconnectedDoesNotSkip is the end-to-end check on
// the EventBridge forward path after #1271: a broadcast with a mix of one
// live bff and two Connection==nil bff replicas (one freshly registered, one
// registered long ago) sends to the live peer and BUFFERS for BOTH unconnected
// peers. The "registered long ago" peer is the regression target: pre-#1271 it
// would have been skipped (its event dropped); now it buffers like any other.
func TestForwardToPeers_BuffersUnconnectedDoesNotSkip(t *testing.T) {
	pm := NewPeerManager(testIdentity(), testLogger())

	bus := events.NewBus(events.WithLogger(testLogger()))
	defer bus.Close()
	eb := NewEventBridge(testIdentity(), bus, pm, testLogger())

	// Live peer: registered + connection attached.
	const liveId = "bff-live"
	pm.RegisterMonitored(&nodev1.PeerInfo{NodeId: liveId, NodeType: "bff", Address: "bff-live:50052"})
	livePC := newPeerConnection(testIdentity(), liveId, "bff-live:50052", testLogger())
	pm.AttachConnection(liveId, livePC)

	// Non-parent replica learned via gossip, never dialed by this node: the
	// #1245 "dead-for-delivery" case that this revert must keep delivering to.
	const gossipedId = "bff-nonparent"
	pm.Register(&nodev1.PeerInfo{NodeId: gossipedId, NodeType: "bff", Address: "bff-nonparent:50052"})

	// A freshly registered unconnected peer.
	const freshId = "bff-fresh"
	pm.Register(&nodev1.PeerInfo{NodeId: freshId, NodeType: "bff", Address: "bff-fresh:50052"})

	decision := routingDecision{Forward: true, Broadcast: true}
	forward := &nodev1.EventForward{EventId: "utt-1", Topic: "graph.node.created.v1:cognition:utterance", Ttl: 3}
	eb.forwardToPeers(forward, decision)

	// Live peer received exactly one message.
	if got := drainSendCh(livePC); len(got) != 1 {
		t.Fatalf("live peer: want 1 sent message, got %d", len(got))
	}
	// BOTH unconnected peers buffered exactly one event -- nothing skipped.
	if d := pm.outbox.depth(gossipedId); d != 1 {
		t.Fatalf("gossiped non-parent replica: want outbox depth 1 (buffered, not skipped), got %d", d)
	}
	if d := pm.outbox.depth(freshId); d != 1 {
		t.Fatalf("fresh unconnected peer: want outbox depth 1, got %d", d)
	}
}

// TestForwardInboundToPeers_BuffersUnconnected mirrors the buffer-not-skip
// behavior on the mesh-relay path (ForwardInboundToPeers) after #1271.
func TestForwardInboundToPeers_BuffersUnconnected(t *testing.T) {
	pm := NewPeerManager(testIdentity(), testLogger())

	bus := events.NewBus(events.WithLogger(testLogger()))
	defer bus.Close()
	eb := NewEventBridge(testIdentity(), bus, pm, testLogger())

	const gossipedId = "bff-relay-nonparent"
	pm.Register(&nodev1.PeerInfo{NodeId: gossipedId, NodeType: "bff", Address: "relay:50052"})

	eb.ForwardInboundToPeers(&nodev1.EventForward{
		EventId: "relay-1", Topic: "graph.node.created.v1:cognition:utterance", Ttl: 3,
	}, "some-origin")

	if d := pm.outbox.depth(gossipedId); d != 1 {
		t.Fatalf("mesh relay to unconnected peer: want outbox depth 1 (buffered, not skipped), got %d", d)
	}
}

// TestOfflineRemovedPeerOutboxDiscarded: when a monitored peer trips OFFLINE
// in checkLiveness and is removed from the routing table, its outbox is
// discarded so no buffered events linger for a peer that's gone. This is the
// correct way a buffered-for peer is reclaimed -- a genuine offline detection,
// not a fast-path skip.
func TestOfflineRemovedPeerOutboxDiscarded(t *testing.T) {
	pm := NewPeerManager(testIdentity(), testLogger())
	// Tight timings so a single checkLiveness pass trips OFFLINE.
	pm.SetTimings(time.Millisecond, time.Millisecond, time.Millisecond)

	const nodeId = "bff-offline"
	pm.RegisterMonitored(&nodev1.PeerInfo{NodeId: nodeId, NodeType: "bff", Address: "bff:50052"})
	// Buffer some events for it.
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
