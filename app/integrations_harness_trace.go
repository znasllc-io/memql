package app

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/znasllc-io/memql/component/harness"
	"github.com/znasllc-io/memql/component/harness/actiontrace"
)

// candidateTraceSink is the concrete memql.ToolTraceSink for action-library
// trace capture (#1735, epic #1734). It accumulates each tool call the inner
// loop executes, in call order; harnessInnerLoop reads the accumulated calls
// after the loop returns, runs the data-flow tracer, and persists the result
// as a v1:actions:candidate row.
//
// Accumulation is mutex-guarded because the loop executes a single
// iteration's tool calls concurrently; the loop emits them to the sink from
// the post-join in-order pass, but the lock keeps the sink correct even if a
// future change emits from the goroutines directly.
type candidateTraceSink struct {
	mu    sync.Mutex
	calls []actiontrace.CapturedCall
}

func newCandidateTraceSink() *candidateTraceSink { return &candidateTraceSink{} }

// RecordToolCall appends one executed tool call. Index is assigned
// monotonically so call order is preserved across loop iterations.
func (s *candidateTraceSink) RecordToolCall(capability string, args map[string]any, result any, isError bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, actiontrace.CapturedCall{
		Index:      len(s.calls),
		Capability: capability,
		Args:       args,
		Result:     result,
		IsError:    isError,
	})
}

// captured returns a copy of the accumulated calls.
func (s *candidateTraceSink) captured() []actiontrace.CapturedCall {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]actiontrace.CapturedCall, len(s.calls))
	copy(out, s.calls)
	return out
}

// recordActionCandidate runs the data-flow tracer over the captured calls and
// persists a v1:actions:candidate via recordActionCandidate. It is
// best-effort: a step with no capability calls records nothing, and any write
// error is logged but never propagated (the step's own result is unaffected).
//
// Phase 0 feeds the tracer the dispatched step's input as the value-provenance
// source. The plan input and upstream step results are threaded in later, when
// the planner (#587 / Phase 3) seeds richer step inputs and parameterized
// replay needs them; until then an arg with no match against the step input
// simply stays literal -- the documented safe fallback.
func (a *App) recordActionCandidate(ctx context.Context, step harness.StepView, sink *candidateTraceSink) {
	calls := sink.captured()
	if len(calls) == 0 {
		return
	}
	trace := actiontrace.Trace(calls, actiontrace.ProvenanceSources{
		StepInput: step.Input,
	})

	callsJSON, err := json.Marshal(trace.Calls)
	if err != nil {
		return
	}
	edges := trace.ResourceEdges
	if edges == nil {
		edges = []actiontrace.ResourceEdge{}
	}
	edgesJSON, err := json.Marshal(edges)
	if err != nil {
		return
	}

	q := fmt.Sprintf(
		`recordActionCandidate({planId:%q, stepId:%q, calls:%s, resourceEdges:%s, callCount:%d})`,
		step.PlanID, step.ID, string(callsJSON), string(edgesJSON), len(trace.Calls),
	)
	adapter := &CognitionEngineAdapter{Engine: a.engine}
	if _, err := adapter.Execute(ctx, q); err != nil && a.Logger != nil {
		a.Logger.Warn("record action candidate failed",
			"plan", step.PlanID, "step", step.ID, "error", err)
	}
}
