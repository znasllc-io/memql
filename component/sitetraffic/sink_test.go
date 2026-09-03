package sitetraffic

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/edge"
	"github.com/znasllc-io/memql/component/metrics"
)

// sink_test.go -- the writer's two promises: it never makes a visitor wait,
// and it never lies about what it wrote.
//
// Both are provable with no database at all, through the insert seam.

// collector is a stand-in insert that records the rows a flush would write.
type collector struct {
	mu   sync.Mutex
	rows []Row
	done chan struct{}
	want int
	err  error
}

func newCollector(want int) *collector {
	return &collector{done: make(chan struct{}), want: want}
}

func (c *collector) insert(_ context.Context, rows []Row) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	c.rows = append(c.rows, rows...)
	if c.want > 0 && len(c.rows) >= c.want {
		select {
		case <-c.done:
		default:
			close(c.done)
		}
	}
	return nil
}

func (c *collector) snapshot() []Row {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Row(nil), c.rows...)
}

func aRecord(site string) Record {
	return Record{
		SiteId:     site,
		ServedAt:   time.Now().UTC(),
		Status:     200,
		PathClass:  PathClassAsset,
		Bytes:      1024,
		DurationNs: 1_000_000,
	}
}

// What went in comes out, with the node stamped and an id minted per row.
func TestSinkWritesWhatItWasGiven(t *testing.T) {
	c := newCollector(3)
	s := NewSink(nil, SinkOptions{Insert: c.insert, Node: "edge-7", FlushInterval: 10 * time.Millisecond})
	s.Start()
	defer func() { _ = s.Stop(context.Background()) }()

	for i := 0; i < 3; i++ {
		s.Record(aRecord(fmt.Sprintf("site-%d", i)))
	}
	select {
	case <-c.done:
	case <-time.After(2 * time.Second):
		t.Fatal("the sink did not flush")
	}

	rows := c.snapshot()
	if len(rows) != 3 {
		t.Fatalf("wrote %d rows, want 3", len(rows))
	}
	ids := map[string]bool{}
	for _, row := range rows {
		if row.Node != "edge-7" {
			t.Errorf("row node = %q, want the sink's own", row.Node)
		}
		if row.Id == "" {
			t.Error("a row has no id; the (served_at, id) key needs one per row")
		}
		if ids[row.Id] {
			t.Errorf("id %q was minted twice -- two requests in the same microsecond would collide", row.Id)
		}
		ids[row.Id] = true
		if row.Status != 200 || row.PathClass != PathClassAsset || row.Bytes != 1024 {
			t.Errorf("row = %+v, want the record's own values", row)
		}
	}
	if written, dropped := s.Stats(); written != 3 || dropped != 0 {
		t.Errorf("stats = (%d written, %d dropped), want (3, 0)", written, dropped)
	}
}

// A record's own Node wins over the sink's, so a caller that knows which
// replica served a request can say so.
func TestSinkKeepsARecordsOwnNode(t *testing.T) {
	c := newCollector(1)
	s := NewSink(nil, SinkOptions{Insert: c.insert, Node: "edge-default", FlushInterval: 10 * time.Millisecond})
	s.Start()
	defer func() { _ = s.Stop(context.Background()) }()

	r := aRecord("site-1")
	r.Node = "edge-explicit"
	s.Record(r)
	select {
	case <-c.done:
	case <-time.After(2 * time.Second):
		t.Fatal("no flush")
	}
	if got := c.snapshot()[0].Node; got != "edge-explicit" {
		t.Errorf("node = %q, want the record's own", got)
	}
}

// THE ONE THAT MATTERS: Record never blocks, whatever the database is doing.
//
// The sink is given an insert that never returns, a buffer of one, and more
// records than it can hold. Every call must return; the ones that do not fit
// are dropped and counted. A sink that blocked here would make every visitor
// of every site on this replica wait on Postgres.
func TestRecordNeverBlocksOnAStalledInsert(t *testing.T) {
	stalled := make(chan struct{})
	defer close(stalled)

	s := NewSink(nil, SinkOptions{
		BufferSize: 1,
		MaxBatch:   1,
		Insert: func(context.Context, []Row) error {
			<-stalled
			return nil
		},
		FlushInterval: time.Millisecond,
	})
	s.Start()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 1000; i++ {
			s.Record(aRecord("site-1"))
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Record blocked on a stalled insert -- the serving path must never wait on the database")
	}

	_, dropped := s.Stats()
	if dropped == 0 {
		t.Error("nothing was counted as dropped, so the overflow went unreported -- a low figure with no account of itself")
	}
	if metrics.SiteTrafficDroppedValue(metrics.SiteTrafficDropQueueFull) == 0 {
		t.Error("the queue_full counter did not move; a drop that is invisible outside the process cannot explain a low figure")
	}
}

// A REFUSED BATCH IS COUNTED AS DROPPED, not only logged. What a reader of the
// figure needs is how many requests are missing from it, and a batch the
// database refused is missing exactly as thoroughly as an overflowed one.
func TestAFailedFlushIsCountedAsDropped(t *testing.T) {
	before := metrics.SiteTrafficDroppedValue(metrics.SiteTrafficDropWriteFailed)
	s := NewSink(nil, SinkOptions{
		Insert:        func(context.Context, []Row) error { return errors.New("connection refused") },
		FlushInterval: 5 * time.Millisecond,
	})
	s.Start()
	s.Record(aRecord("site-1"))
	s.Record(aRecord("site-2"))

	deadline := time.After(2 * time.Second)
	for {
		if _, dropped := s.Stats(); dropped >= 2 {
			break
		}
		select {
		case <-deadline:
			_, dropped := s.Stats()
			t.Fatalf("dropped = %d after a failing flush, want 2", dropped)
		case <-time.After(5 * time.Millisecond):
		}
	}
	_ = s.Stop(context.Background())

	if written, _ := s.Stats(); written != 0 {
		t.Errorf("written = %d after every flush failed, want 0 -- a writer that counts a refused batch as written overstates the figure", written)
	}
	if after := metrics.SiteTrafficDroppedValue(metrics.SiteTrafficDropWriteFailed); after <= before {
		t.Error("the write_failed counter did not move")
	}
}

// Stop flushes what is queued rather than discarding it, so a rolling restart
// does not put a hole in the figure.
func TestStopFlushesWhatIsQueued(t *testing.T) {
	c := newCollector(0)
	s := NewSink(nil, SinkOptions{Insert: c.insert, FlushInterval: time.Hour})
	s.Start()
	for i := 0; i < 5; i++ {
		s.Record(aRecord("site-1"))
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := len(c.snapshot()); got != 5 {
		t.Errorf("wrote %d rows on shutdown, want 5 -- a flush interval of an hour must not mean an hour of records lost on restart", got)
	}
	// Idempotent: a second Stop is a no-op rather than a panic on a closed
	// channel.
	if err := s.Stop(context.Background()); err != nil {
		t.Errorf("a second Stop: %v", err)
	}
}

// A record filed after Stop is dropped rather than panicking on a closed
// channel: the handler and the lifecycle are on different goroutines, so a
// request in flight during shutdown is the ordinary case, not the odd one.
func TestRecordAfterStopIsSafe(t *testing.T) {
	c := newCollector(0)
	s := NewSink(nil, SinkOptions{Insert: c.insert, FlushInterval: time.Hour, BufferSize: 1})
	s.Start()
	_ = s.Stop(context.Background())
	for i := 0; i < 10; i++ {
		s.Record(aRecord("site-1")) // must not panic
	}
}

// ---------------------------------------------------------------------------
// The two spellings of the path classes
// ---------------------------------------------------------------------------

// component/edge declares its own path-class constants rather than importing
// this package, so the edge depends on nothing new. Two spellings of one
// closed set can disagree, and the disagreement would be invisible: rows
// would carry a class the reader never expects and no test would notice.
//
// This is the one place the two are compared. It imports component/edge in a
// TEST only, which is what keeps the production dependency running one way.
func TestPathClassesMatchTheEdgesOwnSpelling(t *testing.T) {
	if got, want := edge.PathClassesForTest(), PathClasses; len(got) != len(want) {
		t.Fatalf("the edge names %d path classes and this package names %d: %v vs %v", len(got), len(want), got, want)
	}
	for i, class := range PathClasses {
		if edge.PathClassesForTest()[i] != class {
			t.Errorf("path class %d: the edge says %q, this package says %q", i, edge.PathClassesForTest()[i], class)
		}
	}
}
