package sitetraffic

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

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

// ---------------------------------------------------------------------------
// The lifecycle
// ---------------------------------------------------------------------------

// A REQUEST SERVED WHILE THE COMPONENT IS STARTING is not a corner case on an
// edge: the site handler is mounted before the database component has
// finished starting, so the very first requests a replica answers arrive
// while Start is still resolving its handle. The component is therefore read
// from a serving goroutine and written from the bootstrap one at the same
// time.
//
// THE PROVIDER MUST HAND BACK A REAL HANDLE, and that is the whole reason
// this test is written the way it is. A first cut let the provider answer nil,
// which sends Start down its no-database branch -- it then writes the field
// at all, so the readers had nothing to race with and the test passed under
// -race against the plain pointer field it was written to catch. A test that
// passes for the wrong reason is worse than no test.
//
// The handle points at a port nothing is listening on: `sql.OpenDB` does not
// dial, so Start resolves it, publishes the sink, and finds out the database
// is unreachable only when the retention call and the first flush fail --
// both of which are warnings by design. What matters here is that the field
// is WRITTEN while eight goroutines read it.
func TestRecordIsSafeWhileTheComponentIsStarting(t *testing.T) {
	unreachable := bun.NewDB(
		sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN("postgres://nobody:nobody@127.0.0.1:1/none?sslmode=disable"))),
		pgdialect.New(),
	)
	t.Cleanup(func() { _ = unreachable.Close() })

	c := NewSinkComponent(slog.New(slog.NewTextHandler(io.Discard, nil)), func() *bun.DB { return unreachable }, 0)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					// Must never panic and never race, whatever Start is
					// doing to the field underneath.
					c.Record(aRecord("site-1"))
				}
			}
		}()
	}

	c.Start(context.Background())
	<-c.Ready()
	if !c.IsRunning() {
		t.Fatal("Start did not resolve a sink, so this test is racing nothing -- see the note above")
	}

	close(stop)
	wg.Wait()
	c.Stop(context.Background())
}

// A component whose provider never yields a handle records nothing and says
// so through its own state -- and files no drop, because nothing was
// measuring yet and a counter that climbed during every boot would put a
// permanent floor under the one series an operator alerts on.
func TestAComponentWithNoDatabaseRecordsNothingAndCountsNothing(t *testing.T) {
	before := metrics.SiteTrafficDroppedValue(metrics.SiteTrafficDropQueueFull)
	c := NewSinkComponent(slog.New(slog.NewTextHandler(io.Discard, nil)), func() *bun.DB { return nil }, 0)
	c.Start(context.Background())
	<-c.Ready()
	if c.IsRunning() {
		t.Error("a component with no database handle must not report itself running")
	}
	for i := 0; i < 20; i++ {
		c.Record(aRecord("site-1")) // must not panic
	}
	if after := metrics.SiteTrafficDroppedValue(metrics.SiteTrafficDropQueueFull); after != before {
		t.Errorf("the drop counter moved by %v during a boot that never started recording", after-before)
	}
	c.Stop(context.Background())
}

// The two knobs, read from the environment.
func TestRequestLogEnvKnobs(t *testing.T) {
	t.Run("recording is on by default", func(t *testing.T) {
		if !RequestLogEnabled() {
			t.Error("unset must mean ON -- a cluster where nobody opted in would answer unmeasured forever with nothing to say why")
		}
	})
	t.Run("an explicit false switches it off", func(t *testing.T) {
		t.Setenv("MEMQL_EDGE_REQUEST_LOG_ENABLED", "false")
		if RequestLogEnabled() {
			t.Error("false must switch it off")
		}
	})
	t.Run("an unparseable value stays on", func(t *testing.T) {
		// Failing CLOSED here would silently stop a cluster measuring
		// anything over a typo in an overlay, and the figure would read as
		// "nobody is visiting".
		t.Setenv("MEMQL_EDGE_REQUEST_LOG_ENABLED", "sometimes")
		if !RequestLogEnabled() {
			t.Error("an unparseable value must take the default")
		}
	})
	t.Run("retention is clamped, and a bad value takes the default", func(t *testing.T) {
		for value, want := range map[string]int{
			"":     DefaultRetentionDays,
			"lots": DefaultRetentionDays,
			"0":    1,
			"-5":   1,
			"7":    7,
			"9000": 365,
		} {
			if value == "" {
				os.Unsetenv("MEMQL_EDGE_REQUEST_LOG_RETENTION_DAYS")
			} else {
				t.Setenv("MEMQL_EDGE_REQUEST_LOG_RETENTION_DAYS", value)
			}
			if got := RetentionDays(); got != want {
				t.Errorf("MEMQL_EDGE_REQUEST_LOG_RETENTION_DAYS=%q gives %d days, want %d", value, got, want)
			}
		}
	})
}
