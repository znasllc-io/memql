package app

import (
	"context"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/harness"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/integrations"
)

// harnessReconcilerComponent wraps the event-sourced harness reconciler
// (component/harness, issue #583) as a lifecycle Dependency. It embeds the
// base Integration for the Start/Stop/Ready/Order/ComponentName plumbing
// (same pattern as the planner integration) and extends Start/Stop to
// drive the reconciler's event subscriptions + periodic tick.
type harnessReconcilerComponent struct {
	*integrations.Integration
	reconciler *harness.Reconciler
}

// Start subscribes the reconciler to the event bus + launches its tick,
// then delegates the rest of the lifecycle to the base Integration.
func (h *harnessReconcilerComponent) Start(ctx context.Context) {
	if h == nil {
		return
	}
	if h.reconciler != nil {
		h.reconciler.Start(ctx)
	}
	if h.Integration != nil {
		h.Integration.Start(ctx)
	}
}

// Stop tears down the reconciler subscriptions then delegates to the base.
func (h *harnessReconcilerComponent) Stop(ctx context.Context) {
	if h == nil {
		return
	}
	if h.reconciler != nil {
		h.reconciler.Stop()
	}
	if h.Integration != nil {
		h.Integration.Stop(ctx)
	}
}

// setupHarnessReconciler constructs the harness reconciler and appends it
// to the dependency set. It wires:
//   - a bun-backed StepReader (graph reads in Go, per the #583/#585 split);
//   - an Execute-backed Writer + atomic StepClaimer (#582 mutations);
//   - a Dispatcher that runs each step through the hardened inner loop
//     (#584) via engine.InvokeSIChatWithFilteredToolsOpts, threading the
//     step's idempotencyKey and a concrete observation sink (closing the
//     ToolLoopObservationSink TODO #584 left);
//   - the event-bus subscriber (graph.node.created for plan + observation).
//
// Called from the agent and planner node setup (both touch plan/step
// execution). Idempotent guards in the reconciler make a double-wire safe.
func (a *App) setupHarnessReconciler() {
	if a.engine == nil {
		a.Logger.Warn("harness reconciler: engine not ready; skipping wiring")
		return
	}

	dbProvider := func() *bun.DB {
		if a.db == nil {
			return nil
		}
		return a.db.BunDB()
	}

	reader := harness.NewBunStepReader(dbProvider)
	writer := harness.NewEngineWriter(&CognitionEngineAdapter{Engine: a.engine})
	dispatcher := harness.NewFuncDispatcher(a.harnessInnerLoop())

	var subscriber harness.Subscriber
	if a.eventBus != nil {
		subscriber = harness.NewBusSubscriber(a.eventBus)
	}

	reconciler, err := harness.New(
		reader,
		writer, // EngineWriter satisfies StepClaimer too (claim == startStep).
		dispatcher,
		writer,
		subscriber,
		a.Logger,
		harness.Config{},
	)
	if err != nil {
		a.fatal("failed to create harness reconciler", "error", err, "component", harness.ComponentName)
	}

	base, err := integrations.NewIntegration(common.ComponentName(harness.ComponentName))
	if err != nil {
		a.fatal("failed to create harness reconciler base", "error", err)
	}

	a.Dependencies = append(a.Dependencies, &harnessReconcilerComponent{
		Integration: base,
		reconciler:  reconciler,
	})
	a.Logger.Info("harness reconciler registered", "topics", harness.Subscriptions())
}

// harnessInnerLoop builds the inner-loop dispatch func bound to the live
// engine. Each step is run through the hardened tool loop (#584) with the
// step's idempotencyKey threaded and a concrete observation sink wired so
// loop notes/stops/errors land as v1:harness:observation rows.
//
// The dispatch uses the step input's `template` field to pick the prompt
// when present; otherwise it falls back to recording the step input as the
// result (a no-LLM passthrough). This keeps the reconciler functional
// before the planner (#587) seeds richer step inputs, and exercises the
// full claim -> dispatch -> observe -> complete path end to end.
func (a *App) harnessInnerLoop() harness.InnerLoop {
	return func(ctx context.Context, step harness.StepView) (map[string]any, int, error) {
		template, _ := step.Input["template"].(string)
		if template == "" {
			// No LLM template configured for this step: pass the input
			// through as the result so the step completes deterministically.
			return map[string]any{"echo": step.Input}, 0, nil
		}

		data := map[string]any{}
		for k, v := range step.Input {
			data[k] = v
		}
		sink := &harnessObservationSink{
			exec:   &CognitionEngineAdapter{Engine: a.engine},
			stepID: step.ID,
			planID: step.PlanID,
		}
		traceSink := newCandidateTraceSink()
		opts := &memql.ToolLoopOptions{
			IdempotencyKey: step.IdempotencyKey,
			Observations:   sink,
			TraceSink:      traceSink,
		}
		out, err := a.engine.InvokeSIChatWithFilteredToolsOpts(ctx, template, data, nil, opts)
		// Action-library trace capture (#1735): persist the captured
		// capability sequence as a v1:actions:candidate. Best-effort and
		// non-fatal -- a trace-write error never fails the step, and a
		// failed step's partial trace is still recorded (useful provenance).
		a.recordActionCandidate(ctx, step, traceSink)
		if err != nil {
			return nil, sink.toolCalls, err
		}
		return map[string]any{"text": out}, sink.toolCalls, nil
	}
}

// harnessObservationSink is the concrete ToolLoopObservationSink (#584
// left this as a TODO). It writes each loop observation (note / stop /
// error) as a v1:harness:observation row via the #582 mutation, so the
// inner loop's reasoning is durable and recall-able (#585).
type harnessObservationSink struct {
	exec      *CognitionEngineAdapter
	stepID    string
	planID    string
	toolCalls int
}

// RecordObservation persists a single loop observation. The harness
// observation kind enum is tool_result / error / note / decision; the
// loop emits "note" / "stop" / "error" -- map "stop" onto "note" so the
// row validates while preserving the reason in data.
func (s *harnessObservationSink) RecordObservation(ctx context.Context, kind, text string, meta map[string]any) {
	if s == nil || s.exec == nil {
		return
	}
	var obsKind string
	switch kind {
	case "error":
		obsKind = "error"
	case "decision":
		obsKind = "decision"
	default:
		obsKind = "note"
	}
	w := harness.NewEngineWriter(s.exec)
	_ = w.RecordObservation(ctx, harness.Observation{
		StepID:  s.stepID,
		PlanID:  s.planID,
		Kind:    obsKind,
		Content: text,
		Data:    meta,
	})
}
