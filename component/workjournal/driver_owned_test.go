package workjournal

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// driver_owned_test.go -- a journal run is its DRIVER's: the work dispatcher
// must not adopt it, and the work sweep must see it beating for as long as the
// pass runs. integrations/work's driver_owned_test.go pins the dispatcher's
// half against the call this package actually renders.

// lockedEngine is countingEngine made safe for the heartbeat goroutine.
type lockedEngine struct {
	mu    sync.Mutex
	calls []string
}

func (e *lockedEngine) Execute(_ context.Context, query string) (any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, query)
	return nil, nil
}

func (e *lockedEngine) snapshot() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.calls...)
}

func heartbeatsIn(calls []string) int {
	n := 0
	for _, c := range calls {
		if strings.HasPrefix(c, "mutation updateWorkRun(") && strings.Contains(c, "heartbeatAt:") {
			n++
		}
	}
	return n
}

func TestAJournalRunIsDriverOwned(t *testing.T) {
	engine := &countingEngine{}
	if _, err := New(engine, nil, "node-1").Begin(context.Background(), work()); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	run := runCallOf(t, engine.calls)
	if !strings.Contains(run, `triggeredBy: "journal:libraryAnalyzeFile"`) {
		t.Fatalf("createWorkRun does not mark the run as its driver's -- the dispatcher adopts it and "+
			"fails it automation_not_runnable:\n%s", run)
	}
}

// TestAnOpenRunHeartbeatsUntilItCloses. Nothing else beats for a run no
// executor runs, and the work sweep abandons a running run whose heartbeat is
// a minute old -- which, for a Library pass embedding a large file or a
// delegated app session, is while it is still running.
func TestAnOpenRunHeartbeatsUntilItCloses(t *testing.T) {
	engine := &lockedEngine{}
	j := New(engine, nil, "node-1")
	j.beat = 5 * time.Millisecond

	run, err := j.Begin(context.Background(), work())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if got := heartbeatsIn(engine.snapshot()); got < 1 {
		t.Fatal("Begin wrote no first heartbeat; a driver that dies before the first tick leaves nothing to judge by")
	}
	time.Sleep(60 * time.Millisecond)
	if got := heartbeatsIn(engine.snapshot()); got < 3 {
		t.Fatalf("an open run beat %d time(s) in twelve intervals; it must keep beating while the pass runs", got)
	}

	run.Succeeded(context.Background(), nil)
	after := heartbeatsIn(engine.snapshot())
	time.Sleep(30 * time.Millisecond)
	if got := heartbeatsIn(engine.snapshot()); got != after {
		t.Fatalf("the run beat %d more time(s) after it closed; a closed run must go quiet", got-after)
	}
	// Closing twice is a caller's right -- a pass can fail and then close on
	// its deferred path -- and must not panic on the stopped heartbeat.
	run.Failed(context.Background(), "x", "y")
}

// TestAHeartbeatStopsWithTheCallersContext: the pass runs on that context, so
// a pass whose context is gone is not being worked any more.
func TestAHeartbeatStopsWithTheCallersContext(t *testing.T) {
	engine := &lockedEngine{}
	j := New(engine, nil, "node-1")
	j.beat = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := j.Begin(ctx, work()); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	cancel()
	time.Sleep(10 * time.Millisecond)
	settled := heartbeatsIn(engine.snapshot())
	time.Sleep(40 * time.Millisecond)
	if got := heartbeatsIn(engine.snapshot()); got != settled {
		t.Fatalf("the run kept beating after its context ended (%d more)", got-settled)
	}
}

// TestASucceededCloseClearsTheErrorFields: the read-merge keeps whatever an
// earlier write named, so a run that succeeded after something else wrote an
// errorCode over it kept the code. A FAILED close still names its own.
func TestASucceededCloseClearsTheErrorFields(t *testing.T) {
	engine := &countingEngine{}
	j := New(engine, nil, "node-1")
	run, _ := j.Begin(context.Background(), work())
	run.Succeeded(context.Background(), map[string]any{"chunks": 3})
	closeCall := engine.calls[len(engine.calls)-2]
	if !strings.Contains(closeCall, `status: "succeeded"`) {
		t.Fatalf("the call before the goal close is not the run close: %s", closeCall)
	}
	for _, want := range []string{`errorCode: ""`, `errorMessage: ""`} {
		if !strings.Contains(closeCall, want) {
			t.Errorf("a succeeded close does not clear %s:\n%s", want, closeCall)
		}
	}

	failing := &countingEngine{}
	failed, _ := New(failing, nil, "node-1").Begin(context.Background(), work())
	failed.Failed(context.Background(), "analysis_failed", "no text")
	closeCall = failing.calls[len(failing.calls)-2]
	if !strings.Contains(closeCall, `errorCode: "analysis_failed"`) {
		t.Errorf("a failed close lost its own code:\n%s", closeCall)
	}
}
