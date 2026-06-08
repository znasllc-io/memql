package node

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNodeServer_StopReturnsPromptlyOnCtxCancel pins the #1119 fix: the
// NodeService gRPC server must shut down when its lifecycle context is
// cancelled. Before the fix, run() called grpcServer.Serve directly and
// ignored ctx cancellation -- the only GracefulStop lived in the OnStop hook,
// which never ran because Serve never returned. Stop therefore blocked until
// the shared shutdown budget expired (~30s), starving every later dependency.
//
// With the fix, run() owns a ctx-driven, bounded GracefulStop, so Stop returns
// effectively immediately for an idle server. The generous outer deadline here
// is a guard against regressing to the old block-until-timeout behavior.
func TestNodeServer_StopReturnsPromptlyOnCtxCancel(t *testing.T) {
	t.Setenv("MEMQL_NODE_SERVICE_ADDRESS", "127.0.0.1:0")

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	identity := NewIdentity("test")
	pm := NewPeerManager(identity, logger)
	srv := NewNodeServer(identity, pm, logger)

	startCtx, startCancel := context.WithCancel(context.Background())
	defer startCancel()
	srv.Start(startCtx)

	select {
	case <-srv.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("node server did not become ready")
	}
	require.True(t, srv.IsRunning(), "server should be running after Start")

	done := make(chan struct{})
	go func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		srv.Stop(stopCtx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("NodeServer.Stop did not return promptly on ctx cancel (regressed to unbounded GracefulStop)")
	}

	assert.False(t, srv.IsRunning(), "server should not be running after Stop")
}

// TestNodeServer_FastStartStopNoPanic exercises the race the goroutine-based
// run() can hit: if Stop fires before the Serve goroutine is scheduled, the
// OnStop cleanup hook nils grpcServer/listener while that goroutine is about
// to read them. run() captures both into locals to stay immune; this test
// drives the Start->immediate-Stop path repeatedly under -race to pin it.
func TestNodeServer_FastStartStopNoPanic(t *testing.T) {
	t.Setenv("MEMQL_NODE_SERVICE_ADDRESS", "127.0.0.1:0")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	for i := 0; i < 20; i++ {
		identity := NewIdentity("test")
		pm := NewPeerManager(identity, logger)
		srv := NewNodeServer(identity, pm, logger)

		ctx, cancel := context.WithCancel(context.Background())
		srv.Start(ctx)
		cancel() // cancel immediately, often before the Serve goroutine runs
		srv.Stop(context.Background())
	}
}
