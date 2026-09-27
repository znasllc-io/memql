//go:build agent

package worker

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	workerservice "github.com/znasllc-io/memql/component/worker"
)

// TestACommandsTimeLimitCanOnlyLengthenItsDispatch (epic memql#5408): the
// worker ends an exec at its timeoutSec, so the dispatch waiting on it must
// wait at least that long -- a replayed step the recording let run ten
// minutes was abandoned at the default five. A limit inside the default
// leaves it (zero), nothing shortens it, and a huge one is bounded.
func TestACommandsTimeLimitCanOnlyLengthenItsDispatch(t *testing.T) {
	for _, tc := range []struct {
		name string
		args map[string]any
		want time.Duration
	}{
		{"no limit", map[string]any{"cmd": "ls"}, 0},
		{"a limit inside the default", map[string]any{"timeoutSec": float64(30)}, 0},
		{"ten minutes, from JSON", map[string]any{"timeoutSec": float64(600)}, 600*time.Second + dispatchAnswerMargin},
		{"ten minutes, from Go", map[string]any{"timeoutSec": 600}, 600*time.Second + dispatchAnswerMargin},
		{"ten minutes, as a json.Number", map[string]any{"timeoutSec": json.Number("600")}, 600*time.Second + dispatchAnswerMargin},
		{"a day is bounded", map[string]any{"timeoutSec": float64(86400)}, dispatchTimeoutCeiling},
		{"zero", map[string]any{"timeoutSec": float64(0)}, 0},
		{"negative", map[string]any{"timeoutSec": float64(-5)}, 0},
		{"NaN", map[string]any{"timeoutSec": math.NaN()}, 0},
		{"not a number", map[string]any{"timeoutSec": "600"}, 0},
		{"no args at all", nil, 0},
	} {
		if got := dispatchTimeoutFor(tc.args); got != tc.want {
			t.Errorf("%s: dispatchTimeoutFor = %v, want %v", tc.name, got, tc.want)
		}
	}
	if dispatchTimeoutCeiling <= workerservice.DispatchTimeoutDefault {
		t.Fatalf("the ceiling %v is not above the default %v, so no limit could lengthen a dispatch", dispatchTimeoutCeiling, workerservice.DispatchTimeoutDefault)
	}
}
