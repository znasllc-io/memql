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
//     many requests are missing from it.
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
}

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
// migrations/20260903020000_edge_request_hypertable.up.sql. Exported so a test
// can supply its own Insert and assert on what would have been written; the
// bun tags stay inside this package.
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
		if err := s.write(batch); err != nil {
			// COUNTED AS DROPPED, not only as an error. What somebody reading
			// the figure needs is how many requests are missing from it, and
			// a failed batch is missing exactly as thoroughly as an overflowed
			// one.
			s.dropped.Add(uint64(len(batch)))
			metrics.SiteTrafficDropped(metrics.SiteTrafficDropWriteFailed, len(batch))
			s.logger.Warn("the request log could not be written; the traffic figure will be low for this window",
				"count", len(batch), "err", err)
		} else {
			s.written.Add(uint64(len(batch)))
			metrics.SiteTrafficWritten(len(batch))
		}
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

// write turns records into rows and hands them to the insert seam.
//
// The row id is minted HERE rather than in Record, so the serving path pays
// for a channel send and nothing else. It only has to be unique within a
// (served_at, id) primary key, and two requests to one site can land in the
// same microsecond -- which is exactly the collision a (served_at, site_id)
// key would have swallowed silently.
func (s *Sink) write(batch []Record) error {
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
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.insert(ctx, rows)
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
