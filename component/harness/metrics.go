package harness

// metrics.go derives per-plan metrics from a reconstructed Trace (#589).
// Like the rest of the observability spine, metrics are a READ over the
// event stream -- nothing is logged separately, so the numbers always match
// what actually happened. Pure functions only (no DB / clock), so the
// scorers and the eval harness can compute metrics over hand-built traces
// in a unit test.

import "time"

// PlanMetrics is the per-plan rollup the eval harness scores and the
// cockpit surfaces. Every field is derived from the Trace.
type PlanMetrics struct {
	// PlanID is the plan these metrics describe.
	PlanID string
	// FinalStatus is the plan's terminal (or current) status.
	FinalStatus string
	// Success is true iff the plan reached status=done.
	Success bool
	// StepCount is the number of distinct steps the plan introduced.
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
	// event -- the plan's observed duration.
	WallClock time.Duration
	// ObservationCount is the total observations recorded for the plan.
	ObservationCount int
	// ErrorCount is the number of error-kind observations.
	ErrorCount int
}

// ComputeMetrics derives PlanMetrics from a Trace. It is the single source
// of the success / step-count / tool-call / token-cost / wall-clock numbers
// the eval scaffold reports, computed purely from the event stream.
func ComputeMetrics(t Trace) PlanMetrics {
	m := PlanMetrics{
		PlanID:      t.PlanID,
		FinalStatus: t.FinalStatus,
		Success:     t.FinalStatus == PlanStatusDone,
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
func intFromData(data map[string]any, key string) int {
	if data == nil {
		return 0
	}
	switch v := data[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	}
	return 0
}
