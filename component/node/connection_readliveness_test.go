package node

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	"github.com/znasllc-io/memql/core/id"
	"google.golang.org/grpc"
)

// halfDeadNodeService is a NodeService server that completes the handshake
// (NodeHello -> NodeWelcome) and then can be told to go SILENT: it stops
// sending server heartbeats and stops responding, but it deliberately does NOT
// close the stream -- emulating a draining old-color bff pod after a
// blue-green cutover (the memql#1388 half-dead parent). Each NodeHello it
// receives bumps helloCount, so a reconnect-driven fresh handshake is
// observable.
type halfDeadNodeService struct {
	nodev1.UnimplementedNodeServiceServer

	selfID string

	mu     sync.Mutex
	silent bool // when true: send no heartbeats, never close the stream

	helloCount int64 // atomic: total NodeHello handshakes completed
}

func (s *halfDeadNodeService) goSilent() {
	s.mu.Lock()
	s.silent = true
	s.mu.Unlock()
}

func (s *halfDeadNodeService) isSilent() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.silent
}

func (s *halfDeadNodeService) Stream(stream nodev1.NodeService_StreamServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	if first.GetNodeHello() == nil {
		return nil
	}
	atomic.AddInt64(&s.helloCount, 1)

	// NodeWelcome so the client registers us + a sibling peer and binds its
	// outbound connection (mirrors a real handshake).
	if err := stream.Send(&nodev1.NodeServerMessage{
		MessageId: id.NewShortId(),
		Payload: &nodev1.NodeServerMessage_NodeWelcome{
			NodeWelcome: &nodev1.NodeWelcome{
				NodeId:   s.selfID,
				NodeType: string(NodeTypeBFF),
				Peers: []*nodev1.PeerInfo{{
					NodeId:   "planner-sibling",
					NodeType: string(NodeTypePlanner),
					Address:  "planner:50056",
					Health:   nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY,
				}},
			},
		},
	}); err != nil {
		return err
	}

	// Drain inbound in the background so client sends don't block.
	go func() {
		for {
			if _, err := stream.Recv(); err != nil {
				return
			}
		}
	}()

	// Server heartbeat ticker -- until told to go silent, after which we hold
	// the stream open and emit nothing (the half-dead condition).
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case <-ticker.C:
			if s.isSilent() {
				continue // half-dead: keep the stream open, send nothing
			}
			if err := stream.Send(buildServerHeartbeat(nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY)); err != nil {
				return err
			}
		}
	}
}

// TestPeerConnection_ReadLivenessReconnectsOnHalfDeadStream is the memql#1388
// regression guard for the CORE outage mechanism: a leaf wedged on a half-dead
// parent stream after a bff blue-green cutover. The parent keeps the gRPC
// stream ESTABLISHED (no EOF/RST) but stops sending server heartbeats and
// stops re-advertising peers -- so stream.Recv() blocks forever and, without a
// read-liveness deadline, connectOnce never returns: the leaf's events never
// leave the node and its routing table decays.
//
// With the watchdog the inbound-silent stream is torn down after
// readLivenessTimeout and the supervising reconnect loop re-dials, completing a
// FRESH handshake (a second NodeHello) that re-runs NodeWelcome and so
// re-advertises the peer set -- exactly the recovery a real cutover needs.
//
// This is a cross-node (client<->server stream) test by construction: the bug
// only exists across a node boundary and is invisible to single-node code.
func TestPeerConnection_ReadLivenessReconnectsOnHalfDeadStream(t *testing.T) {
	svc := &halfDeadNodeService{selfID: "bff-half-dead"}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := listener.Addr().String()

	grpcSrv := grpc.NewServer()
	nodev1.RegisterNodeServiceServer(grpcSrv, svc)
	srvDone := make(chan struct{})
	go func() {
		_ = grpcSrv.Serve(listener)
		close(srvDone)
	}()
	defer func() {
		grpcSrv.Stop()
		<-srvDone
	}()

	identity := &Identity{ID: "agent-leaf", Type: NodeTypeAgent, Version: "test", Address: "agent:50055"}
	pc := newPeerConnection(identity, "", addr, testLogger())
	pc.SetHeartbeatInterval(100 * time.Millisecond)
	// Short read-liveness deadline so the test runs quickly. The server
	// heartbeats every 200ms while healthy, so 600ms tolerates a couple of
	// missed beats before declaring the stream dead.
	pc.SetReadLivenessTimeout(600 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	connectDone := make(chan struct{})
	go func() {
		_ = pc.Connect(ctx, func(*nodev1.NodeServerMessage) {})
		close(connectDone)
	}()
	defer func() {
		pc.Close()
		<-connectDone
	}()

	// First handshake completes.
	waitForCount(t, &svc.helloCount, 1, 3*time.Second, "initial handshake")

	// The parent goes half-dead: stream stays open, no more heartbeats. With
	// the watchdog this is detected within ~readLivenessTimeout and the client
	// re-dials, producing a SECOND NodeHello. Without the watchdog connectOnce
	// would block on Recv() forever and helloCount would stay at 1 (the bug).
	svc.goSilent()

	// Let the reconnect happen, then let the server stream healthy again so the
	// re-dialed stream stabilizes (proves the recovery path actually
	// re-establishes a live link, not just thrashes).
	time.AfterFunc(900*time.Millisecond, func() {
		svc.mu.Lock()
		svc.silent = false
		svc.mu.Unlock()
	})

	waitForCount(t, &svc.helloCount, 2, 5*time.Second,
		"reconnect after half-dead parent stream (memql#1388)")
}

// TestPeerConnection_NoReconnectWhileHealthy guards against the watchdog
// firing on a HEALTHY stream: while the server heartbeats normally there must
// be exactly ONE handshake -- the read-liveness deadline must be reset by
// inbound heartbeats and never trip.
func TestPeerConnection_NoReconnectWhileHealthy(t *testing.T) {
	svc := &halfDeadNodeService{selfID: "bff-healthy"}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := listener.Addr().String()

	grpcSrv := grpc.NewServer()
	nodev1.RegisterNodeServiceServer(grpcSrv, svc)
	srvDone := make(chan struct{})
	go func() {
		_ = grpcSrv.Serve(listener)
		close(srvDone)
	}()
	defer func() {
		grpcSrv.Stop()
		<-srvDone
	}()

	identity := &Identity{ID: "agent-leaf-2", Type: NodeTypeAgent, Version: "test", Address: "agent:50055"}
	pc := newPeerConnection(identity, "", addr, testLogger())
	pc.SetHeartbeatInterval(100 * time.Millisecond)
	pc.SetReadLivenessTimeout(600 * time.Millisecond) // server beats every 200ms

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	connectDone := make(chan struct{})
	go func() {
		_ = pc.Connect(ctx, func(*nodev1.NodeServerMessage) {})
		close(connectDone)
	}()
	defer func() {
		pc.Close()
		<-connectDone
	}()

	waitForCount(t, &svc.helloCount, 1, 3*time.Second, "initial handshake")

	// Stay healthy across multiple read-liveness windows; the count must hold
	// at 1 (no spurious reconnect).
	time.Sleep(2 * time.Second)
	if got := atomic.LoadInt64(&svc.helloCount); got != 1 {
		t.Fatalf("expected exactly 1 handshake on a healthy stream, got %d (watchdog firing spuriously)", got)
	}
}

func waitForCount(t *testing.T, counter *int64, want int64, timeout time.Duration, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if atomic.LoadInt64(counter) >= want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for count>=%d (%s); got %d", want, what, atomic.LoadInt64(counter))
}
