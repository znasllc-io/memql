package sitetraffic

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/env"
)

// SinkComponent is the writer's lifecycle: it resolves the database handle
// after the database component has started, builds the Sink, applies the
// configured retention window, and flushes what is queued on shutdown.
//
// Wired only into an EDGE node (app/transport_edge.go), because the edge is
// the only node type that serves a site. The read half is a plug-in and
// registers everywhere; only the write half is node-specific.
//
// A missing database is a WARNING, not a fatal: an edge with no handle still
// serves every site it was serving, and the honest consequence is that the
// traffic figure reads unmeasured. Refusing to boot over a metric would take
// a cluster's sites down to protect a number.
type SinkComponent struct {
	logger   *slog.Logger
	provider func() *bun.DB
	enabled  bool

	// sink is ATOMIC, and that is not defensive tidiness. It is written by
	// Start on the bootstrap goroutine and read by Record on whichever
	// goroutine is serving an HTTP request -- and on an edge those overlap by
	// construction, because the handler is mounted before the database
	// component has finished starting. A plain field here is a data race the
	// detector finds and a production node experiences as a torn pointer.
	sink atomic.Pointer[Sink]

	running atomic.Bool
	readyCh chan struct{}
	order   int
}

// ComponentName is the bus-side name.
const ComponentName = common.ComponentName("sitetraffic.sink")

var (
	_ common.Dependency = (*SinkComponent)(nil)
	_ Recorder          = (*SinkComponent)(nil)
)

// NewSinkComponent constructs the lifecycle wrapper. order should be later
// than the database component's; 0 takes 50, the observe sink's number and
// well past the standard startup phases.
func NewSinkComponent(logger *slog.Logger, provider func() *bun.DB, order int) *SinkComponent {
	if logger == nil {
		logger = slog.Default()
	}
	if order == 0 {
		order = 50
	}
	return &SinkComponent{
		logger:   logger.With("component", "sitetraffic"),
		provider: provider,
		enabled:  RequestLogEnabled(),
		readyCh:  make(chan struct{}),
		order:    order,
	}
}

// Record implements Recorder by forwarding to the sink once there is one.
//
// THE COMPONENT IS THE RECORDER, not the sink it builds, and the reason is
// startup order: the handler is constructed before the database component has
// started, so a caller handed `c.sink` would be handed nil and keep it. A
// record arriving before Start -- or on a replica where the log is switched
// off, or where no database resolved -- is dropped here, silently and
// cheaply, which is the same answer the sink's full-buffer branch gives.
//
// Not counted as a drop: nothing was measuring yet, and a counter that
// climbed during every boot would put a permanent floor under the one series
// an operator is meant to alert on.
func (c *SinkComponent) Record(r Record) {
	sink := c.sink.Load()
	if sink == nil {
		return
	}
	sink.Record(r)
}

func (c *SinkComponent) Start(ctx context.Context) {
	defer close(c.readyCh)
	if !c.enabled {
		c.logger.Info("the edge request log is off on this replica (MEMQL_EDGE_REQUEST_LOG_ENABLED=false); traffic figures will not count what it serves")
		return
	}
	db := c.provider()
	for i := 0; i < 10 && db == nil; i++ {
		// The database component's Start may still be in flight; the observe
		// sink waits the same way rather than ordering itself around it.
		time.Sleep(100 * time.Millisecond)
		db = c.provider()
	}
	if db == nil {
		c.logger.Warn("the edge request log has no database handle; this replica serves sites normally and records no traffic")
		return
	}

	sink := NewSink(db, SinkOptions{Logger: c.logger, Node: nodeId()})
	sink.Start()
	// PUBLISHED LAST. The store happens after Start, so the first record a
	// serving goroutine files can never reach a sink whose drain is not yet
	// running -- it is dropped by the nil branch instead, which is the honest
	// outcome for a request served before this node was recording.
	c.sink.Store(sink)
	c.running.Store(true)

	days := RetentionDays()
	if err := ApplyRetention(ctx, db, days); err != nil {
		// NOT FATAL, and it is worth being clear about what an operator has
		// then: the migration's own thirty-day policies are still in force,
		// so the data is bounded -- it is the CONFIGURED window that did not
		// take, which is a difference the message has to name or somebody
		// will read it as unbounded growth.
		c.logger.Warn("could not apply the request log's retention window; the migration's default of 30 days stays in force",
			"requestedDays", days, "err", err)
	}
	c.logger.Info("the edge request log is recording", "retentionDays", days, "node", nodeId())
}

func (c *SinkComponent) Stop(ctx context.Context) {
	sink := c.sink.Load()
	if sink == nil {
		return
	}
	written, dropped := sink.Stats()
	if err := sink.Stop(ctx); err != nil {
		c.logger.Warn("the request log did not finish flushing before shutdown", "err", err)
	}
	c.running.Store(false)
	c.logger.Info("the edge request log stopped", "written", written, "dropped", dropped)
}

func (c *SinkComponent) IsRunning() bool { return c.running.Load() }
func (c *SinkComponent) Order() int      { return c.order }
func (c *SinkComponent) ComponentName() common.ComponentName {
	return ComponentName
}
func (c *SinkComponent) Ready() <-chan struct{} { return c.readyCh }

// RequestLogEnabled reads MEMQL_EDGE_REQUEST_LOG_ENABLED. ON BY DEFAULT: a
// deployable's Live stop asks "is anybody using it", and a cluster where
// nobody had opted in would answer unmeasured forever with nothing to say why.
func RequestLogEnabled() bool {
	reader := env.NewEnvReader("MEMQL_EDGE_REQUEST_LOG")
	v, err := reader.OptionalBool("ENABLED")
	if err != nil || v == nil {
		return true
	}
	return *v
}

// Retention bounds.
const (
	// DefaultRetentionDays matches the migration's own policies and the log
	// store's thirty days.
	DefaultRetentionDays = 30
	minRetentionDays     = 1
	maxRetentionDays     = 365
)

// RetentionDays reads MEMQL_EDGE_REQUEST_LOG_RETENTION_DAYS, clamped. An
// unparseable value takes the default rather than zero, which would ask the
// database to drop everything the moment it was written.
func RetentionDays() int {
	reader := env.NewEnvReader("MEMQL_EDGE_REQUEST_LOG")
	v, err := reader.OptionalInt("RETENTION_DAYS")
	if err != nil || v == nil {
		return DefaultRetentionDays
	}
	switch {
	case *v < minRetentionDays:
		return minRetentionDays
	case *v > maxRetentionDays:
		return maxRetentionDays
	default:
		return *v
	}
}

// ApplyRetention points the three TimescaleDB retention policies at the
// configured window -- the raw rows and both aggregates, in step, so
// "unmeasured" means the same thing at every horizon.
//
// A no-op without TimescaleDB: a plain Postgres box has no policies to move,
// and the views the migration created there are computed live from rows
// nothing drops. That is a real difference and it is the developer box's,
// which is why it is a log line rather than a refusal.
func ApplyRetention(ctx context.Context, db *bun.DB, days int) error {
	if db == nil {
		return fmt.Errorf("no database handle")
	}
	var installed bool
	if err := db.NewRaw("SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'timescaledb')").
		Scan(ctx, &installed); err != nil {
		return fmt.Errorf("check for timescaledb: %w", err)
	}
	if !installed {
		return nil
	}
	interval := fmt.Sprintf("%d days", days)
	for _, relation := range []string{"edge_request", "edge_request_1m", "edge_request_1h"} {
		// remove_retention_policy first: add_retention_policy refuses to
		// change an existing policy's interval, so an add alone would leave
		// the migration's thirty days in place and report success.
		if _, err := db.NewRaw("SELECT remove_retention_policy(?, if_exists => TRUE)", relation).Exec(ctx); err != nil {
			return fmt.Errorf("clear the retention policy on %s: %w", relation, err)
		}
		if _, err := db.NewRaw("SELECT add_retention_policy(?, ?::interval, if_not_exists => TRUE)", relation, interval).Exec(ctx); err != nil {
			return fmt.Errorf("set the retention policy on %s: %w", relation, err)
		}
	}
	return nil
}

// nodeId resolves which replica is writing. Read once at Start rather than
// per request, and falling back to the hostname the way every other node-id
// reader in the tree does.
func nodeId() string {
	if v := os.Getenv("MEMQL_NODE_ID"); v != "" {
		return v
	}
	host, err := os.Hostname()
	if err != nil {
		return ""
	}
	return host
}
