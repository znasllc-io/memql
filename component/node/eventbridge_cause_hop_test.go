package node

import (
	"context"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/bus"
	"github.com/znasllc-io/memql/component/events"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
)

// eventbridge_cause_hop_test.go (memql#5382) proves an event's causal
// lineage (events.Cause) survives every hop the mesh puts an event through:
// the direct forward (onLocalEvent -> HandleInbound), the relay
// (ForwardInboundToPeers), and the channel-bus path (publishViaBus ->
// handleChannelMessage). Modeled on the existing bridge tests in this
// package (grep TestEventBridge) and on readiness_mesh_hop_test.go's use of
// a real *peerConnection's sendCh to capture what a bridge actually sends,
// with no live gRPC stream.
//
// This is the test that stands between the depth cap (a later task's
// refusal, reading the cause off the triggering event) and a two-node
// ping-pong running forever: without this plumbing, the cause resets to the
// zero (root) cause at every node boundary and depth never advances past 1.

// testCause returns a non-zero, non-trivial Cause: a depth-3 chain of two
// prior runs, so a bug that drops the chain, swaps CausationId and
// CorrelationId, or truncates the chain all show up distinctly.
func testCause() events.Cause {
	return events.Cause{
		CausationId:   "run-1",
		CorrelationId: "evt-x",
		Depth:         3,
		Chain: []events.Link{
			{Automation: "a", RunId: "run-0"},
			{Automation: "b", RunId: "run-1"},
		},
	}
}

func assertCauseEqual(t *testing.T, got, want events.Cause) {
	t.Helper()
	if got.Depth != want.Depth || got.CorrelationId != want.CorrelationId || got.CausationId != want.CausationId {
		t.Fatalf("cause = %+v, want %+v", got, want)
	}
	if len(got.Chain) != len(want.Chain) {
		t.Fatalf("chain = %+v, want %+v", got.Chain, want.Chain)
	}
	for i := range want.Chain {
		if got.Chain[i] != want.Chain[i] {
			t.Fatalf("chain[%d] = %+v, want %+v (oldest first)", i, got.Chain[i], want.Chain[i])
		}
	}
}

// TestEventBridgeCauseSurvivesTheDirectForwardAndRelay covers the
// EventForward leg end to end: node A forwards a local event carrying a
// cause; node B's ReceiveForward rebuilds it on B's bus AND relays it onward
// one hop further, cause intact.
func TestEventBridgeCauseSurvivesTheDirectForwardAndRelay(t *testing.T) {
	busA := events.NewBus(events.WithLogger(testLogger()))
	defer busA.Close()
	busB := events.NewBus(events.WithLogger(testLogger()))
	defer busB.Close()

	identA := testIdentity()
	identA.ID = "node-a"
	identB := testIdentity()
	identB.ID = "node-b"
	identB.Type = NodeTypeAgent

	pmA := NewPeerManager(identA, testLogger())
	bridgeA := NewEventBridge(identA, busA, pmA, testLogger())
	pmB := NewPeerManager(identB, testLogger())
	bridgeB := NewEventBridge(identB, busB, pmB, testLogger())

	// A dials B: a registered peer plus an attached *peerConnection, with no
	// live gRPC stream behind it -- Send() only ever writes to sendCh.
	pmA.Register(&nodev1.PeerInfo{
		NodeId:   "node-b",
		NodeType: string(NodeTypeAgent),
		Address:  "node-b:50052",
		Health:   nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY,
	})
	connAB := newPeerConnection(identA, "node-b", "node-b:50052", testLogger())
	pmA.AttachConnection("node-b", connAB)

	cause := testCause()
	event := events.NewEvent("graph.node.created.v1:cluster:node", events.KindNodeCreated,
		map[string]any{"id": "x"}).WithCause(cause)

	bridgeA.onLocalEvent(event)

	var forward *nodev1.EventForward
	select {
	case msg := <-connAB.sendCh:
		forward = msg.GetEventForward()
	case <-time.After(2 * time.Second):
		t.Fatal("node A did not forward the event")
	}
	if forward == nil {
		t.Fatal("captured message carried no EventForward")
	}

	// B dials C, so the relay has somewhere to go.
	pmB.Register(&nodev1.PeerInfo{
		NodeId:   "node-c",
		NodeType: string(NodeTypeAgent),
		Address:  "node-c:50052",
		Health:   nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY,
	})
	connBC := newPeerConnection(identB, "node-c", "node-c:50052", testLogger())
	pmB.AttachConnection("node-c", connBC)

	// Feed the captured EventForward into node B's one arrival path, exactly
	// as NodeServer.Stream does.
	var received events.Event
	delivered := make(chan struct{})
	unsubscribe := busB.Subscribe("graph.node.created.v1:cluster:node", func(e events.Event) {
		received = e
		close(delivered)
	})
	defer unsubscribe()

	bridgeB.ReceiveForward(forward, "node-a")

	select {
	case <-delivered:
	case <-time.After(2 * time.Second):
		t.Fatal("node B never delivered the inbound event")
	}
	assertCauseEqual(t, received.Cause, cause)

	var relayed *nodev1.EventForward
	select {
	case msg := <-connBC.sendCh:
		relayed = msg.GetEventForward()
	case <-time.After(2 * time.Second):
		t.Fatal("node B did not relay the event onward")
	}
	if relayed == nil {
		t.Fatal("captured relay message carried no EventForward")
	}
	if relayed.Hops != forward.Hops+1 {
		t.Fatalf("relayed hops = %d, want %d", relayed.Hops, forward.Hops+1)
	}
	if relayed.EventId != forward.EventId || relayed.OriginNodeId != forward.OriginNodeId {
		t.Fatalf("a relay must keep the event's id and origin: got (%s, %s), want (%s, %s)",
			relayed.EventId, relayed.OriginNodeId, forward.EventId, forward.OriginNodeId)
	}
	assertCauseEqual(t, causeFromProto(relayed.Cause), cause)
}

// TestEventBridgeCauseSurvivesTheChannelPath covers the other inbound path:
// publishViaBus encodes the cause onto an EventPublish sent over
// wiring.EventPublishCh, and events.Bus.RunWithChannel's handleChannelMessage
// decodes it back before the local subscriber sees it.
func TestEventBridgeCauseSurvivesTheChannelPath(t *testing.T) {
	producerBus := events.NewBus(events.WithLogger(testLogger()))
	defer producerBus.Close()

	identity := testIdentity()
	eb := NewEventBridge(identity, producerBus, NewPeerManager(identity, testLogger()), testLogger())

	wiring := bus.NewWiring(bus.DefaultChannelConfig())
	eb.SetWiring(wiring)

	consumerBus := events.NewBus(events.WithLogger(testLogger()))
	defer consumerBus.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go consumerBus.RunWithChannel(ctx, wiring.EventPublishCh)

	var received events.Event
	delivered := make(chan struct{})
	unsubscribe := consumerBus.Subscribe("graph.node.created.v1:cluster:node", func(e events.Event) {
		received = e
		close(delivered)
	})
	defer unsubscribe()

	cause := testCause()
	event := events.NewEvent("graph.node.created.v1:cluster:node", events.KindNodeCreated,
		map[string]any{"id": "x"}).WithCause(cause)

	eb.publishViaBus(event)

	select {
	case <-delivered:
	case <-time.After(2 * time.Second):
		t.Fatal("the channel path never delivered the event")
	}
	assertCauseEqual(t, received.Cause, cause)
}
