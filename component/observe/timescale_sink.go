package observe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/uptrace/bun"
)

// TimescaleSink writes captured invocation records into the
// code_invocation hypertable backing the v1:observability:invocation
// concept. Writes are batched so the instrumentation hot path never
// blocks on Postgres; a single goroutine drains the buffer every
// FlushInterval (or whenever the buffer fills) and issues a single
// COPY-style multi-row insert via bun.
//
// The sink is intentionally lossy under sustained pressure: when the
// buffer is full and a new record arrives, the record is dropped and
// the drop counter is incremented. The alternative -- blocking the
// instrumented call -- would turn observability into a head-of-line
// latency hazard, which is exactly what we want to avoid.
//
// Lifecycle: NewTimescaleSink returns a sink + a Dependency the app
// bootstrap registers so Stop() flushes the remaining buffer on
// graceful shutdown.
type TimescaleSink struct {
	logger        *slog.Logger
	db            *bun.DB
	buffer        chan Record
	flushInterval time.Duration
	maxBatch      int

	// run-loop control
	startOnce sync.Once
	stopOnce  sync.Once
	stopCh    chan struct{}
	doneCh    chan struct{}

	// observability counters (read by tests, surfaced in metrics
	// later). Atomics aren't required because only the run loop
	// writes them; the public Stats reader takes the same lock.
	statsMu  sync.Mutex
	written  uint64
	dropped  uint64
	flushes  uint64
	errCount uint64
}

// TimescaleSinkOptions configures the buffered writer. Zero values
// fall back to conservative defaults that work well on a dev
// machine: 1024-record buffer, 1-second flush, 256-row batches.
type TimescaleSinkOptions struct {
	BufferSize    int
	FlushInterval time.Duration
	MaxBatch      int
	Logger        *slog.Logger
}

func (o TimescaleSinkOptions) defaults() TimescaleSinkOptions {
	if o.BufferSize <= 0 {
		o.BufferSize = 1024
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

// NewTimescaleSink constructs the sink. It does not start the
// background drainer; call Start (typically from app bootstrap) to
// begin draining. Stop blocks until the buffer has flushed.
func NewTimescaleSink(db *bun.DB, opts TimescaleSinkOptions) *TimescaleSink {
	opts = opts.defaults()
	return &TimescaleSink{
		logger:        opts.Logger.With("component", "observe.timescale_sink"),
		db:            db,
		buffer:        make(chan Record, opts.BufferSize),
		flushInterval: opts.FlushInterval,
		maxBatch:      opts.MaxBatch,
		stopCh:        make(chan struct{}),
		doneCh:        make(chan struct{}),
	}
}

// Write implements Sink. Non-blocking: when the buffer is full the
// record is dropped and the drop counter advances. The caller's
// hot path stays predictable.
func (s *TimescaleSink) Write(_ context.Context, r Record) {
	select {
	case s.buffer <- r:
	default:
		s.statsMu.Lock()
		s.dropped++
		s.statsMu.Unlock()
	}
}

// Start begins the background drain. Safe to call multiple times;
// only the first invocation has effect.
func (s *TimescaleSink) Start() {
	s.startOnce.Do(func() {
		go s.run()
	})
}

// Stop signals the drain loop to flush remaining records and exit.
// Blocks until the loop has finished or ctx is cancelled. Idempotent.
func (s *TimescaleSink) Stop(ctx context.Context) error {
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

// Stats returns a snapshot of the sink's counters. Useful for tests
// and operational dashboards; not exposed via concept yet.
func (s *TimescaleSink) Stats() (written, dropped, flushes, errs uint64) {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	return s.written, s.dropped, s.flushes, s.errCount
}

func (s *TimescaleSink) run() {
	defer close(s.doneCh)
	ticker := time.NewTicker(s.flushInterval)
	defer ticker.Stop()

	batch := make([]Record, 0, s.maxBatch)

	drainTo := func(target *[]Record, max int) {
		for len(*target) < max {
			select {
			case r := <-s.buffer:
				*target = append(*target, r)
			default:
				return
			}
		}
	}

	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := s.insertBatch(batch); err != nil {
			s.statsMu.Lock()
			s.errCount++
			s.statsMu.Unlock()
			s.logger.Warn("flush failed; dropping batch", "count", len(batch), "err", err)
		} else {
			s.statsMu.Lock()
			s.written += uint64(len(batch))
			s.flushes++
			s.statsMu.Unlock()
		}
		batch = batch[:0]
	}

	for {
		select {
		case r := <-s.buffer:
			batch = append(batch, r)
			drainTo(&batch, s.maxBatch)
			if len(batch) >= s.maxBatch {
				flush()
			}
		case <-ticker.C:
			drainTo(&batch, s.maxBatch)
			flush()
		case <-s.stopCh:
			// Drain whatever's queued before exit; the deadline on
			// the app shutdown context is the upper bound. We don't
			// reset the ticker because we want a single final pass.
			drainTo(&batch, s.maxBatch)
			flush()
			// Now drain anything that arrived between the drain
			// above and the read of stopCh -- one more best-effort
			// pull. After this we're done; producers calling Write
			// after Stop just hit the full-buffer branch and drop.
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

// codeInvocationRow mirrors the schema in
// migrations/20260515000000_observability_hypertable.up.sql. Lives
// inside the sink package so the bun struct tags don't leak into
// the public Record type.
type codeInvocationRow struct {
	bun.BaseModel `bun:"table:code_invocation,alias:ci"`

	OccurredAt    time.Time `bun:"occurred_at,notnull"`
	CodeReference string    `bun:"code_reference,notnull"`
	DurationNs    int64     `bun:"duration_ns,notnull"`
	Level         string    `bun:"level,notnull"`
	ErrorMessage  string    `bun:"error_message"`
	Args          string    `bun:"args,type:jsonb"`
	Result        string    `bun:"result,type:jsonb"`
	TraceID       string    `bun:"trace_id"`
	SpanID        string    `bun:"span_id"`
}

func (s *TimescaleSink) insertBatch(batch []Record) error {
	if s.db == nil {
		return errors.New("nil bun.DB")
	}
	rows := make([]codeInvocationRow, 0, len(batch))
	for _, r := range batch {
		argsJSON, err := marshalNullableJSON(r.Args)
		if err != nil {
			argsJSON = "null"
		}
		resultJSON, err := marshalNullableJSON(r.Result)
		if err != nil {
			resultJSON = "null"
		}
		rows = append(rows, codeInvocationRow{
			OccurredAt:    r.Ts.UTC(),
			CodeReference: r.FQN,
			DurationNs:    int64(r.Duration),
			Level:         r.Level.String(),
			ErrorMessage:  truncate(r.Error, 4096),
			Args:          argsJSON,
			Result:        resultJSON,
			TraceID:       r.TraceID,
			SpanID:        r.SpanID,
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.db.NewInsert().Model(&rows).Exec(ctx); err != nil {
		return fmt.Errorf("code_invocation batch insert: %w", err)
	}
	return nil
}

// marshalNullableJSON returns "null" when the input is nil, the
// raw JSON otherwise. We marshal at flush time rather than at
// capture time so the hot path never pays the json cost; the
// flush path runs on its own goroutine.
func marshalNullableJSON(v any) (string, error) {
	if v == nil {
		return "null", nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "null", err
	}
	return string(b), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
