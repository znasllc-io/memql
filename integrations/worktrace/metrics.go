package worktrace

// metrics.go derives per-run metrics from a reconstructed Trace (#589,
// re-keyed onto the work journal by the work spine's epic A1). Like the
// rest of the observability spine, metrics are a READ over the event
// stream -- nothing is logged separately, so the numbers always match what
// actually happened. Pure functions only (no DB / clock), so they compute
// over hand-built traces in a unit test.

import (
	"time"

	"github.com/znasllc-io/memql/core/num"
)

// RunMetrics is the per-plan rollup the eval harness scores and the
// cockpit surfaces. Every field is derived from the Trace.
type RunMetrics struct {
	// RunID is the run these metrics describe.
	RunID string
	// FinalStatus is the run's terminal (or current) status.
	FinalStatus string
	// Success is true iff the run reached status=succeeded.
	Success bool
	// StepCount is the number of distinct steps the run introduced.
	StepCount int
	// StepsCompleted is how many steps reached done.
	StepsCompleted int
	// StepsFailed is how many steps reached failed.
	StepsFailed int
	// ToolCalls is the cumulative tool-call count summed from observation
	// data (tool_result observations and any explicit toolCalls counters).
	ToolCalls int
	// TokenCost is the cumulative token cost summed from observation data
	// (any "tokens" / "tokenCost" counter carried on a tool_result).
	TokenCost int
	// WallClock is the elapsed time from the first to the last timeline
	// event -- the run's observed duration.
	WallClock time.Duration
	// ObservationCount is the total observations recorded for the run.
	ObservationCount int
	// ErrorCount is the number of error-kind observations.
	ErrorCount int
}

// ComputeMetrics derives RunMetrics from a Trace. It is the single source
// of the success / step-count / tool-call / token-cost / wall-clock numbers
// the eval scaffold reports, computed purely from the event stream.
func ComputeMetrics(t Trace) RunMetrics {
	m := RunMetrics{
		RunID:       t.RunID,
		FinalStatus: t.FinalStatus,
		Success:     t.FinalStatus == RunStatusSucceeded,
		StepCount:   len(t.Steps),
	}

	for _, st := range t.Steps {
		switch st.FinalStatus {
		case StepStatusDone:
			m.StepsCompleted++
		case StepStatusFailed:
			m.StepsFailed++
		}
	}

	var first, last time.Time
	for i, ev := range t.Events {
		if i == 0 || ev.At.Before(first) {
			first = ev.At
		}
		if i == 0 || ev.At.After(last) {
			last = ev.At
		}
		if ev.Kind == EventKindObservation {
			m.ObservationCount++
			if ev.ObservationKind == "error" {
				m.ErrorCount++
			}
			m.ToolCalls += toolCallsFromData(ev)
			m.TokenCost += intFromData(ev.Data, "tokens") + intFromData(ev.Data, "tokenCost")
		}
	}
	if !first.IsZero() && last.After(first) {
		m.WallClock = last.Sub(first)
	}
	return m
}

// toolCallsFromData counts tool calls contributed by one observation. A
// tool_result observation is itself one tool call; an explicit toolCalls
// counter on the data (recorded by the reconciler) wins when present.
func toolCallsFromData(ev TraceEvent) int {
	if n := intFromData(ev.Data, "toolCalls"); n > 0 {
		return n
	}
	if ev.ObservationKind == "tool_result" {
		return 1
	}
	return 0
}

// intFromData reads an integer-ish field from observation data, tolerating
// the float64 JSON-decode shape.
// intFromData reads a harness event's numeric field.
//
// SATURATES out of range (memql#4779), because `toolCalls` is read through a
// `> 0` guard and a wrapped negative answers it wrong.
//
// HONEST LIMIT: `tokens` and `tokenCost` are ACCUMULATED (`m.TokenCost += ...`),
// so a saturated MaxInt added twice still overflows the running total. Reaching
// that needs two events each carrying a number above ~9.2e18; the narrowing is
// what this issue is about, and the accumulator's own bound is a separate
// question nobody has had to answer yet.
func intFromData(data map[string]any, key string) int {
	if data == nil {
		return 0
	}
	switch v := data[key].(type) {
	case float64:
		return num.ClampFloat64(v)
	case int:
		return v
	case int64:
		return num.ClampInt64(v)
	}
	return 0
}
