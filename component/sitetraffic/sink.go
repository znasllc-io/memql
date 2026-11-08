package sitetraffic

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/metrics"
)

// Sink batches Records into the edge_request hypertable.
//
// The shape is component/observe.TimescaleSink's, and the reasoning is the
// same one that file states: the writer is INTENTIONALLY LOSSY under sustained
// pressure. When the buffer is full a record is dropped and counted, because
// the alternative -- blocking the serving path -- turns measurement into the
// thing that makes a site slow. A dropped record makes the figure low; a
// blocked write makes the site late, and only one of those is visible to a
// visitor.
//
// It differs from the observe sink in two ways that matter here:
//
//   - Drops are on a Prometheus counter, not only on an internal number. The
//     figure this feeds is read as evidence, so "the figure is low because we
//     dropped records" has to be answerable from outside the process. The
//     observe sink's counters are readable by tests and by nothing else.
//   - A flush failure is counted as dropped rows rather than only as an error,
//     for the same reason: what a reader of the figure needs to know is how
//     many requests are missing from it. And a refused batch is retried one
//     row at a time first (writeRowByRow), so the rows missing are only the
//     ones the database refused, not every request that shared their batch.
type Sink struct {
	logger        *slog.Logger
	db            *bun.DB
	node          string
	buffer        chan Record
	flushInterval time.Duration
	maxBatch      int

	startOnce sync.Once
	stopOnce  sync.Once
	stopCh    chan struct{}
	doneCh    chan struct{}

	written atomic.Uint64
	dropped atomic.Uint64

	// insert is the write seam, resolved once at construction. A test
	// substitutes one to assert on the rows a flush WOULD have written --
	// which is what lets the drop, batch and shutdown behaviour be proven
	// with no database at all.
	insert func(ctx context.Context, rows []Row) error

	// writeTimeout bounds one batch insert, and the row-by-row fallback after
	// a refused batch shares one more of it (writeRowByRow). A field,
	// defaulted by NewSink, so a test can shorten it.
	writeTimeout time.Duration
}

// defaultWriteTimeout is how long one insert may take before it is abandoned.
const defaultWriteTimeout = 5 * time.Second

// SinkOptions configures the writer. Zero values take the defaults, which are
// the Logs store's numbers (its design's L5) for the same reason it chose
// them: a queue deep enough to ride out a flush, a batch big enough that one
// insert is worth making, and a flush interval short enough that a figure is
// never more than a second behind the traffic it describes.
type SinkOptions struct {
	// BufferSize is how many records may be waiting for a flush. Default 4096.
	BufferSize int
	// FlushInterval is how often a partial batch is written. Default 1s.
	FlushInterval time.Duration
	// MaxBatch is the largest single insert. Default 256.
	MaxBatch int
	// Node is MEMQL_NODE_ID, stamped on every row. Resolved once by the
	// caller so the serving path never reads the environment.
	Node string
	// Logger is where a flush failure is reported. Defaults to slog.Default.
	Logger *slog.Logger
	// Insert replaces the bun insert, for tests. Nil means the real one.
	Insert func(ctx context.Context, rows []Row) error
}

func (o SinkOptions) defaults() SinkOptions {
	if o.BufferSize <= 0 {
		o.BufferSize = 4096
	}
	if o.FlushInterval <= 0 {
		o.FlushInterval = time.Second
	}
	if o.MaxBatch <= 0 {
		o.MaxBatch = 256
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return o
}

// Row mirrors the edge_request schema in
// migrations/20260903020000_edge_request_hypertable.up.sql, plus the visit
// columns of migrations/20261108090100_edge_request_visit_fields.up.sql.
// Exported so a test can supply its own Insert and assert on what would have
// been written; the bun tags stay inside this package.
type Row struct {
	bun.BaseModel `bun:"table:edge_request,alias:er"`

	ServedAt   time.Time `bun:"served_at,notnull"`
	Id         string    `bun:"id,notnull"`
	SiteId     string    `bun:"site_id,notnull"`
	Node       string    `bun:"node"`
	Status     int       `bun:"status,notnull"`
	PathClass  string    `bun:"path_class,notnull"`
	Bytes      int64     `bun:"bytes"`
	DurationNs int64     `bun:"duration_ns"`

	// The visit columns. Each has a constant default in the table, so an
	// insert that names none of them -- a previous release's edge during a
	// rolling upgrade -- still lands, and reads back unmeasured.
	Measured bool   `bun:"measured,notnull"`
	PageView bool   `bun:"page_view,notnull"`
	Bot      bool   `bun:"bot,notnull"`
	Path     string `bun:"path,notnull"`
	Source   string `bun:"source,notnull"`
	Country  string `bun:"country,notnull"`
	Device   string `bun:"device,notnull"`
	Browser  string `bun:"browser,notnull"`
	OS       string `bun:"os,notnull"`
	Visitor  string `bun:"visitor,notnull"`
}

// NewSink constructs the writer. It does not start the drain; call Start.
func NewSink(db *bun.DB, opts SinkOptions) *Sink {
	opts = opts.defaults()
	s := &Sink{
		logger:        opts.Logger.With("component", "sitetraffic.sink"),
		db:            db,
		node:          opts.Node,
		buffer:        make(chan Record, opts.BufferSize),
		flushInterval: opts.FlushInterval,
		maxBatch:      opts.MaxBatch,
		stopCh:        make(chan struct{}),
		doneCh:        make(chan struct{}),
		writeTimeout:  defaultWriteTimeout,
	}
	s.insert = opts.Insert
	if s.insert == nil {
		s.insert = s.insertBatch
	}
	return s
}

var _ Recorder = (*Sink)(nil)

// Record implements Recorder. NON-BLOCKING: a full buffer drops the record and
// advances the counter, so the serving path's cost is one channel send whether
// the database is healthy, slow or gone.
func (s *Sink) Record(r Record) {
	select {
	case s.buffer <- r:
	default:
		s.dropped.Add(1)
		metrics.SiteTrafficDropped(metrics.SiteTrafficDropQueueFull, 1)
	}
}

// Start begins the background drain. Safe to call more than once.
func (s *Sink) Start() {
	s.startOnce.Do(func() { go s.run() })
}

// Stop drains what is queued and exits. Blocks until the loop finishes or ctx
// is cancelled. Idempotent.
func (s *Sink) Stop(ctx context.Context) error {
	var err error
	s.stopOnce.Do(func() {
		close(s.stopCh)
		select {
		case <-s.doneCh:
		case <-ctx.Done():
			err = ctx.Err()
		}
	})
	return err
}

// Stats returns what the sink has written and dropped, for tests and for the
// boot log line.
func (s *Sink) Stats() (written, dropped uint64) {
	return s.written.Load(), s.dropped.Load()
}

func (s *Sink) run() {
	defer close(s.doneCh)
	ticker := time.NewTicker(s.flushInterval)
	defer ticker.Stop()

	batch := make([]Record, 0, s.maxBatch)

	drain := func() {
		for len(batch) < s.maxBatch {
			select {
			case r := <-s.buffer:
				batch = append(batch, r)
			default:
				return
			}
		}
	}

	flush := func() {
		if len(batch) == 0 {
			return
		}
		// write accounts for every row itself, as written or dropped: after
		// a refused batch the two are no longer all of one or all of the
		// other (writeRowByRow).
		s.write(batch)
		batch = batch[:0]
	}

	for {
		select {
		case r := <-s.buffer:
			batch = append(batch, r)
			drain()
			if len(batch) >= s.maxBatch {
				flush()
			}
		case <-ticker.C:
			drain()
			flush()
		case <-s.stopCh:
			// One last pass, then whatever arrived during it. A record
			// written after Stop simply hits the full-buffer branch.
			drain()
			flush()
			for {
				select {
				case r := <-s.buffer:
					batch = append(batch, r)
					if len(batch) >= s.maxBatch {
						flush()
					}
				default:
					flush()
					return
				}
			}
		}
	}
}

// write turns records into rows, inserts them as one batch and accounts for
// every row as written or dropped. A refused batch falls back to
// writeRowByRow; a batch that lands is one insert, as it always was.
//
// The row id is minted HERE rather than in Record, so the serving path pays
// for a channel send and nothing else. It only has to be unique within a
// (served_at, id) primary key, and two requests to one site can land in the
// same microsecond -- which is exactly the collision a (served_at, site_id)
// key would have swallowed silently.
func (s *Sink) write(batch []Record) {
	rows := make([]Row, 0, len(batch))
	for _, r := range batch {
		node := r.Node
		if node == "" {
			node = s.node
		}
		rows = append(rows, Row{
			ServedAt:   r.ServedAt.UTC(),
			Id:         newRowId(),
			SiteId:     r.SiteId,
			Node:       node,
			Status:     r.Status,
			PathClass:  r.PathClass,
			Bytes:      r.Bytes,
			DurationNs: r.DurationNs,
			Measured:   r.Measured,
			PageView:   r.PageView,
			Bot:        r.Bot,
			Path:       r.Path,
			Source:     r.Source,
			Country:    r.Country,
			Device:     r.Device,
			Browser:    r.Browser,
			OS:         r.OS,
			Visitor:    r.Visitor,
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.writeTimeout)
	err := s.insert(ctx, rows)
	cancel()
	if err == nil {
		s.countWritten(len(rows))
		return
	}
	s.writeRowByRow(rows, err)
}

// writeRowByRow retries a refused batch one row at a time, so that one bad
// row costs one row.
//
// A BATCH INSERT IS ALL OR NOTHING. One row the database refuses fails the
// statement, and before this every request in the batch was dropped with it:
// up to MaxBatch rows, other sites' among them, lost to a single bad value.
// Alone, each row lands or fails on its own merits. Only the rows refused
// again are dropped, each counted as write_failed, and the batch is reported
// in ONE warning naming how many rows were isolated, rather than a line per
// row.
//
// ONE WRITE TIMEOUT, SHARED BY THE WHOLE PASS. A batch also fails when the
// database is down or hung, and then every row fails alone too. A timeout per
// row would hold this drain for a batch's worth of them (256 times five
// seconds is over twenty minutes) while the queue overflowed behind it and a
// shutdown waited. Sharing one deadline bounds a failed batch at two write
// timeouts whatever its size. A row the deadline leaves unattempted is
// dropped and counted like a refused one: what a reader of the figure needs is
// how many requests are missing, not why each one is.
//
// A batch the database committed but whose reply was lost would retry into
// duplicate keys and be counted dropped although it is stored, so in that
// rare case the drop counter errs high, as it did before this fallback.
func (s *Sink) writeRowByRow(rows []Row, batchErr error) {
	ctx, cancel := context.WithTimeout(context.Background(), s.writeTimeout)
	defer cancel()
	written, isolated := 0, 0
	var rowErr error
	for i := range rows {
		err := ctx.Err()
		if err == nil {
			err = s.insert(ctx, rows[i:i+1])
		}
		if err != nil {
			isolated++
			if rowErr == nil {
				rowErr = err
			}
			s.dropped.Add(1)
			metrics.SiteTrafficDropped(metrics.SiteTrafficDropWriteFailed, 1)
			continue
		}
		written++
	}
	s.countWritten(written)
	s.logger.Warn("a request log batch was refused and its rows were retried one at a time; the isolated rows were dropped, and the traffic figure is low by that many",
		"rows", len(rows), "written", written, "isolated", isolated, "err", batchErr, "rowErr", rowErr)
}

// countWritten adds rows that landed to the written count and its metric.
func (s *Sink) countWritten(n int) {
	if n == 0 {
		return
	}
	s.written.Add(uint64(n))
	metrics.SiteTrafficWritten(n)
}

func (s *Sink) insertBatch(ctx context.Context, rows []Row) error {
	if s.db == nil {
		return fmt.Errorf("sitetraffic: no database handle")
	}
	if _, err := s.db.NewInsert().Model(&rows).Exec(ctx); err != nil {
		return fmt.Errorf("edge_request batch insert: %w", err)
	}
	return nil
}

// newRowId mints the per-row id. crypto/rand rather than a counter or a
// timestamp: the rows are written by every edge replica independently, so an
// id has to be unique across processes with nothing shared between them.
func newRowId() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Unreachable in practice; a time-based fallback keeps a batch
		// writable rather than failing the whole flush over an id.
		return fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
