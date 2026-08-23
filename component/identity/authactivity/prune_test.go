package authactivity

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// The LOOP, tested without a database.
//
// What the SQL does is covered by prune_db_test.go against real Postgres.
// What is covered here is the control flow around it -- when the sweep stops,
// what it does with an error mid-run, and that a bounded batch size is really
// bounded -- because those are the properties that turn a retention job into
// either a no-op or a table-locking full scan, and neither shows up as a
// failing SQL statement.

type fakeDeleter struct {
	mu      sync.Mutex
	batches []int64 // per-call rows deleted
	calls   []int   // limit each call was given
	err     error
	errAt   int
}

func (f *fakeDeleter) deleteOlderThan(_ context.Context, _ time.Time, limit int) (int64, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	idx := len(f.calls)
	f.calls = append(f.calls, limit)
	if f.err != nil && idx == f.errAt {
		return 0, 0, f.err
	}
	if idx >= len(f.batches) {
		return 0, 0, nil
	}
	// One version per row: authActivity is append-only, so in production the
	// two counts are equal. prune_db_test.go covers the case where they are
	// not.
	return f.batches[idx], f.batches[idx], nil
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newTestPruner(d *fakeDeleter) *Pruner {
	return &Pruner{
		Retention: 30 * 24 * time.Hour,
		BatchSize: 500,
		Logger:    discard(),
		Now:       func() time.Time { return time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC) },
		deleter:   d,
	}
}

// A full batch means there is probably more; a SHORT one means there is not.
// Stopping only on zero would cost one extra round trip per sweep forever;
// stopping on the first short batch is what "bounded batches until drained"
// actually means.
func TestPruneOnceDrainsUntilABatchComesBackShort(t *testing.T) {
	d := &fakeDeleter{batches: []int64{500, 500, 137}}
	p := newTestPruner(d)

	n, err := p.PruneOnce(context.Background())
	if err != nil {
		t.Fatalf("PruneOnce: %v", err)
	}
	if n != 1137 {
		t.Errorf("deleted %d, want 1137", n)
	}
	if len(d.calls) != 3 {
		t.Fatalf("made %d delete call(s), want 3 (two full batches then a short one)", len(d.calls))
	}
	for i, limit := range d.calls {
		if limit != 500 {
			t.Errorf("call %d used limit %d, want the configured batch size 500 -- an unbounded "+
				"delete on the hypertable is the failure this batching exists to prevent", i, limit)
		}
	}
}

// Nothing to do is the steady state on a quiet cluster, and it must cost
// exactly one query.
func TestPruneOnceOnAnEmptyWindowMakesOneCall(t *testing.T) {
	d := &fakeDeleter{batches: []int64{0}}
	p := newTestPruner(d)
	n, err := p.PruneOnce(context.Background())
	if err != nil {
		t.Fatalf("PruneOnce: %v", err)
	}
	if n != 0 || len(d.calls) != 1 {
		t.Errorf("deleted %d over %d call(s), want 0 over 1", n, len(d.calls))
	}
}

// A mid-sweep error RETURNS what was already deleted alongside the error. The
// rows are gone either way, and a caller that logged only the error would
// under-report the counter by a whole batch on every hiccup.
func TestPruneOnceReportsPartialProgressWithItsError(t *testing.T) {
	boom := errors.New("connection reset")
	d := &fakeDeleter{batches: []int64{500, 500}, err: boom, errAt: 2}
	p := newTestPruner(d)

	n, err := p.PruneOnce(context.Background())
	if !errors.Is(err, boom) {
		t.Fatalf("PruneOnce err = %v, want %v", err, boom)
	}
	if n != 1000 {
		t.Errorf("deleted %d alongside the error, want the 1000 rows the first two batches removed", n)
	}
}

// A cancelled context stops the sweep rather than draining a large backlog
// through a shutting-down pod.
func TestPruneOnceStopsOnACancelledContext(t *testing.T) {
	d := &fakeDeleter{batches: []int64{500, 500, 500, 500}}
	p := newTestPruner(d)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := p.PruneOnce(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("PruneOnce err = %v, want context.Canceled", err)
	}
	if len(d.calls) != 0 {
		t.Errorf("made %d delete call(s) on a cancelled context, want 0", len(d.calls))
	}
}

// The cutoff is `now - retention`, and it is what the whole job means. A sign
// error here deletes everything EXCEPT the rows it was meant to remove.
func TestCutoffIsNowMinusRetention(t *testing.T) {
	d := &fakeDeleter{}
	p := newTestPruner(d)
	got := p.cutoff()
	want := time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("cutoff = %s, want %s (now minus 30 days)", got, want)
	}
	if !got.Before(p.Now()) {
		t.Error("the cutoff is not in the past, so the sweep would delete rows it just wrote")
	}
}

// A misconfigured Pruner must refuse rather than delete on a zero cutoff --
// which, with Retention 0, is "everything up to this instant".
func TestPruneOnceRefusesAZeroRetention(t *testing.T) {
	d := &fakeDeleter{batches: []int64{500}}
	p := newTestPruner(d)
	p.Retention = 0
	if _, err := p.PruneOnce(context.Background()); err == nil {
		t.Fatal("a zero retention was accepted; the cutoff would be `now` and the sweep would " +
			"delete the whole log including the row written a millisecond ago")
	}
	if len(d.calls) != 0 {
		t.Errorf("made %d delete call(s) with a zero retention, want 0", len(d.calls))
	}
}

// Defaults are applied where a caller leaves a field at its zero value, so a
// Pruner built with only a DB and a retention still batches.
func TestDefaultsAreApplied(t *testing.T) {
	p := &Pruner{Retention: 24 * time.Hour, deleter: &fakeDeleter{}}
	if got := p.batchSize(); got != DefaultBatchSize {
		t.Errorf("batchSize() = %d, want the %d default", got, DefaultBatchSize)
	}
	if got := p.interval(); got != DefaultInterval {
		t.Errorf("interval() = %d, want the %s default", got, DefaultInterval)
	}
	// A negative or absurd batch size must not become an unbounded delete.
	p.BatchSize = -5
	if got := p.batchSize(); got != DefaultBatchSize {
		t.Errorf("a negative BatchSize resolved to %d; it must fall back to the default rather "+
			"than reaching the LIMIT clause", got)
	}
}
