package logstore

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/logger"
)

// ComponentName is the bus-side identifier for the log store's sink. Used in
// lifecycle logs and dependency ordering; not surfaced to end users.
const ComponentName = common.ComponentName("logstore")

// BunDBProvider resolves the database handle at Start time, or nil when the
// database is not ready yet (the component retries briefly). The same seam
// component/observe.SinkComponent uses, for the same reason: this package
// must not import the database component's graph.
type BunDBProvider func() *bun.DB

// SinkComponent owns the Sink's lifecycle so app/database.go can wire it in
// beside the observe sink on every node type, and shutdown flushes the queue.
//
// On a node with no database the component installs a no-op sink and says so
// once: the pre-boot ring drains into nothing, the console keeps every line
// it kept, and logsRecordClient answers that this node keeps no lines.
type SinkComponent struct {
	logger    *slog.Logger
	provider  BunDBProvider
	opts      SinkOptions
	sink      *Sink
	running   atomic.Bool
	readyCh   chan struct{}
	order     int
	stopGuard atomic.Bool
}

// NewSinkComponent constructs the dependency wrapper. order 0 falls back to
// 50, safely after the database component (around 2) has started.
func NewSinkComponent(log *slog.Logger, provider BunDBProvider, opts SinkOptions, order int) *SinkComponent {
	if log == nil {
		log = slog.Default()
	}
	if order == 0 {
		order = 50
	}
	if opts.Logger == nil {
		opts.Logger = log
	}
	return &SinkComponent{
		logger:   log.With("component", storeComponent),
		provider: provider,
		opts:     opts,
		readyCh:  make(chan struct{}),
		order:    order,
	}
}

// noopSink drops every line. Installed when the node has no database so the
// pre-boot ring is released rather than held for the life of the process.
type noopSink struct{}

func (noopSink) Write(logger.Line) {}

// Start resolves the bun handle, builds and starts the sink, registers it as
// the process's logger.Sink (which drains the pre-boot ring into it) and as
// logstore.Current().
func (c *SinkComponent) Start(_ context.Context) {
	db := c.provider()
	for i := 0; i < 10 && db == nil; i++ {
		time.Sleep(100 * time.Millisecond)
		db = c.provider()
	}
	if db == nil {
		c.logger.Warn("log store: no bun.DB available -- this node keeps no log lines; the console is unaffected")
		logger.SetSink(noopSink{})
		close(c.readyCh)
		return
	}
	c.sink = NewSink(db, c.opts)
	c.sink.Start()
	setCurrent(c.sink)
	logger.SetSink(c.sink)
	c.running.Store(true)
	close(c.readyCh)
	// Under component `logs`, not `logs.store`: this is the one line that says
	// the store is up, and it should be readable from the store.
	c.opts.Logger.Info("log store started",
		"component", gapComponent,
		"level", LevelName(),
		"max_lines_per_second", c.sink.MaxLinesPerSecond(),
		"queue", c.sink.Stats().QueueSize,
		"node_type", c.sink.NodeType(),
		"node", c.sink.Node(),
		"retention_days", RetentionDays(),
		"archive_container", ArchiveContainer())
}

// Stop unregisters the sink first -- so lines written during the flush go to
// the ring rather than a closing queue -- then flushes and reports.
func (c *SinkComponent) Stop(ctx context.Context) {
	if !c.stopGuard.CompareAndSwap(false, true) {
		return
	}
	logger.SetSink(nil)
	setCurrent(nil)
	if c.sink != nil {
		_ = c.sink.Stop(ctx)
		st := c.sink.Stats()
		c.logger.Info("log store stopped",
			"written", st.Written, "dropped", st.Dropped(), "flushes", st.Flushes, "insert_errors", st.InsertErrors)
	}
	c.running.Store(false)
}

// Sink returns the running sink, or nil.
func (c *SinkComponent) Sink() *Sink { return c.sink }

// IsRunning, Order, ComponentName, Ready -- the standard Dependency surface.
// Ready closes when Start has finished regardless of whether the DB resolved.
func (c *SinkComponent) IsRunning() bool                     { return c.running.Load() }
func (c *SinkComponent) Order() int                          { return c.order }
func (c *SinkComponent) ComponentName() common.ComponentName { return ComponentName }
func (c *SinkComponent) Ready() <-chan struct{}              { return c.readyCh }
