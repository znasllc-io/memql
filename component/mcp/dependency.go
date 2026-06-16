package mcp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"

	"github.com/znasllc-io/memql/core/common"
)

// stdioDependency runs a Server over a fixed reader/writer pair as a
// common.Dependency so the MCP head participates in the standard service
// lifecycle (parallel startup, ordered shutdown, /healthz readiness).
type stdioDependency struct {
	server *Server
	in     io.Reader
	out    io.Writer
	logger *slog.Logger

	cancel  context.CancelFunc
	running atomic.Bool
	readyCh chan struct{}
	doneCh  chan struct{}
}

// NewStdioDependency wraps server's Serve loop over (in, out) as a
// common.Dependency.
func NewStdioDependency(server *Server, in io.Reader, out io.Writer, logger *slog.Logger) common.Dependency {
	if logger == nil {
		logger = slog.Default()
	}
	return &stdioDependency{
		server:  server,
		in:      in,
		out:     out,
		logger:  logger,
		readyCh: make(chan struct{}),
		doneCh:  make(chan struct{}),
	}
}

// Start launches the Serve loop in the background and returns immediately --
// the lifecycle starts dependencies sequentially, so Start must not block.
// Cancellation arrives via Stop.
func (d *stdioDependency) Start(ctx context.Context) {
	runCtx, cancel := context.WithCancel(context.Background())
	d.cancel = cancel
	d.running.Store(true)
	close(d.readyCh)

	go func() {
		defer close(d.doneCh)
		defer d.running.Store(false)
		if err := d.server.Serve(runCtx, d.in, d.out); err != nil && !errors.Is(err, context.Canceled) {
			d.logger.Error("mcp stdio server exited with error", "error", err)
			return
		}
		d.logger.Info("mcp stdio server stopped")
	}()

	d.logger.Info("mcp stdio server started")
}

// Stop cancels the Serve loop and waits for it to unwind, bounded by ctx. A
// read blocked on stdin cannot be interrupted, so on a clean shutdown (no
// client disconnect) this returns when ctx (the shutdown timeout) fires.
func (d *stdioDependency) Stop(ctx context.Context) {
	if d.cancel != nil {
		d.cancel()
	}
	select {
	case <-d.doneCh:
	case <-ctx.Done():
	}
}

func (d *stdioDependency) IsRunning() bool { return d.running.Load() }

// Order places the MCP head near the top of the stack so it stops before the
// engine/database underneath it during shutdown.
func (d *stdioDependency) Order() int { return 80 }

func (d *stdioDependency) ComponentName() common.ComponentName { return ComponentName }

func (d *stdioDependency) Ready() <-chan struct{} { return d.readyCh }
