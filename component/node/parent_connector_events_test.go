package node

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/events"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	"google.golang.org/grpc"
)

// A parent removed during a BFF rollout closes its outbound transport. The
// agent must create a working replacement, then deliver its composition update
// to the parent's bus, rather than merely report itself running.
func TestParentConnector_CompositionEventsRecoverAfterParentRemoval(t *testing.T) {
	logger := testLogger()
	parentIdentity := &Identity{ID: "bff-parent", Type: NodeTypeBFF}
	parentPM := NewPeerManager(parentIdentity, logger)
	parentBus := events.NewBus()
	defer parentBus.Close()
	// The bridge installs itself as the peer table's event sink; the server
	// needs no wiring of its own.
	_ = NewEventBridge(parentIdentity, parentBus, parentPM, logger)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := grpc.NewServer()
	nodev1.RegisterNodeServiceServer(server, &nodeService{
		logger: logger, identity: parentIdentity, peerManager: parentPM,
	})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	identity := &Identity{ID: "agent-child", Type: NodeTypeAgent, ParentAddress: listener.Addr().String()}
	pm := NewPeerManager(identity, logger)
	connector := NewParentConnector(identity, pm, logger)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	connector.Start(ctx)
	t.Cleanup(func() { connector.Stop(context.Background()) })
	attached := func() *peerConnection {
		conn, _ := pm.sendTarget(pm.Get(parentIdentity.ID))
		return conn
	}
	require.Eventually(t, func() bool { return attached() != nil }, 3*time.Second, 10*time.Millisecond)
	original := attached()
	pm.Remove(parentIdentity.ID)
	require.Eventually(t, func() bool { return attached() != nil && attached() != original }, 5*time.Second, 10*time.Millisecond,
		"parent supervisor must replace the transport permanently closed by peer removal")

	const topic = "graph.node.updated.v1:compose:composition"
	got := make(chan events.Event, 1)
	unsub := parentBus.Subscribe(topic, func(e events.Event) { got <- e })
	defer unsub()
	childBridge := NewEventBridge(identity, events.NewBus(), pm, logger)
	defer childBridge.localBus.Close()
	childBridge.onLocalEvent(events.NewEvent(topic, events.KindNodeUpdated,
		map[string]any{"id": "composition-1", "status": "ready", "ownerUserId": "owner-1"}))
	select {
	case event := <-got:
		require.Equal(t, "ready", event.Payload["status"])
		require.Equal(t, identity.ID, event.OriginNodeId)
	case <-time.After(2 * time.Second):
		t.Fatal("composition update did not cross the recovered agent-to-BFF stream")
	}
}

func TestParentConnector_ChangedParentDetachesOldIdentity(t *testing.T) {
	identity := &Identity{ID: "agent", Type: NodeTypeAgent, ParentAddress: "bff-active:50058"}
	pm := NewPeerManager(identity, testLogger())
	connector := NewParentConnector(identity, pm, testLogger())
	conn := newPeerConnection(identity, "", identity.ParentAddress, testLogger())
	defer conn.Close()
	connector.conn = conn
	welcome := func(nodeID string) {
		connector.handleServerMessage(&nodev1.NodeServerMessage{Payload: &nodev1.NodeServerMessage_NodeWelcome{
			NodeWelcome: &nodev1.NodeWelcome{NodeId: nodeID, NodeType: string(NodeTypeBFF)},
		}})
	}
	welcome("bff-old")
	welcome("bff-new")
	_, attached := pm.sendTarget(pm.Get("bff-old"))
	require.False(t, attached, "old parent's eventual offline reap must not close the replacement parent's stream")
	pm.Remove("bff-old")
	conn.mu.Lock()
	closed := conn.closed
	conn.mu.Unlock()
	require.False(t, closed)
	current, ok := pm.sendTarget(pm.Get("bff-new"))
	require.True(t, ok)
	require.Same(t, conn, current)
}
