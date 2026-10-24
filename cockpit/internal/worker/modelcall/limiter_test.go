package modelcall

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"

	"github.com/znasllc-io/memql/cockpit/internal/worker/models"
)

// heldRuntime answers a chat call only when released, so a slot stays
// taken for as long as a test needs it to. release is safe to call more
// than once.
func heldRuntime(t *testing.T) (*httptest.Server, chan struct{}, func()) {
	t.Helper()
	arrived := make(chan struct{}, 16)
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		drain(r)
		arrived <- struct{}{}
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
		fmt.Fprintf(w, "%s\n", mustJSON(map[string]any{"done": true, "done_reason": "stop"}))
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(release)
	return srv, arrived, release
}

// startWithIdle is a chat start whose idle ceiling -- and so the longest a
// call waits for a slot -- is idle seconds.
func startWithIdle(requestID string, idle int64) *memqlv1.ModelCallStart {
	st := start(requestID, "m", KindChat)
	st.Limits = &memqlv1.ModelCallLimits{TimeoutSeconds: 30, IdleTimeoutSeconds: idle, KeepaliveSeconds: 1}
	return st
}

func (r *recorder) keepalives() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, d := range r.deltas {
		if d.GetKeepalive() {
			n++
		}
	}
	return n
}

// TestTheCeilingIsTheWholeMachine (memql-cockpit#432). A worker serving
// two clusters runs a Manager for each, and every one of those clusters
// was told the SAME ceiling. With a ceiling per Manager, the two together
// could run twice what the hardware said it could; one Limiter makes the
// claim true across both. The second cluster's call WAITS for the slot --
// the engine would fail a refused call, not reroute it -- saying it is
// alive meanwhile, and runs the moment the first one finishes.
func TestTheCeilingIsTheWholeMachine(t *testing.T) {
	srv, arrived, release := heldRuntime(t)
	inv := inventoryWith(ollamaModel(srv.URL, "m", models.Attributes{MaxConcurrent: 1}))
	shared := NewLimiter()
	homeA := NewManager(Options{Inventory: inv, Limiter: shared, Getenv: func(string) string { return "" }})
	homeB := NewManager(Options{Inventory: inv, Limiter: shared, Getenv: func(string) string { return "" }})

	first := newRecorder()
	homeA.Start(context.Background(), first, startWithIdle("a-1", 10))
	awaitRequest(t, arrived)

	second := newRecorder()
	homeB.Start(context.Background(), second, startWithIdle("b-1", 10))
	// It waits, and says so: keepalives at the call's cadence, and no
	// request at the runtime while the first holds the only slot.
	deadline := time.Now().Add(5 * time.Second)
	for second.keepalives() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("a call waiting for a slot must keep the cluster's idle clock alive")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case <-arrived:
		t.Fatal("the second cluster's call reached the runtime while the first held the machine's only slot")
	default:
	}
	if shared.InFlight() != 1 {
		t.Fatalf("InFlight = %d, want 1", shared.InFlight())
	}

	release()
	if end := first.wait(t); end.GetErrorCode() != "" {
		t.Fatalf("first call ended %q", end.GetErrorCode())
	}
	awaitRequest(t, arrived)
	if end := second.wait(t); end.GetErrorCode() != "" {
		t.Fatalf("the waiting call ended %q (%s), want it to run once the slot freed", end.GetErrorCode(), end.GetError())
	}
	// One numbering across the wait and the generation: the engine drops
	// a delta whose seq does not increase.
	second.mu.Lock()
	for i, d := range second.deltas {
		if d.GetSeq() != uint64(i) {
			t.Errorf("delta %d has seq %d; keepalives and content share one monotonic seq", i, d.GetSeq())
		}
	}
	second.mu.Unlock()
	deadline = time.Now().Add(5 * time.Second)
	for shared.InFlight() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the slots were never given back: InFlight = %d", shared.InFlight())
		}
		time.Sleep(time.Millisecond)
	}
}

// The wait ends at the call's own idle ceiling -- as long as a call queued
// in the runtime could have gone silent before the watchdog ended it --
// and only then is the call refused, naming the ceiling and the wait.
func TestAWaitForASlotEndsAtTheIdleCeiling(t *testing.T) {
	srv, arrived, _ := heldRuntime(t)
	inv := inventoryWith(ollamaModel(srv.URL, "m", models.Attributes{MaxConcurrent: 1}))
	m := managerFor(inv)
	m.Start(context.Background(), newRecorder(), startWithIdle("holder", 30))
	awaitRequest(t, arrived)

	waiter := newRecorder()
	began := time.Now()
	m.Start(context.Background(), waiter, startWithIdle("waiter", 1))
	end := waiter.wait(t)
	if end.GetErrorCode() != CodeConcurrencyExceeded {
		t.Fatalf("error_code = %q, want %q", end.GetErrorCode(), CodeConcurrencyExceeded)
	}
	if want := `model "m" stayed at its concurrency limit of 1 on this machine for 1s`; end.GetError() != want {
		t.Fatalf("error = %q, want %q", end.GetError(), want)
	}
	if waited := time.Since(began); waited < time.Second {
		t.Fatalf("refused after %s, want only after the 1s idle ceiling", waited)
	}
}

// A call cancelled while it waits ends cancelled, and takes no slot.
func TestACancelledWaitEndsCancelled(t *testing.T) {
	srv, arrived, release := heldRuntime(t)
	inv := inventoryWith(ollamaModel(srv.URL, "m", models.Attributes{MaxConcurrent: 1}))
	shared := NewLimiter()
	m := NewManager(Options{Inventory: inv, Limiter: shared, Getenv: func(string) string { return "" }})
	m.Start(context.Background(), newRecorder(), startWithIdle("holder", 30))
	awaitRequest(t, arrived)

	waiter := newRecorder()
	m.Start(context.Background(), waiter, startWithIdle("waiter", 30))
	time.Sleep(50 * time.Millisecond)
	m.Cancel(&memqlv1.ModelCallCancel{RequestId: "waiter", Reason: "the caller went away"})
	end := waiter.wait(t)
	if end.GetFinishReason() != FinishCancelled || end.GetErrorCode() != CodeCancelled {
		t.Fatalf("end = %q / %q, want %q / %q", end.GetFinishReason(), end.GetErrorCode(), FinishCancelled, CodeCancelled)
	}
	if shared.InFlight() != 1 {
		t.Fatalf("InFlight = %d, want 1: the cancelled waiter must not have taken a slot", shared.InFlight())
	}
	release()
}

// Separate Limiters are what the ceiling was before: each Manager on its
// own. Asserted so the difference above is visibly the shared Limiter.
func TestSeparateLimitersDoNotShareACeiling(t *testing.T) {
	srv, arrived, _ := heldRuntime(t)
	inv := inventoryWith(ollamaModel(srv.URL, "m", models.Attributes{MaxConcurrent: 1}))
	homeA := managerFor(inv)
	homeB := managerFor(inv)
	homeA.Start(context.Background(), newRecorder(), start("a-1", "m", KindChat))
	homeB.Start(context.Background(), newRecorder(), start("b-1", "m", KindChat))
	awaitRequest(t, arrived)
	awaitRequest(t, arrived)
}

// Two calls arriving together against a ceiling of one run ONE AT A TIME
// and both finish. The ceiling counts slots taken, not calls still being
// resolved: counted the other way, two arrivals could each see the other
// and wait on a slot neither held.
func TestTwoArrivalsAgainstACeilingOfOneRunOneAtATime(t *testing.T) {
	var mu sync.Mutex
	inside, most := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		drain(r)
		mu.Lock()
		inside++
		if inside > most {
			most = inside
		}
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		inside--
		mu.Unlock()
		fmt.Fprintf(w, "%s\n", mustJSON(map[string]any{"done": true, "done_reason": "stop"}))
	}))
	defer srv.Close()
	inv := inventoryWith(ollamaModel(srv.URL, "m", models.Attributes{MaxConcurrent: 1}))
	for i := 0; i < 10; i++ {
		m := managerFor(inv)
		one, two := newRecorder(), newRecorder()
		m.Start(context.Background(), one, startWithIdle(fmt.Sprintf("one-%d", i), 10))
		m.Start(context.Background(), two, startWithIdle(fmt.Sprintf("two-%d", i), 10))
		for _, r := range []*recorder{one, two} {
			if end := r.wait(t); end.GetErrorCode() != "" {
				t.Fatalf("round %d: a call ended %q (%s), want both to run", i, end.GetErrorCode(), end.GetError())
			}
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if most != 1 {
		t.Fatalf("the runtime saw %d calls at once, want at most the ceiling of 1", most)
	}
}

// A nil Limiter holds nothing.
func TestNilLimiterHoldsNothing(t *testing.T) {
	var l *Limiter
	if l.InFlight() != 0 {
		t.Fatal("a nil Limiter holds nothing")
	}
}

// failingDeltas is a stream that has gone: every delta fails, and the End
// is still recorded (as far as this side knows, it was attempted).
type failingDeltas struct{ *recorder }

func (f failingDeltas) SendModelCallDelta(string, uint64, string, bool) error {
	return fmt.Errorf("transport is closing")
}

// A call waiting for a slot ends -- taking no slot -- when its stream goes
// (the keepalive it sends while it waits fails) and when the stream loss
// stops every call (StopAll).
func TestAWaitEndsWhenItsStreamGoes(t *testing.T) {
	srv, arrived, release := heldRuntime(t)
	defer release()
	inv := inventoryWith(ollamaModel(srv.URL, "m", models.Attributes{MaxConcurrent: 1}))
	shared := NewLimiter()
	m := NewManager(Options{Inventory: inv, Limiter: shared, Getenv: func(string) string { return "" }})
	m.Start(context.Background(), newRecorder(), startWithIdle("holder", 30))
	awaitRequest(t, arrived)

	gone := failingDeltas{newRecorder()}
	m.Start(context.Background(), gone, startWithIdle("keepalive-fails", 30))
	end := gone.wait(t)
	if end.GetErrorCode() != CodeWorkerStopped || end.GetError() != "the stream to the cluster went while this call waited for a concurrency slot" {
		t.Fatalf("end = %q / %q", end.GetErrorCode(), end.GetError())
	}

	stopped := newRecorder()
	m.Start(context.Background(), stopped, startWithIdle("stopped", 30))
	time.Sleep(50 * time.Millisecond)
	m.StopAll("the worker's stream to the cluster was lost")
	end = stopped.wait(t)
	if end.GetErrorCode() != CodeWorkerStopped || end.GetError() != "the worker's stream to the cluster was lost" {
		t.Fatalf("StopAll end = %q / %q", end.GetErrorCode(), end.GetError())
	}
	if shared.InFlight() != 0 {
		// StopAll ended the holder too, which gives its slot back.
		deadline := time.Now().Add(5 * time.Second)
		for shared.InFlight() != 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if shared.InFlight() != 0 {
			t.Fatalf("InFlight = %d after StopAll, want 0: no waiter may leave a slot behind", shared.InFlight())
		}
	}
}
