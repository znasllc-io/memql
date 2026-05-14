package node

import (
	"context"
	"net"
	"testing"
	"time"

	nodev1 "github.com/visionarys-io/memql/component/node/gen"
	"google.golang.org/grpc"
)

// TestWorkerDialer_RedialsAfterExternalClose is the regression guard for
// the cluster-mode bug where a BFF worker_dialer connection to a restarting
// worker (cognition, voice, agent, planner) went permanently dead.
//
// Symptom: after the worker container restarts, BFF logs
// "worker_dialer: connection ended with error" once and never again. The
// worker's ParentConnector reconnects to BFF (inbound stream), so BFF's
// peer table shows the worker as connected -- but BFF's outbound stream to
// the worker stays down. Events forwarded with `broadcast: true` count the
// worker in peer_count but never actually land on it.
//
// Root cause: when the worker container goes away, two streams die at once:
//  1. BFF's outbound stream (WorkerDialer -> worker).
//  2. Worker's ParentConnector inbound stream (worker -> BFF).
//
// The inbound stream's death runs stream_handler.Stream's defer, which
// calls peerManager.Remove(workerId). Remove closes entry.Connection,
// which is the WorkerDialer's outbound *peerConnection. Closing it
// cancels Connect's inner ctx, so Connect returns and the dialTarget
// goroutine exits -- but the entry stays in wd.conns, so reconcile sees
// the key as occupied and never re-dials.
//
// The fix: when Connect exits without a worker_dialer-initiated shutdown,
// drop the entry from wd.conns and trigger an immediate reconcile so the
// next pass re-dials if the target is still desired.
//
// The test simulates the bug by starting a real NodeService server as the
// "worker", dialing it from a WorkerDialer, waiting for the handshake,
// then directly calling peerManager.Remove (which is what the inbound
// stream's defer would do in production) and asserting the WorkerDialer
// re-establishes the stream within a short window.
func TestWorkerDialer_RedialsAfterExternalClose(t *testing.T) {
	workerIdentity := &Identity{
		ID:      "cognition-redial-test",
		Type:    NodeTypeCognition,
		Version: "test",
	}

	dialerIdentity := &Identity{
		ID:      "bff-redial-test",
		Type:    NodeTypeBFF,
		Version: "test",
	}

	// Start the "worker" NodeService server directly. We bypass NodeServer
	// here because NodeServer.Stop currently does not cancel grpc.Server.Serve
	// (its run hook ignores ctx and blocks on Accept), and we need a clean
	// shutdown path to keep this test from hanging on cleanup.
	workerPM := NewPeerManager(workerIdentity, testLogger())

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	workerAddr := listener.Addr().String()
	workerIdentity.Address = workerAddr

	grpcSrv := grpc.NewServer()
	svc := &nodeService{
		logger:      testLogger(),
		identity:    workerIdentity,
		peerManager: workerPM,
	}
	nodev1.RegisterNodeServiceServer(grpcSrv, svc)

	srvDone := make(chan struct{})
	go func() {
		_ = grpcSrv.Serve(listener)
		close(srvDone)
	}()
	defer func() {
		grpcSrv.GracefulStop()
		<-srvDone
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Build the BFF-side PeerManager + WorkerDialer. Seed targets point at
	// the "worker" we just started.
	bffPM := NewPeerManager(dialerIdentity, testLogger())
	bffPM.Start(ctx)
	defer bffPM.Stop(context.Background())

	seeds := []WorkerTarget{
		{NodeType: NodeTypeCognition, Address: workerAddr},
	}
	wd := NewWorkerDialer(dialerIdentity, bffPM, nil, nil, seeds, testLogger())
	if wd == nil {
		t.Fatal("expected WorkerDialer to be constructed")
	}
	wd.Start(ctx)
	defer wd.Stop(context.Background())

	// Wait for the initial handshake -- the worker's NodeId should appear
	// in the BFF's PeerManager with its outbound *peerConnection attached
	// via AttachConnection.
	waitForAttachedPeer(t, bffPM, workerIdentity.ID, 3*time.Second, "initial dial")

	// Reproduce the bug. In production this is what
	// nodeService.Stream's defer runs when the worker's inbound stream
	// (its ParentConnector) dies because the worker container restarted.
	// Remove closes entry.Connection, which is the WorkerDialer's own
	// outbound *peerConnection.
	bffPM.Remove(workerIdentity.ID)

	// With the fix: the dialTarget goroutine notices Connect() returned
	// without a shutdown signal from us, drops the entry from wd.conns,
	// and triggers an immediate reconcile. The reconcile dials again and
	// a fresh NodeWelcome re-attaches the connection.
	//
	// Without the fix: the entry sits in wd.conns forever, reconcile sees
	// the key as occupied, never re-dials, and the timeout below trips.
	waitForAttachedPeer(t, bffPM, workerIdentity.ID, 5*time.Second, "redial after external close")

	// And the dialer's internal bookkeeping should reflect the active
	// dial, not a stale reference to a dead connection.
	active := wd.ActiveAddresses()
	found := false
	for _, a := range active {
		if a == workerAddr {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("WorkerDialer.ActiveAddresses() missing %q after redial; got %v", workerAddr, active)
	}
}

// waitForAttachedPeer polls the PeerManager until the given nodeId appears
// with a non-nil Connection. Non-nil Connection is the signal that
// NodeWelcome was processed and the outbound *peerConnection was bound via
// AttachConnection -- this is what AiForwardRouter and EventBridge rely on
// to Send to the peer.
func waitForAttachedPeer(t *testing.T, pm *PeerManager, nodeId string, timeout time.Duration, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if entry := pm.Get(nodeId); entry != nil && entry.Connection != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	entry := pm.Get(nodeId)
	switch {
	case entry == nil:
		t.Fatalf("timeout waiting for peer %q to appear (%s)", nodeId, what)
	case entry.Connection == nil:
		t.Fatalf("peer %q present but Connection is nil (%s) -- AttachConnection never called", nodeId, what)
	}
}
