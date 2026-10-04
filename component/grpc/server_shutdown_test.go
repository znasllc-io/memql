package memql

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// The MemqlService gRPC server must stop when its lifecycle context is
// cancelled, the same defect memql#1119 fixed in NodeServer and left here.
//
// Before the fix run() was a bare grpcServer.Serve(listener), which ignores
// ctx. The only GracefulStop lived in the OnStop hook, and OnStop runs only
// AFTER run returns -- which Serve never did, because nothing called Stop on
// it. Server.Stop therefore parked on the lifecycle until the caller's
// deadline: on a draining agent (2026-10-04, k3d, goroutine dump taken
// mid-sweep) goroutine 1 sat in grpc.(*Server).Stop for the whole 30s shared
// shutdown budget, and every dependency after it in the sweep (WorkerDialer,
// NodeServer, RunDelivery, ParentConnector, EventBridge, PeerManager) was
// handed an already-expired context and logged "failed to stop" in the same
// instant -- while NodeServer went on accepting mesh streams it could no
// longer serve, its database pool already released.

func newShutdownTestServer(t *testing.T, interceptor grpc.StreamServerInterceptor) *Server {
	t.Helper()
	// prepareForRun fails on a set-but-unreadable TLS pair; do not inherit the
	// developer's environment.
	t.Setenv("MEMQL_GRPC_TLS_CERT_FILE", "")
	t.Setenv("MEMQL_GRPC_TLS_KEY_FILE", "")

	srv := NewServer("127.0.0.1:0", slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv.SetStreamInterceptor(interceptor)
	srv.Start(context.Background())

	select {
	case <-srv.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("gRPC server did not become ready")
	}
	if !srv.IsRunning() {
		t.Fatal("gRPC server is not running after Start")
	}
	return srv
}

// stopWithin calls Stop under a deadline far longer than the bound the server
// is expected to honour, and fails if Stop has not returned and the lifecycle
// ended within `within`. A Stop that returns only because the context ran out
// leaves the lifecycle running, which is the regression.
func stopWithin(t *testing.T, srv *Server, within time.Duration) {
	t.Helper()
	stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	done := make(chan struct{})
	start := time.Now()
	go func() {
		srv.Stop(stopCtx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(within):
		t.Fatalf("Server.Stop did not return within %s: run() is not stopping the gRPC server on ctx cancel, "+
			"so Stop waits out the caller's whole deadline and starves every later dependency in the sweep", within)
	}
	if srv.IsRunning() {
		t.Fatalf("Server.Stop returned after %s with the lifecycle still running: it gave up on the context "+
			"instead of the server stopping", time.Since(start))
	}
}

func TestServer_StopReturnsPromptlyOnCtxCancel(t *testing.T) {
	srv := newShutdownTestServer(t, func(s any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, h grpc.StreamHandler) error {
		return h(s, ss)
	})
	stopWithin(t, srv, 3*time.Second)
}

// A long-lived stream (a browser over the WebSocket bridge, a cockpit's held
// WorkerService stream) never ends on its own, so an UNBOUNDED GracefulStop
// would wedge the sweep exactly as the bare Serve did. The bound forces it.
func TestServer_StopForcesAHeldStreamAfterTheGracefulBound(t *testing.T) {
	prev := grpcServerGracefulStopTimeout
	grpcServerGracefulStopTimeout = 200 * time.Millisecond
	t.Cleanup(func() { grpcServerGracefulStopTimeout = prev })

	held := make(chan struct{})
	srv := newShutdownTestServer(t, func(_ any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, _ grpc.StreamHandler) error {
		close(held)
		<-ss.Context().Done() // never returns until the transport is torn down
		return ss.Context().Err()
	})

	conn, err := grpc.NewClient(srv.listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	clientCtx, cancelClient := context.WithCancel(context.Background())
	defer cancelClient()
	if _, err := memqlv1.NewMemqlServiceClient(conn).Stream(clientCtx); err != nil {
		t.Fatalf("open stream: %v", err)
	}
	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream never reached the server")
	}

	stopWithin(t, srv, 3*time.Second)
}
