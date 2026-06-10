package node

import (
	"testing"

	"github.com/znasllc-io/memql/component/events"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
)

// These tests cover the EventBridge forward path after epic memql#1259 Phase 2
// closeout (memql#1267): the ad-hoc push-model patches -- the #1232 per-peer
// outbox and the #1245 dead-peer skip -- are retired. The forward path is now a
// pure best-effort fast-path: a peer with a live outbound connection is sent to;
// a Connection==nil peer is simply skipped (no buffering, no skip-classification).
// The durable delivery substrate (memql#1264), not the mesh, is the cross-replica
// delivery guarantee, so a fast-path hint that misses a not-yet-connected peer is
// harmless (the durable pull catches that consumer up).

// drainSendCh non-blockingly collects every queued message from a
// peerConnection's send channel.
func drainSendCh(pc *peerConnection) []*nodev1.NodeClientMessage {
	var out []*nodev1.NodeClientMessage
	for {
		select {
		case m := <-pc.sendCh:
			out = append(out, m)
		default:
			return out
		}
	}
}

// eventIdOf extracts the EventForward id from a NodeClientMessage.
func eventIdOf(msg *nodev1.NodeClientMessage) string {
	if ef, ok := msg.Payload.(*nodev1.NodeClientMessage_EventForward); ok {
		return ef.EventForward.EventId
	}
	return ""
}

// TestSendTarget_ConnectedPeer: a peer with a live connection is a send target.
func TestSendTarget_ConnectedPeer(t *testing.T) {
	pm := NewPeerManager(testIdentity(), testLogger())

	const nodeId = "bff-live"
	pm.RegisterMonitored(&nodev1.PeerInfo{NodeId: nodeId, NodeType: "bff", Address: "bff:50052"})
	pc := newPeerConnection(testIdentity(), nodeId, "bff:50052", testLogger())
	pm.AttachConnection(nodeId, pc)

	if conn, ok := pm.sendTarget(pm.Get(nodeId)); !ok || conn == nil {
		t.Fatalf("connected peer: want (conn, true), got (%v, %v)", conn, ok)
	}
}

// TestSendTarget_UnconnectedPeerSkipped: a Connection==nil peer is not a send
// target -- the fast-path skips it (no buffering). The durable substrate
// backstops delivery.
func TestSendTarget_UnconnectedPeerSkipped(t *testing.T) {
	pm := NewPeerManager(testIdentity(), testLogger())

	const nodeId = "bff-gossiped"
	pm.Register(&nodev1.PeerInfo{NodeId: nodeId, NodeType: "bff", Address: "bff:50052"})

	if conn, ok := pm.sendTarget(pm.Get(nodeId)); ok || conn != nil {
		t.Fatalf("unconnected peer: want (nil, false), got (%v, %v)", conn, ok)
	}
}

// TestSendTarget_ReconnectedPeerResumes: a peer that attaches a connection
// becomes a live send target; detaching it drops it back to a non-target.
func TestSendTarget_ReconnectedPeerResumes(t *testing.T) {
	pm := NewPeerManager(testIdentity(), testLogger())

	const nodeId = "bff-recover"
	pm.Register(&nodev1.PeerInfo{NodeId: nodeId, NodeType: "bff", Address: "bff:50052"})

	if _, ok := pm.sendTarget(pm.Get(nodeId)); ok {
		t.Fatalf("pre-attach: want not a send target")
	}

	pc := newPeerConnection(testIdentity(), nodeId, "bff:50052", testLogger())
	pm.AttachConnection(nodeId, pc)
	if _, ok := pm.sendTarget(pm.Get(nodeId)); !ok {
		t.Fatalf("after attach: want send target")
	}

	pm.DetachConnection(nodeId)
	if _, ok := pm.sendTarget(pm.Get(nodeId)); ok {
		t.Fatalf("after detach: want not a send target")
	}
}

// TestSendTarget_NilEntry: a nil entry is never a send target.
func TestSendTarget_NilEntry(t *testing.T) {
	pm := NewPeerManager(testIdentity(), testLogger())
	if _, ok := pm.sendTarget(nil); ok {
		t.Fatalf("nil entry: want not a send target")
	}
}

// TestForwardToPeers_SendsConnectedSkipsUnconnected is the end-to-end check on
// the EventBridge forward path after #1267: a broadcast with one live bff and
// two Connection==nil bff replicas sends ONLY to the live peer and silently
// skips the unconnected ones (no panic, no buffering). The durable substrate is
// the delivery guarantee for the skipped replicas.
func TestForwardToPeers_SendsConnectedSkipsUnconnected(t *testing.T) {
	pm := NewPeerManager(testIdentity(), testLogger())

	bus := events.NewBus(events.WithLogger(testLogger()))
	defer bus.Close()
	eb := NewEventBridge(testIdentity(), bus, pm, testLogger())

	// Live peer: registered + connection attached.
	const liveId = "bff-live"
	pm.RegisterMonitored(&nodev1.PeerInfo{NodeId: liveId, NodeType: "bff", Address: "bff-live:50052"})
	livePC := newPeerConnection(testIdentity(), liveId, "bff-live:50052", testLogger())
	pm.AttachConnection(liveId, livePC)

	// Two non-parent replicas learned via gossip, never dialed by this node.
	const gossipedId = "bff-nonparent"
	pm.Register(&nodev1.PeerInfo{NodeId: gossipedId, NodeType: "bff", Address: "bff-nonparent:50052"})
	const freshId = "bff-fresh"
	pm.Register(&nodev1.PeerInfo{NodeId: freshId, NodeType: "bff", Address: "bff-fresh:50052"})

	decision := routingDecision{Forward: true, Broadcast: true}
	forward := &nodev1.EventForward{EventId: "utt-1", Topic: "graph.node.created.v1:cognition:utterance", Ttl: 3}
	eb.forwardToPeers(forward, decision)

	// Live peer received exactly one message; the unconnected ones got nothing.
	got := drainSendCh(livePC)
	if len(got) != 1 || eventIdOf(got[0]) != "utt-1" {
		t.Fatalf("live peer: want 1 message (utt-1), got %#v", got)
	}
}

// TestForwardInboundToPeers_SkipsUnconnected mirrors the send-if-connected
// behavior on the mesh-relay path (ForwardInboundToPeers) after #1267: a
// Connection==nil peer is harmlessly skipped (no buffering, no panic).
func TestForwardInboundToPeers_SkipsUnconnected(t *testing.T) {
	pm := NewPeerManager(testIdentity(), testLogger())

	bus := events.NewBus(events.WithLogger(testLogger()))
	defer bus.Close()
	eb := NewEventBridge(testIdentity(), bus, pm, testLogger())

	// One live peer, one gossiped (unconnected) peer.
	const liveId = "bff-relay-live"
	pm.RegisterMonitored(&nodev1.PeerInfo{NodeId: liveId, NodeType: "bff", Address: "relay-live:50052"})
	livePC := newPeerConnection(testIdentity(), liveId, "relay-live:50052", testLogger())
	pm.AttachConnection(liveId, livePC)

	const gossipedId = "bff-relay-nonparent"
	pm.Register(&nodev1.PeerInfo{NodeId: gossipedId, NodeType: "bff", Address: "relay:50052"})

	eb.ForwardInboundToPeers(&nodev1.EventForward{
		EventId: "relay-1", Topic: "graph.node.created.v1:cognition:utterance", Ttl: 3,
	}, "some-origin")

	// The live peer received the relay; the gossiped peer was skipped (no buffer).
	got := drainSendCh(livePC)
	if len(got) != 1 || eventIdOf(got[0]) != "relay-1" {
		t.Fatalf("live relay peer: want 1 message (relay-1), got %#v", got)
	}
}
