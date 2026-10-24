package crash_test

// Integration-style tests that exercise the recovery pattern the way
// app.go uses it. We don't pull tcell into the test (a real Screen
// requires a TTY); we simulate the dispatch shape -- a wrapper that
// holds per-tab sticky state, invokes a Draw / HandleEvent function
// under Catch, and renders an inline placeholder when a report
// comes back. Same pattern the app uses.

import (
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/znasllc-io/memql/cockpit/internal/crash"
)

// tabSim mimics the app.tabCrashes sticky-state pattern. Tests check
// that the broken-tab state survives across multiple Draw calls
// (no re-panic per frame, no panic-log flood) AND that switching
// tabs clears the state (the user's "retry this pane" gesture).
type tabSim struct {
	mu      sync.Mutex
	crashes map[string]*crash.Report

	// drawCalls tracks how many times we actually invoked each tab's
	// Draw fn. If the per-tab sticky state works, a Draw that
	// panicked once should NOT be re-invoked on subsequent frames
	// for the same tab -- the count stays at 1.
	drawCalls map[string]int
}

func newTabSim() *tabSim {
	return &tabSim{
		crashes:   make(map[string]*crash.Report),
		drawCalls: make(map[string]int),
	}
}

// draw is the per-frame entry point. Mirrors the per-tab dispatch
// in app.go's draw().
func (s *tabSim) draw(tab string, drawFn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.crashes[tab] != nil {
		// Skip the broken tab's Draw -- show the placeholder instead.
		// In the real cockpit this is crash.DrawInline; here we don't
		// need to render anything, just refuse to re-invoke drawFn.
		return
	}
	s.drawCalls[tab]++
	if r := crash.Catch("draw:"+tab, drawFn); r != nil {
		s.crashes[tab] = r
	}
}

// switchTo simulates the F1/F2/F3 path -- clears the sticky crash
// state for the newly-active tab so the next Draw retries.
func (s *tabSim) switchTo(tab string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.crashes, tab)
}

// TestTabDispatch_PanicProducesStickyState: a Draw panic is caught,
// recorded per-tab, and the same Draw is NOT re-invoked on
// subsequent draws (so we don't spam crash logs every frame).
func TestTabDispatch_PanicProducesStickyState(t *testing.T) {
	tmpHome(t)

	s := newTabSim()
	for i := 0; i < 5; i++ {
		s.draw("Concepts", func() {
			panic("intentional")
		})
	}
	// Only one actual invocation should have happened; the subsequent
	// 4 frames should have hit the sticky placeholder path.
	if got := s.drawCalls["Concepts"]; got != 1 {
		t.Errorf("Concepts drawCalls = %d; want 1 (sticky state should suppress re-draws)", got)
	}
	if s.crashes["Concepts"] == nil {
		t.Error("expected sticky crash report for Concepts; got nil")
	}
}

// TestTabDispatch_OtherTabsUnaffected: a Concepts crash must NOT
// stop the Clusters tab from rendering normally. This is the WHOLE
// POINT of per-tab recovery.
func TestTabDispatch_OtherTabsUnaffected(t *testing.T) {
	tmpHome(t)

	s := newTabSim()
	s.draw("Concepts", func() { panic("boom") })
	for i := 0; i < 3; i++ {
		s.draw("Clusters", func() { /* fine */ })
	}
	if s.crashes["Clusters"] != nil {
		t.Errorf("Clusters got a crash report; expected nil. Got %+v", s.crashes["Clusters"])
	}
	if got := s.drawCalls["Clusters"]; got != 3 {
		t.Errorf("Clusters drawCalls = %d; want 3 (other tabs should render normally)", got)
	}
}

// TestTabDispatch_SwitchClearsCrash: switching away from a broken
// tab and back retries it.
func TestTabDispatch_SwitchClearsCrash(t *testing.T) {
	tmpHome(t)

	s := newTabSim()
	s.draw("Concepts", func() { panic("first") })
	s.switchTo("Concepts") // user navigates Tab -> Concepts again
	// Next Draw should re-attempt; if we make it succeed, no crash.
	s.draw("Concepts", func() { /* recovered */ })
	if s.crashes["Concepts"] != nil {
		t.Errorf("expected crash cleared after switchTo + successful Draw; got %+v", s.crashes["Concepts"])
	}
	if got := s.drawCalls["Concepts"]; got != 2 {
		t.Errorf("Concepts drawCalls = %d; want 2 (first panicked, second succeeded)", got)
	}
}

// TestMainLoop_PanicDoesNotKillLoop simulates the main-loop wrapper.
// Even if the dispatch body panics, the loop must keep running.
func TestMainLoop_PanicDoesNotKillLoop(t *testing.T) {
	tmpHome(t)

	iterations := 0
	var panicked, succeeded int
	for i := 0; i < 10; i++ {
		iterations++
		report := crash.Catch("main-loop", func() {
			if i%3 == 0 {
				panic(errors.New("scheduled crash"))
			}
		})
		if report != nil {
			panicked++
		} else {
			succeeded++
		}
	}
	if iterations != 10 {
		t.Errorf("loop iterations = %d; want 10 (no iteration should be skipped)", iterations)
	}
	if panicked != 4 {
		t.Errorf("panicked iterations = %d; want 4", panicked)
	}
	if succeeded != 6 {
		t.Errorf("succeeded iterations = %d; want 6", succeeded)
	}
}

// TestRecover_PanicInRecoverHandlerStillCaught: app.go wraps the
// post-crash-draw in its own Catch precisely because the recovery
// handler itself could panic. Verify that nested Catch handles it.
func TestRecover_PanicInRecoverHandlerStillCaught(t *testing.T) {
	tmpHome(t)

	outerReport := crash.Catch("outer", func() {
		if r := crash.Catch("inner", func() {
			panic("first")
		}); r != nil {
			// Simulate the post-crash handler itself panicking.
			panic("second, from the handler")
		}
	})
	if outerReport == nil {
		t.Fatal("outer Catch returned nil; expected to catch the second panic")
	}
	if outerReport.Code == "" {
		t.Error("outer report missing code")
	}
}

// TestParallelTabs_NoLockContentionDeadlock runs many tab dispatches
// concurrently. The simulator uses a mutex for the sticky map; the
// real app uses the event loop (single-threaded for tab dispatch),
// but this still proves Catch + the sticky pattern don't deadlock
// under load.
func TestParallelTabs_NoLockContentionDeadlock(t *testing.T) {
	tmpHome(t)

	s := newTabSim()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			tab := "Tab" + string(rune('A'+(idx%4)))
			s.draw(tab, func() {
				if idx%5 == 0 {
					panic("rolling crash")
				}
			})
		}(i)
	}
	wg.Wait()
}

// --- helpers ---------------------------------------------------------

// tmpHome redirects $HOME at a t.TempDir for the duration of the
// test so crash logs land in a sandboxed location that gets cleaned
// up after the test, not in the developer's real ~/.memql.
func tmpHome(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	saved, had := os.LookupEnv("HOME")
	t.Cleanup(func() {
		if had {
			_ = os.Setenv("HOME", saved)
		} else {
			_ = os.Unsetenv("HOME")
		}
	})
	if err := os.Setenv("HOME", dir); err != nil {
		t.Fatalf("set HOME: %v", err)
	}
}
