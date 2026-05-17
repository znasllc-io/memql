package observe

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/core/common"
)

// ComponentName is the bus-side identifier for the observability
// runtime. Used in lifecycle logs and in the dependency ordering
// math; not surfaced to end users.
const ComponentName = common.ComponentName("observe")

// BunDBProvider is the narrow seam between the observe runtime and
// the memQL database component. The sink doesn't import the database
// package directly because that's a heavy graph; instead the
// bootstrap hands it a closure that resolves to the underlying
// *bun.DB at Start time. Returns nil if the database isn't ready yet
// (the sink retries lazily).
type BunDBProvider func() *bun.DB

// SinkComponent is the dependency-bus integration for the observe
// runtime. Owns the TimescaleSink lifecycle so the app bootstrap
// can wire it in alongside the other components and shutdown
// flushes the buffered records cleanly.
type SinkComponent struct {
	logger    *slog.Logger
	provider  BunDBProvider
	opts      TimescaleSinkOptions
	sink      *TimescaleSink
	running   atomic.Bool
	readyCh   chan struct{}
	order     int
	stopGuard atomic.Bool
}

// NewSinkComponent constructs the dependency wrapper. Order should
// be higher than the database component's order so this component
// starts AFTER the DB; pass 0 to fall back to a default that's
// safely later than the standard memql startup phases.
func NewSinkComponent(logger *slog.Logger, provider BunDBProvider, opts TimescaleSinkOptions, order int) *SinkComponent {
	if logger == nil {
		logger = slog.Default()
	}
	if order == 0 {
		// Database is around Order=2; we want to start later. 50 is
		// well past anything else in the standard chain.
		order = 50
	}
	return &SinkComponent{
		logger:   logger.With("component", "observe"),
		provider: provider,
		opts:     opts,
		readyCh:  make(chan struct{}),
		order:    order,
	}
}

// Start resolves the bun handle, constructs the sink, registers it
// as the active observe sink, and kicks off the buffered drain.
// Failure to resolve the DB is logged but not fatal -- the default
// slog sink stays active and the runtime keeps working at LevelOff /
// LevelCount without persistence.
func (c *SinkComponent) Start(_ context.Context) {
	db := c.provider()
	if db == nil {
		// Try a few short retries; the DB component's Start may
		// be in flight when we land here.
		for i := 0; i < 10 && db == nil; i++ {
			time.Sleep(100 * time.Millisecond)
			db = c.provider()
		}
	}
	if db == nil {
		c.logger.Warn("observe sink: no bun.DB available -- falling back to slog sink")
		close(c.readyCh)
		return
	}
	c.sink = NewTimescaleSink(db, c.opts)
	c.sink.Start()
	Register(c.sink)
	c.running.Store(true)
	close(c.readyCh)
	c.logger.Info("observe sink started",
		"default_level", DefaultLevel().String(),
		"buffer_size", c.opts.defaults().BufferSize,
		"flush_interval", c.opts.defaults().FlushInterval)
}

// Stop flushes the remaining buffer and reinstalls the default
// slog sink so any in-flight callers writing through observe.Method
// don't NPE on a closed sink.
func (c *SinkComponent) Stop(ctx context.Context) {
	if !c.stopGuard.CompareAndSwap(false, true) {
		return
	}
	if c.sink != nil {
		_ = c.sink.Stop(ctx)
		written, dropped, flushes, errs := c.sink.Stats()
		c.logger.Info("observe sink stopped",
			"written", written, "dropped", dropped, "flushes", flushes, "errors", errs)
	}
	// Drop back to the safe default so post-stop writes still go
	// somewhere benign.
	Register(nil)
	c.running.Store(false)
}

// IsRunning, Order, ComponentName, Ready -- standard Dependency
// surface. Ready closes when Start has finished its setup
// regardless of whether the DB resolved (the runtime should not
// block other components on us being healthy).
func (c *SinkComponent) IsRunning() bool             { return c.running.Load() }
func (c *SinkComponent) Order() int                  { return c.order }
func (c *SinkComponent) ComponentName() common.ComponentName { return ComponentName }
func (c *SinkComponent) Ready() <-chan struct{}      { return c.readyCh }
