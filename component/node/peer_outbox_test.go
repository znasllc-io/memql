package node

import (
	"testing"
	"time"

	nodev1 "github.com/znasllc-io/memql/component/node/gen"
)

// newTestForward builds a NodeClientMessage carrying an EventForward with the
// given event id, mirroring what EventBridge.forwardToPeers constructs.
func newTestForward(eventId string) *nodev1.NodeClientMessage {
	return &nodev1.NodeClientMessage{
		MessageId: "msg-" + eventId,
		Payload: &nodev1.NodeClientMessage_EventForward{
			EventForward: &nodev1.EventForward{
				EventId: eventId,
				Topic:   "graph.node.created.v1:cognition:utterance",
				Ttl:     3,
			},
		},
	}
}

func eventIdOf(msg *nodev1.NodeClientMessage) string {
	if ef, ok := msg.Payload.(*nodev1.NodeClientMessage_EventForward); ok {
		return ef.EventForward.EventId
	}
	return ""
}

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

// TestPeerOutbox_AccumulatesAndDrainsInOrder proves that EnqueuePending
// buffers events for a Connection==nil peer and AttachConnection flushes them
// to the new connection in FIFO order.
func TestPeerOutbox_AccumulatesAndDrainsInOrder(t *testing.T) {
	pm := NewPeerManager(testIdentity(), testLogger())

	const nodeId = "peer-cognition-1"
	pm.RegisterMonitored(&nodev1.PeerInfo{
		NodeId:   nodeId,
		NodeType: "cognition",
		Address:  "cognition:50054",
	})

	// Peer is registered but no connection attached yet (the nil window).
	if entry := pm.Get(nodeId); entry == nil || entry.Connection != nil {
		t.Fatalf("expected registered peer with nil Connection, got %#v", entry)
	}

	ids := []string{"e1", "e2", "e3"}
	for _, id := range ids {
		pm.EnqueuePending(nodeId, newTestForward(id))
	}
	if got := pm.outbox.depth(nodeId); got != len(ids) {
		t.Fatalf("expected %d buffered events, got %d", len(ids), got)
	}

	// Attach a connection -- this must flush the outbox to it in order.
	pc := newPeerConnection(testIdentity(), nodeId, "cognition:50054", testLogger())
	pm.AttachConnection(nodeId, pc)

	got := drainSendCh(pc)
	if len(got) != len(ids) {
		t.Fatalf("expected %d flushed messages, got %d", len(ids), len(got))
	}
	for i, want := range ids {
		if eventIdOf(got[i]) != want {
			t.Errorf("flush order mismatch at %d: got %q want %q", i, eventIdOf(got[i]), want)
		}
	}
	if depth := pm.outbox.depth(nodeId); depth != 0 {
		t.Errorf("expected drained outbox, got depth %d", depth)
	}
}

// TestPeerOutbox_NoSendUnderPeerManagerLock proves AttachConnection does not
// invoke conn.Send while holding pm.mu. We sit a goroutine in Send (by
// filling the connection's send channel) and assert AttachConnection releases
// pm.mu before the flush blocks -- a concurrent pm.Get() must not deadlock.
func TestPeerOutbox_NoSendUnderPeerManagerLock(t *testing.T) {
	pm := NewPeerManager(testIdentity(), testLogger())
	const nodeId = "peer-x"
	pm.RegisterMonitored(&nodev1.PeerInfo{NodeId: nodeId, NodeType: "agent"})

	// Buffer more events than the connection's send channel can hold so the
	// flush loop would block inside conn.Send if it ran under a held lock.
	for i := 0; i < sendChCapacity+5; i++ {
		pm.EnqueuePending(nodeId, newTestForward("evt"))
	}

	pc := newPeerConnection(testIdentity(), nodeId, "agent:50055", testLogger())

	attachDone := make(chan struct{})
	go func() {
		pm.AttachConnection(nodeId, pc)
		close(attachDone)
	}()

	// Concurrently hammer the PeerManager lock. If AttachConnection held
	// pm.mu across the (blocking) Send loop, these would stall until the
	// channel drains. We assert they keep returning promptly.
	deadline := time.After(2 * time.Second)
	for i := 0; i < 50; i++ {
		done := make(chan struct{})
		go func() {
			pm.Get(nodeId) // RLock
			pm.PeerCount() // RLock
			close(done)
		}()
		select {
		case <-done:
		case <-deadline:
			t.Fatal("pm.mu appears held during outbox flush -- possible deadlock (Send under lock)")
		}
	}

	// Unblock the flush by draining the connection channel.
	go func() {
		for {
			select {
			case <-pc.sendCh:
			case <-attachDone:
				return
			}
		}
	}()

	select {
	case <-attachDone:
	case <-time.After(2 * time.Second):
		t.Fatal("AttachConnection did not complete")
	}
}

// TestPeerOutbox_OverflowDropsOldest proves the per-peer cap is enforced and
// the OLDEST entry is dropped on overflow.
func TestPeerOutbox_OverflowDropsOldest(t *testing.T) {
	o := newPeerOutbox(3, peerOutboxTTL, testLogger())
	const nodeId = "p"

	o.enqueue(nodeId, newTestForward("a"))
	o.enqueue(nodeId, newTestForward("b"))
	o.enqueue(nodeId, newTestForward("c"))
	o.enqueue(nodeId, newTestForward("d")) // overflow -- "a" drops

	got := o.drain(nodeId)
	if len(got) != 3 {
		t.Fatalf("expected cap=3 retained, got %d", len(got))
	}
	want := []string{"b", "c", "d"}
	for i, w := range want {
		if eventIdOf(got[i]) != w {
			t.Errorf("at %d: got %q want %q (oldest should have been dropped)", i, eventIdOf(got[i]), w)
		}
	}
}

// TestPeerOutbox_TTLExpiry proves stale buffered events are dropped both on
// the next enqueue and on drain.
func TestPeerOutbox_TTLExpiry(t *testing.T) {
	o := newPeerOutbox(16, 30*time.Second, testLogger())
	const nodeId = "p"

	base := time.Unix(1_000_000, 0)
	cur := base
	o.now = func() time.Time { return cur }

	o.enqueue(nodeId, newTestForward("old"))
	if o.depth(nodeId) != 1 {
		t.Fatalf("expected 1 buffered, got %d", o.depth(nodeId))
	}

	// Advance past the TTL, then enqueue a fresh event. The stale one must
	// be swept on enqueue.
	cur = base.Add(31 * time.Second)
	o.enqueue(nodeId, newTestForward("new"))
	if o.depth(nodeId) != 1 {
		t.Fatalf("expected stale event swept on enqueue, depth=%d", o.depth(nodeId))
	}

	got := o.drain(nodeId)
	if len(got) != 1 || eventIdOf(got[0]) != "new" {
		t.Fatalf("expected only fresh event after expiry, got %#v", got)
	}

	// Buffer one and let it go stale, then drain -- drain must drop it.
	o.enqueue(nodeId, newTestForward("stale2"))
	cur = cur.Add(31 * time.Second)
	if got := o.drain(nodeId); len(got) != 0 {
		t.Fatalf("expected drain to drop TTL-expired event, got %#v", got)
	}
}

// TestPeerOutbox_ConnectedPeerUnaffected proves a normally-connected peer
// sends immediately and never touches the outbox.
func TestPeerOutbox_ConnectedPeerUnaffected(t *testing.T) {
	pm := NewPeerManager(testIdentity(), testLogger())
	const nodeId = "peer-connected"
	pm.RegisterMonitored(&nodev1.PeerInfo{NodeId: nodeId, NodeType: "cognition"})

	pc := newPeerConnection(testIdentity(), nodeId, "cognition:50054", testLogger())
	pm.AttachConnection(nodeId, pc) // attach BEFORE any events

	entry := pm.Get(nodeId)
	if entry == nil || entry.Connection == nil {
		t.Fatal("expected attached connection")
	}
	// Simulate the forwardToPeers fast path: Connection != nil -> Send.
	entry.Connection.Send(newTestForward("direct"))

	if depth := pm.outbox.depth(nodeId); depth != 0 {
		t.Errorf("connected peer should not buffer, outbox depth=%d", depth)
	}
	if got := drainSendCh(pc); len(got) != 1 || eventIdOf(got[0]) != "direct" {
		t.Errorf("expected 1 directly-sent message, got %#v", got)
	}
}

// TestPeerOutbox_DetachThenReattachRoundTrips proves events buffered after a
// detach are flushed on the next attach.
func TestPeerOutbox_DetachThenReattachRoundTrips(t *testing.T) {
	pm := NewPeerManager(testIdentity(), testLogger())
	const nodeId = "peer-churn"
	pm.RegisterMonitored(&nodev1.PeerInfo{NodeId: nodeId, NodeType: "cognition"})

	pc1 := newPeerConnection(testIdentity(), nodeId, "cognition:50054", testLogger())
	pm.AttachConnection(nodeId, pc1)

	// Connection drops (churn) -> detach. Subsequent forwards buffer.
	pm.DetachConnection(nodeId)
	pm.EnqueuePending(nodeId, newTestForward("during-churn-1"))
	pm.EnqueuePending(nodeId, newTestForward("during-churn-2"))

	if got := drainSendCh(pc1); len(got) != 0 {
		t.Fatalf("detached connection should not have received the churn events, got %#v", got)
	}

	// New connection attaches -> buffered events flush to it.
	pc2 := newPeerConnection(testIdentity(), nodeId, "cognition:50054", testLogger())
	pm.AttachConnection(nodeId, pc2)

	got := drainSendCh(pc2)
	if len(got) != 2 {
		t.Fatalf("expected 2 events flushed to reattached connection, got %d", len(got))
	}
	if eventIdOf(got[0]) != "during-churn-1" || eventIdOf(got[1]) != "during-churn-2" {
		t.Errorf("flush order wrong: %q, %q", eventIdOf(got[0]), eventIdOf(got[1]))
	}
}

// TestPeerOutbox_DiscardOnRemove proves a removed peer's buffer is dropped.
func TestPeerOutbox_DiscardOnRemove(t *testing.T) {
	pm := NewPeerManager(testIdentity(), testLogger())
	const nodeId = "peer-gone"
	pm.RegisterMonitored(&nodev1.PeerInfo{NodeId: nodeId, NodeType: "agent"})
	pm.EnqueuePending(nodeId, newTestForward("e1"))

	if pm.outbox.depth(nodeId) != 1 {
		t.Fatalf("expected 1 buffered before remove")
	}
	pm.Remove(nodeId)
	if pm.outbox.depth(nodeId) != 0 {
		t.Errorf("expected outbox discarded on Remove, depth=%d", pm.outbox.depth(nodeId))
	}
}

// TestEventBridge_ForwardBuffersWhenConnectionNil proves the integration:
// forwardToPeers enqueues into the outbox for a Connection==nil peer rather
// than dropping, and AttachConnection then flushes it.
func TestEventBridge_ForwardBuffersWhenConnectionNil(t *testing.T) {
	pm := NewPeerManager(testIdentity(), testLogger())
	const nodeId = "peer-bff-1"
	// Register a bff peer with no connection -- the post-cutover nil window.
	pm.RegisterMonitored(&nodev1.PeerInfo{NodeId: nodeId, NodeType: "bff"})

	// Broadcast forward to all peers.
	forward := &nodev1.EventForward{
		EventId: "utterance-1",
		Topic:   "graph.node.created.v1:cognition:utterance",
		Ttl:     3,
	}
	// Use the EventBridge directly (no bus needed for forwardToPeers).
	eb := &EventBridge{peerManager: pm, identity: testIdentity(), logger: testLogger()}
	eb.forwardToPeers(forward, routingDecision{Forward: true, Broadcast: true})

	if depth := pm.outbox.depth(nodeId); depth != 1 {
		t.Fatalf("expected forwardToPeers to buffer 1 event for nil-connection peer, depth=%d", depth)
	}

	pc := newPeerConnection(testIdentity(), nodeId, "bff:50051", testLogger())
	pm.AttachConnection(nodeId, pc)

	got := drainSendCh(pc)
	if len(got) != 1 || eventIdOf(got[0]) != "utterance-1" {
		t.Fatalf("expected the buffered utterance flushed on attach, got %#v", got)
	}
}
