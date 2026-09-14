package memql

import (
	"context"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
)

// THE CATALOG RELOAD COALESCER, WITHOUT A DATABASE (memql#5252).
//
// The reload is injected, so every property rbac_catalog_reload.go claims is
// asserted against what the coalescer did to a probe: how many reloads ran,
// whether two ever overlapped, and where each one STARTED in the sequence of
// change events -- the property that says an authorization change is never
// left unread.

// reloadProbe is a reload that records what the coalescer does to it.
type reloadProbe struct {
	seq *atomic.Int64 // the event sequence; each start records where it began

	mu        sync.Mutex
	calls     int
	active    int
	maxActive int
	starts    []int64

	gate    chan struct{} // when set, the FIRST call blocks until it is closed
	started chan struct{} // when set, receives once per call, at its start
	hold    time.Duration // how long each call takes
}

func (p *reloadProbe) reload(context.Context) {
	p.mu.Lock()
	p.calls++
	first := p.calls == 1
	p.active++
	if p.active > p.maxActive {
		p.maxActive = p.active
	}
	if p.seq != nil {
		p.starts = append(p.starts, p.seq.Load())
	}
	p.mu.Unlock()

	if p.started != nil {
		p.started <- struct{}{}
	}
	if first && p.gate != nil {
		<-p.gate
	}
	if p.hold > 0 {
		time.Sleep(p.hold)
	}

	p.mu.Lock()
	p.active--
	p.mu.Unlock()
}

func (p *reloadProbe) snapshot() (calls, maxActive int, starts []int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls, p.maxActive, append([]int64(nil), p.starts...)
}

// waitIdle waits for the coalescer's loop to exit, which it does only after
// its last reload has returned.
func waitIdle(t *testing.T, r *coalescedReloader) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		running := r.running
		r.mu.Unlock()
		if !running {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the reload loop never went idle")
}

func awaitStart(t *testing.T, p *reloadProbe) {
	t.Helper()
	select {
	case <-p.started:
	case <-time.After(10 * time.Second):
		t.Fatal("no reload started")
	}
}

func TestABurstDuringAReloadCostsExactlyOneMoreReload(t *testing.T) {
	p := &reloadProbe{gate: make(chan struct{}), started: make(chan struct{}, 8)}
	r := newCoalescedReloader(context.Background(), 0, p.reload)

	r.request()
	awaitStart(t, p) // the first reload is reading, and held there

	// 200 changes land while it reads. None may wait for it: request only
	// records the change.
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				r.request()
			}
		}()
	}
	wg.Wait()

	close(p.gate)
	waitIdle(t, r)
	if calls, _, _ := p.snapshot(); calls != 2 {
		t.Fatalf("a burst of 200 changes during one reload ran %d reloads in all, want exactly 2: "+
			"the one in flight and one more that reads what the burst wrote", calls)
	}
}

func TestAChangeDuringAReloadGetsAReloadThatStartsAfterIt(t *testing.T) {
	var seq atomic.Int64
	p := &reloadProbe{seq: &seq, gate: make(chan struct{}), started: make(chan struct{}, 8)}
	r := newCoalescedReloader(context.Background(), 0, p.reload)

	seq.Add(1)
	r.request()
	awaitStart(t, p)

	// This change arrives after the in-flight reload has already read: that
	// reload cannot have seen it, so another must START after it.
	change := seq.Add(1)
	r.request()

	close(p.gate)
	waitIdle(t, r)
	_, _, starts := p.snapshot()
	if len(starts) == 0 || starts[len(starts)-1] < change {
		t.Fatalf("reload starts %v: none began after change %d, which arrived mid-reload -- an authorization "+
			"change nobody reads", starts, change)
	}
}

// Many writers, reloads that take real time: never two reloads at once, and
// the last reload always starts after the last change.
func TestReloadsNeverOverlapAndTheLastChangeIsNeverSkipped(t *testing.T) {
	var seq atomic.Int64
	p := &reloadProbe{seq: &seq, hold: 200 * time.Microsecond}
	r := newCoalescedReloader(context.Background(), 0, p.reload)

	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(g)))
			for i := 0; i < 200; i++ {
				seq.Add(1)
				r.request()
				if rng.Intn(4) == 0 {
					time.Sleep(time.Duration(rng.Intn(300)) * time.Microsecond)
				}
			}
		}(g)
	}
	wg.Wait()
	lastChange := seq.Load()
	waitIdle(t, r)

	calls, maxActive, starts := p.snapshot()
	if maxActive != 1 {
		t.Fatalf("%d reloads ran at once; the coalescer must never run two", maxActive)
	}
	if starts[len(starts)-1] < lastChange {
		t.Fatalf("the last reload started at change %d, before the last change (%d): it was skipped",
			starts[len(starts)-1], lastChange)
	}
	t.Logf("%d changes, %d reloads", lastChange, calls)
}

func TestCancellingTheContextStopsFurtherReloads(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p := &reloadProbe{gate: make(chan struct{}), started: make(chan struct{}, 8)}
	r := newCoalescedReloader(ctx, 0, p.reload)

	r.request()
	awaitStart(t, p)
	r.request() // pending when the context ends
	cancel()
	close(p.gate)
	waitIdle(t, r)
	if calls, _, _ := p.snapshot(); calls != 1 {
		t.Fatalf("%d reloads ran; a reload started after the context was cancelled", calls)
	}

	r.request()
	waitIdle(t, r)
	if calls, _, _ := p.snapshot(); calls != 1 {
		t.Fatalf("%d reloads ran; a change after cancellation started one", calls)
	}

	// Cancelled during the settle wait: nothing reads at all.
	ctx2, cancel2 := context.WithCancel(context.Background())
	p2 := &reloadProbe{}
	r2 := newCoalescedReloader(ctx2, time.Hour, p2.reload)
	r2.request()
	cancel2()
	waitIdle(t, r2)
	if calls, _, _ := p2.snapshot(); calls != 0 {
		t.Fatalf("%d reloads ran after a cancellation during the settle wait", calls)
	}
}

func TestABurstInsideTheSettleWindowIsOneReload(t *testing.T) {
	p := &reloadProbe{}
	r := newCoalescedReloader(context.Background(), 100*time.Millisecond, p.reload)
	for i := 0; i < 500; i++ {
		r.request()
	}
	waitIdle(t, r)
	if calls, _, _ := p.snapshot(); calls != 1 {
		t.Fatalf("500 changes inside one settle window ran %d reloads, want 1", calls)
	}
}

// THE SETTLE IS NOT A DEBOUNCE. Under a stream of changes that never goes
// quiet, reloads keep starting while it flows; a debounce that restarted on
// every change would start none until the writers stopped.
func TestTheSettleDoesNotStarveUnderASustainedStream(t *testing.T) {
	var seq atomic.Int64
	p := &reloadProbe{seq: &seq}
	r := newCoalescedReloader(context.Background(), 20*time.Millisecond, p.reload)

	stop := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(stop) {
		seq.Add(1)
		r.request()
		time.Sleep(time.Millisecond)
	}
	streamEnd := seq.Load()
	waitIdle(t, r)

	_, _, starts := p.snapshot()
	if len(starts) < 3 || starts[0] >= streamEnd {
		t.Fatalf("reload starts %v over a 300 ms stream of %d changes: the reloads waited for the stream to end",
			starts, streamEnd)
	}
	if starts[len(starts)-1] < streamEnd {
		t.Fatalf("the last reload started at change %d, before the stream's last (%d)", starts[len(starts)-1], streamEnd)
	}
}

// Through the real bus and the same wiring StartCapabilityCatalog uses: event
// delivery returns while a reload is held mid-read -- the handler records the
// change and nothing more -- and a hundred events behind it cost one reload.
func TestCatalogEventsReachTheReloaderWithoutHoldingDelivery(t *testing.T) {
	bus := events.NewBus()
	defer bus.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := &reloadProbe{gate: make(chan struct{}), started: make(chan struct{}, 8)}
	r := newCoalescedReloader(ctx, 0, p.reload)
	subscribeCoalescedReloads(bus, rbacCatalogTopics, "memql:rbacCatalog", r)

	bus.PublishSync(events.Event{Topic: rbacCatalogTopics[0]})
	awaitStart(t, p)

	delivered := make(chan struct{})
	go func() {
		defer close(delivered)
		for i := 0; i < 100; i++ {
			bus.PublishSync(events.Event{Topic: rbacCatalogTopics[i%len(rbacCatalogTopics)]})
		}
	}()
	select {
	case <-delivered:
	case <-time.After(10 * time.Second):
		t.Fatal("event delivery is held behind a reload that is still reading")
	}

	close(p.gate)
	waitIdle(t, r)
	if calls, _, _ := p.snapshot(); calls != 2 {
		t.Fatalf("101 catalog events, 100 of them during one reload, ran %d reloads, want 2", calls)
	}
}
