package procedure

import (
	"strings"
	"testing"
	"time"
)

// heartbeat_test.go -- a replay run is alive while its step is (review
// finding I3, epic memql#5408).
//
// The abandoned-run sweep judges a run by its heartbeat. A replay wrote one
// beat per step, before the dispatch -- so a single step that ran longer than
// the sweep's window (a build, a test suite, a long download on somebody's
// machine) left an in-flight replay looking abandoned. The heartbeat is
// renewed for as long as a step is in flight, at the interval the automation
// runtime uses, and stopped -- joined -- before the step's receipt is
// written, so no beat can land after the run is closed.

// heartbeats are the recorded heartbeat-only run writes, and the position of
// each in the engine's call log.
func heartbeats(w *replayWorld) (positions []int, closeAt int) {
	closeAt = -1
	for n, c := range w.eng.recorded() {
		if c.Name() != "updateWorkRun" {
			continue
		}
		switch {
		case strings.Contains(c.Query, "status:"):
			closeAt = n
		case strings.Contains(c.Query, "heartbeatAt:"):
			positions = append(positions, n)
		}
	}
	return positions, closeAt
}

func TestAReplayRunStaysAliveWhileAStepIsInFlight(t *testing.T) {
	w := newReplayWorld(t, "trusted")
	w.i.heartbeatEvery = 5 * time.Millisecond
	w.d.alter["step0"] = func(*DispatchResult) { time.Sleep(80 * time.Millisecond) }

	if out := w.serve(t, ReplayTrusted, map[string]any{"file": goalFile}); !out.Served {
		t.Fatalf("outcome = %+v", out)
	}
	beats, closeAt := heartbeats(w)
	// One beat per step's intent, and more while step 0 ran for 80ms.
	if len(beats) < 4 {
		t.Fatalf("%d heartbeats written; a step in flight for 80ms at a 5ms interval must renew it", len(beats))
	}
	if closeAt < 0 || beats[len(beats)-1] > closeAt {
		t.Fatalf("a heartbeat (call %d) landed after the run was closed (call %d): the pulse must be joined before the receipt",
			beats[len(beats)-1], closeAt)
	}
	after := len(w.eng.recorded())
	time.Sleep(30 * time.Millisecond)
	if n := len(w.eng.recorded()); n != after {
		t.Fatalf("%d calls reached the engine after the replay returned: the pulse outlived its step", n-after)
	}
}

// TestTheDefaultHeartbeatIsTheAutomationRuntimes: an integration nobody tuned
// renews at the automation runtime's own interval.
func TestTheDefaultHeartbeatIsTheAutomationRuntimes(t *testing.T) {
	if got := newTestIntegration(newFakeEngine()).heartbeatInterval(); got != 15*time.Second {
		t.Fatalf("heartbeat interval = %v, want the automation runtime's 15s", got)
	}
}
