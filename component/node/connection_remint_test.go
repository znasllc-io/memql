package node

import (
	"context"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	"github.com/znasllc-io/memql/core/id"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// keyRotatingNodeService is a NodeService server that models an identity key
// rotation (memql#1521). It accepts a bearer token ONLY when that token is in
// its current acceptedKeys set; everything else is rejected with
// codes.Unauthenticated "invalid or expired token: verifier: unknown kid ..."
// -- byte-for-byte the rejection a leaf sees in production after identity
// rotates its signing key. The test rotates the accepted set at runtime to
// simulate the rotation, and counts both rejected and accepted handshakes so
// the stuck-loop vs. recovered behaviour is observable.
type keyRotatingNodeService struct {
	nodev1.UnimplementedNodeServiceServer

	selfID string

	mu           sync.Mutex
	acceptedKeys map[string]bool // tokens this server currently honours

	rejectedHandshakes int64 // atomic: dials rejected with Unauthenticated
	acceptedHandshakes int64 // atomic: NodeHello handshakes that completed
}

func newKeyRotatingNodeService(selfID string, initialKey string) *keyRotatingNodeService {
	return &keyRotatingNodeService{
		selfID:       selfID,
		acceptedKeys: map[string]bool{initialKey: true},
	}
}

// rotateTo replaces the accepted-token set with exactly newKey: tokens minted
// under the previous key now fail (the rotation), tokens minted under newKey
// pass.
func (s *keyRotatingNodeService) rotateTo(newKey string) {
	s.mu.Lock()
	s.acceptedKeys = map[string]bool{newKey: true}
	s.mu.Unlock()
}

func (s *keyRotatingNodeService) accepts(tok string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.acceptedKeys[tok]
}

func (s *keyRotatingNodeService) Stream(stream nodev1.NodeService_StreamServer) error {
	// Mirror the real class-pin interceptor (component/node/auth.go): pull the
	// bearer token off the inbound metadata and reject an unknown/rotated key
	// with codes.Unauthenticated BEFORE the handshake, so the client's dial
	// fails exactly like a post-rotation leaf.
	tok := bearerFromIncoming(stream.Context())
	if !s.accepts(tok) {
		atomic.AddInt64(&s.rejectedHandshakes, 1)
		return status.Error(codes.Unauthenticated,
			"invalid or expired token: verifier: unknown kid \"Xu4zLjaN3Q8\"")
	}

	first, err := stream.Recv()
	if err != nil {
		return err
	}
	if first.GetNodeHello() == nil {
		return nil
	}
	atomic.AddInt64(&s.acceptedHandshakes, 1)

	if err := stream.Send(&nodev1.NodeServerMessage{
		MessageId: id.NewShortId(),
		Payload: &nodev1.NodeServerMessage_NodeWelcome{
			NodeWelcome: &nodev1.NodeWelcome{
				NodeId:   s.selfID,
				NodeType: string(NodeTypeBFF),
			},
		},
	}); err != nil {
		return err
	}

	go func() {
		for {
			if _, err := stream.Recv(); err != nil {
				return
			}
		}
	}()

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case <-ticker.C:
			if err := stream.Send(buildServerHeartbeat(nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY)); err != nil {
				return err
			}
		}
	}
}

func bearerFromIncoming(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	vals := md.Get("authorization")
	if len(vals) == 0 {
		return ""
	}
	parts := strings.Fields(vals[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return parts[1]
}

// TestPeerConnection_RemintsNodeTokenOnAuthRejection is the memql#1521
// regression guard for the cross-node stuck loop: when identity rotates its
// signing key, a leaf holding a token signed by the OLD key is rejected by the
// parent (Unauthenticated / unknown kid) on every dial and -- before the fix
// -- loops forever reusing the dead token, never recovering without a pod
// restart.
//
// With the re-mint hook wired the connection re-acquires a token signed by the
// CURRENT key on the auth rejection and the next dial succeeds, all WITHOUT a
// process/pod restart. This is a cross-node test by construction: the rejection
// only happens across the client<->server NodeService stream boundary.
//
// It FAILS against pre-fix code (no reauthFn -> the client keeps presenting
// "token-A", acceptedHandshakes stays 0) and PASSES with the fix.
func TestPeerConnection_RemintsNodeTokenOnAuthRejection(t *testing.T) {
	const oldKeyToken = "token-signed-by-key-A"
	const newKeyToken = "token-signed-by-key-B"

	// Server starts honouring the OLD key; the client boots with the matching
	// old-key token.
	svc := newKeyRotatingNodeService("bff-parent", oldKeyToken)

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

	identity := &Identity{
		ID:          "agent-leaf",
		Type:        NodeTypeAgent,
		Version:     "test",
		Address:     "agent:50055",
		BearerToken: oldKeyToken,
	}
	pc := newPeerConnection(identity, "", addr, testLogger())
	pc.SetHeartbeatInterval(100 * time.Millisecond)

	// The re-mint hook models identity now serving the rotated key: it returns
	// the NEW-key token and stores it on the shared Identity, exactly as the
	// production RemintBearerToken does after hitting /node/bootstrap.
	var remintCount int64
	pc.SetReauthFn(func(ctx context.Context) (string, error) {
		atomic.AddInt64(&remintCount, 1)
		identity.setBearerToken(newKeyToken)
		return newKeyToken, nil
	})

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

	// The first handshake completes on the old key.
	waitForCount(t, &svc.acceptedHandshakes, 1, 3*time.Second, "initial handshake on old key")

	// Identity rotates its signing key. The leaf's live stream dies and every
	// reconnect now presents the OLD-key token, which the server rejects with
	// Unauthenticated -- the production stuck-loop trigger.
	svc.rotateTo(newKeyToken)

	// Force the live stream to drop so the reconnect loop re-dials and hits the
	// now-rejecting server. Closing the underlying conn unblocks Recv().
	pc.mu.Lock()
	if pc.conn != nil {
		pc.conn.Close()
	}
	pc.mu.Unlock()

	// Recovery: the rejection triggers a re-mint (new-key token) and the next
	// dial completes a SECOND accepted handshake -- no pod restart. Pre-fix,
	// acceptedHandshakes would never reach 2 (the client keeps sending the dead
	// old-key token and the server keeps rejecting it).
	waitForCount(t, &svc.acceptedHandshakes, 2, 10*time.Second,
		"reconnect after identity key rotation via node-token re-mint (memql#1521)")

	if got := atomic.LoadInt64(&remintCount); got < 1 {
		t.Fatalf("expected at least one token re-mint after the auth rejection, got %d", got)
	}
	if got := atomic.LoadInt64(&svc.rejectedHandshakes); got < 1 {
		t.Fatalf("expected at least one rejected dial after the rotation (the stuck-loop trigger), got %d", got)
	}
}

// TestPeerConnection_NoRemintWithoutHook proves the legacy behaviour is
// preserved: without a reauthFn (an out-of-band MEMQL_NODE_TOKEN that cannot be
// re-minted, or single-node dev), a key rotation leaves the leaf stuck -- it
// keeps presenting the dead token and never completes a second handshake. This
// is the exact stuck loop the fix targets, and it documents that the recovery
// is gated entirely on the re-mint hook being wired.
func TestPeerConnection_NoRemintWithoutHook(t *testing.T) {
	const oldKeyToken = "token-signed-by-key-A"
	const newKeyToken = "token-signed-by-key-B"

	svc := newKeyRotatingNodeService("bff-parent", oldKeyToken)

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

	identity := &Identity{
		ID:          "agent-leaf-no-hook",
		Type:        NodeTypeAgent,
		Version:     "test",
		Address:     "agent:50055",
		BearerToken: oldKeyToken,
	}
	pc := newPeerConnection(identity, "", addr, testLogger())
	pc.SetHeartbeatInterval(100 * time.Millisecond)
	// No SetReauthFn -- legacy behaviour.

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

	waitForCount(t, &svc.acceptedHandshakes, 1, 3*time.Second, "initial handshake on old key")

	svc.rotateTo(newKeyToken)
	pc.mu.Lock()
	if pc.conn != nil {
		pc.conn.Close()
	}
	pc.mu.Unlock()

	// Wait long enough for several reconnect attempts; without the hook the
	// count must stay at 1 (stuck loop) -- proving the rotation genuinely
	// breaks the connection and the recovery depends on the re-mint.
	time.Sleep(2500 * time.Millisecond)
	if got := atomic.LoadInt64(&svc.acceptedHandshakes); got != 1 {
		t.Fatalf("expected to stay stuck at 1 handshake without a re-mint hook, got %d", got)
	}
	if got := atomic.LoadInt64(&svc.rejectedHandshakes); got < 1 {
		t.Fatalf("expected rejected dials after the rotation, got %d", got)
	}
}
