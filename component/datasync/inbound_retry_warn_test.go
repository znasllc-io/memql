package datasync

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
)

// A WARNING PER SOURCE, NOT PER DELIVERY. While a connector cannot read its
// tenant list, every failed delivery of every tenant is undecidable on every
// pass, each minute: one line a row floods the log for as long as the outage
// lasts. The pass says it once for each source, with how many deliveries wait
// on it, and asks the connector once for each source too.

// recorded is a slog handler that keeps what it is handed.
type recorded struct {
	mu      sync.Mutex
	records []slog.Record
}

func (r *recorded) Enabled(context.Context, slog.Level) bool { return true }
func (r *recorded) WithAttrs([]slog.Attr) slog.Handler       { return r }
func (r *recorded) WithGroup(string) slog.Handler            { return r }
func (r *recorded) Handle(_ context.Context, rec slog.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, rec.Clone())
	return nil
}

// waiting is, by source, how many deliveries each warning of the pass said
// are waiting: one entry for each warning that names a source and a count.
func (r *recorded) waiting() map[string][]int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string][]int64{}
	for _, rec := range r.records {
		var source string
		var count int64 = -1
		rec.Attrs(func(a slog.Attr) bool {
			switch a.Key {
			case "source":
				source = a.Value.String()
			case "deliveries":
				count = a.Value.Int64()
			}
			return true
		})
		if rec.Level == slog.LevelWarn && source != "" {
			out[source] = append(out[source], count)
		}
	}
	return out
}

// askedConnector is a tenantConnector that counts how often it was asked
// whether a source is its own.
type askedConnector struct {
	*tenantConnector
	asked map[string]int
}

func (c *askedConnector) ClaimsInboundSource(ctx context.Context, name string) (bool, error) {
	c.mu.Lock()
	c.asked[name]++
	c.mu.Unlock()
	return c.tenantConnector.ClaimsInboundSource(ctx, name)
}

func TestAnUndecidableSourceIsWarnedOfOnceAPassWithItsCount(t *testing.T) {
	name := fmt.Sprintf("dbtest-warn-%d", time.Now().UnixNano())
	memqlsync.Declare(name)
	c := &askedConnector{tenantConnector: &tenantConnector{fakeConnector: &fakeConnector{name: name}, readFails: true}, asked: map[string]int{}}
	if err := memqlsync.Bind(c); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { memqlsync.UnbindForTest(name) })

	engine := newRetryEngine()
	store := NewStore(engine)
	log := &recorded{}
	d := NewDispatcher(store, NewApplier(store, newFakeWriter())).WithLogger(slog.New(log))
	d.now = func() time.Time { return testNow }
	first, second := name+"-store1", name+"-store2"
	var rows []map[string]any
	for i, source := range []string{first, first, second, first, second, name} {
		row := dueRow(fmt.Sprintf("req-%d", i), 1)
		row["source"] = source
		rows = append(rows, row)
	}
	engine.seed("query inboundRequestsDueForRetry", rows)

	for pass := 1; pass <= 2; pass++ {
		log.records = nil
		c.mu.Lock()
		c.asked = map[string]int{}
		c.mu.Unlock()
		if _, err := d.RetryDue(context.Background()); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		waiting := log.waiting()
		if got := waiting[first]; len(got) != 1 || got[0] != 3 {
			t.Fatalf("pass %d: the three deliveries of %s were warned of as %v, want one warning counting 3", pass, first, got)
		}
		if got := waiting[second]; len(got) != 1 || got[0] != 2 {
			t.Fatalf("pass %d: the two deliveries of %s were warned of as %v, want one warning counting 2", pass, second, got)
		}
		if len(waiting) != 2 {
			t.Fatalf("pass %d: warnings named %v, want only the two sources nobody could tell", pass, waiting)
		}
		c.mu.Lock()
		asked := map[string]int{first: c.asked[first], second: c.asked[second]}
		c.mu.Unlock()
		if asked[first] != 1 || asked[second] != 1 {
			t.Fatalf("pass %d: the connector was asked %v times, want once for each source", pass, asked)
		}
	}
	// None of them was written, claimed or dispatched: they wait.
	for _, call := range engine.callsContaining("mutation updateInboundRequestStatus(") {
		if id, _ := renderedArgs(call)["requestId"].(string); id != "req-5" {
			t.Fatalf("an undecidable delivery was written: %s", call)
		}
	}
}
