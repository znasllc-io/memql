package planner

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// maxConcurrencyDispatch returns a dispatch fn that tracks the peak number
// of concurrent in-flight calls, plus a pointer to read it after the run.
func maxConcurrencyDispatch(work time.Duration) (func(context.Context, string) error, *int32, *int32) {
	var inFlight, peak int32
	fn := func(_ context.Context, _ string) error {
		cur := atomic.AddInt32(&inFlight, 1)
		for {
			p := atomic.LoadInt32(&peak)
			if cur <= p || atomic.CompareAndSwapInt32(&peak, p, cur) {
				break
			}
		}
		time.Sleep(work)
		atomic.AddInt32(&inFlight, -1)
		return nil
	}
	return fn, &peak, &inFlight
}

func TestRunPhasedFanOut_IndependentTasksRunConcurrently(t *testing.T) {
	dispatch, peak, _ := maxConcurrencyDispatch(30 * time.Millisecond)
	phases := [][]string{{"a", "b", "c", "d"}} // one phase, all independent
	gate := NewSemaphoreGate(4)

	res := runPhasedFanOut(context.Background(), phases, gate, dispatch)
	if !res.AllSucceeded() {
		t.Fatalf("expected all succeeded, got failed=%v skipped=%v", res.Failed, res.Skipped)
	}
	if len(res.Succeeded) != 4 {
		t.Fatalf("expected 4 succeeded, got %d", len(res.Succeeded))
	}
	// With a pool of 4 and a work window, independent tasks must actually
	// overlap (this is the whole point of #899). >=2 proves concurrency
	// without being flaky on a busy scheduler.
	if *peak < 2 {
		t.Fatalf("expected concurrent execution (peak >= 2), got peak=%d", *peak)
	}
}

func TestRunPhasedFanOut_BoundedByGate(t *testing.T) {
	dispatch, peak, _ := maxConcurrencyDispatch(15 * time.Millisecond)
	phases := [][]string{{"a", "b", "c", "d", "e"}}
	gate := NewSemaphoreGate(1) // serialize

	res := runPhasedFanOut(context.Background(), phases, gate, dispatch)
	if !res.AllSucceeded() || len(res.Succeeded) != 5 {
		t.Fatalf("expected all 5 succeeded, got succeeded=%d failed=%v", len(res.Succeeded), res.Failed)
	}
	if *peak != 1 {
		t.Fatalf("pool size 1 must serialize: peak=%d, want 1", *peak)
	}
}

func TestRunPhasedFanOut_PhasesAreSequential(t *testing.T) {
	var order []string
	var mu sync.Mutex
	dispatch := func(_ context.Context, id string) error {
		mu.Lock()
		order = append(order, id)
		mu.Unlock()
		return nil
	}
	phases := [][]string{{"p1"}, {"p2"}, {"p3"}}
	res := runPhasedFanOut(context.Background(), phases, NewSemaphoreGate(4), dispatch)
	if !res.AllSucceeded() {
		t.Fatalf("expected all succeeded, got %+v", res)
	}
	if !reflect.DeepEqual(order, []string{"p1", "p2", "p3"}) {
		t.Fatalf("phases must run in order, got %v", order)
	}
}

func TestRunPhasedFanOut_PhaseFailureSkipsDownstream(t *testing.T) {
	dispatch := func(_ context.Context, id string) error {
		if id == "b" {
			return errors.New("boom")
		}
		return nil
	}
	// Phase 1 has an independent sibling that succeeds (a) and one that
	// fails (b); phase 2 (c) must be skipped because phase 1 failed.
	phases := [][]string{{"a", "b"}, {"c"}}
	res := runPhasedFanOut(context.Background(), phases, NewSemaphoreGate(4), dispatch)

	if _, ok := res.Succeeded["a"]; !ok {
		t.Fatal("independent sibling 'a' should still have succeeded")
	}
	if _, ok := res.Failed["b"]; !ok {
		t.Fatal("'b' should be recorded as failed")
	}
	if len(res.Skipped) != 1 || res.Skipped[0] != "c" {
		t.Fatalf("downstream phase task 'c' should be skipped, got skipped=%v", res.Skipped)
	}
	if res.AllSucceeded() {
		t.Fatal("AllSucceeded must be false when a phase failed")
	}
}

func TestRunPhasedFanOut_ContextCancelSkipsRemaining(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	dispatch := func(_ context.Context, id string) error {
		if id == "a" {
			cancel() // cancel partway through phase 1
		}
		return nil
	}
	phases := [][]string{{"a"}, {"b"}, {"c"}}
	res := runPhasedFanOut(ctx, phases, NewSemaphoreGate(1), dispatch)
	// b and c are downstream of the cancellation -> skipped.
	if len(res.Skipped) == 0 {
		t.Fatal("expected downstream tasks skipped after cancellation")
	}
}

func TestGroupTasksIntoPhases(t *testing.T) {
	phaseOrder := []string{"gather", "analyze"}
	tasks := []fanOutTaskRow{
		{ID: "t-analyze-2", Phase: "analyze", Seq: 2, Category: "semantic"},
		{ID: "t-gather-1", Phase: "gather", Seq: 1, Category: "semantic"},
		{ID: "t-gather-2", Phase: "gather", Seq: 2, Category: "semantic"},
		{ID: "t-analyze-1", Phase: "analyze", Seq: 1, Category: "semantic"},
		{ID: "t-toolcall", Phase: "gather", Seq: 0, Category: "toolInvocation"}, // filtered out
		{ID: "t-orphan", Phase: "unknownPhase", Seq: 0, Category: "semantic"},   // trailing
	}
	got := groupTasksIntoPhases(phaseOrder, tasks)
	want := [][]string{
		{"t-gather-1", "t-gather-2"}, // gather wave, seq-ordered
		{"t-analyze-1", "t-analyze-2"},
		{"t-orphan"}, // trailing wave for the unknown-phase task
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("groupTasksIntoPhases =\n  %v\nwant\n  %v", got, want)
	}
}

func TestNewSemaphoreGate_ClampsAndBlocks(t *testing.T) {
	g := NewSemaphoreGate(0) // clamps to 1
	if err := g.Acquire(context.Background()); err != nil {
		t.Fatalf("first acquire should succeed: %v", err)
	}
	// Second acquire must block; a cancelled ctx makes it return promptly.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := g.Acquire(ctx); err == nil {
		t.Fatal("second acquire on a full size-1 gate with a cancelled ctx must return ctx error")
	}
	g.Release()
	if err := g.Acquire(context.Background()); err != nil {
		t.Fatalf("acquire after release should succeed: %v", err)
	}
}
