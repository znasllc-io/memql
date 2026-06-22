package app

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/automations"
	automationSteps "github.com/znasllc-io/memql/component/automations/steps"
	"github.com/znasllc-io/memql/component/harness"
	"github.com/znasllc-io/memql/component/harness/actionreplay"
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
//     (#584) via engine.InvokeAIChatWithFilteredToolsOpts, threading the
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
		// Action-library replay step (#1758): a planner-emitted step whose
		// input is `{type:"action", ref:"id@version", args:{...}}` references a
		// reusable library action by id. Dispatch it through the merged
		// ActionExecutor (literal/parameterized replay + composite recursion)
		// with ZERO LLM tool calls, instead of running an LLM template. The
		// planner only emits these when MEMQL_ACTION_REPLAY_ENABLED is on
		// (the emission side gates), so on shared main this branch never
		// triggers -- nothing authors action-typed harness steps otherwise.
		if at, _ := step.Input["type"].(string); at == "action" {
			return a.dispatchActionStep(ctx, step)
		}

		template, _ := step.Input["template"].(string)
		if template == "" {
			// No LLM template configured for this step: pass the input
			// through as the result so the step completes deterministically.
			return map[string]any{"echo": step.Input}, 0, nil
		}

		// Action-library literal replay (#1736): if a prior identical-input
		// run was minted into an action, replay its captured calls token-free
		// and skip the LLM entirely. Gated OFF by default
		// (MEMQL_ACTION_REPLAY_ENABLED) so this is a no-op on shared main until
		// an operator opts in.
		replayEnabled := actionReplayEnabled()
		inputFP := ""
		actionExisted := false
		if replayEnabled {
			inputFP = actionreplay.Fingerprint(step.Input)
			// Exact-input replay (Phase 1, fingerprint-verified).
			if replayed, result, found := a.tryReplayAction(ctx, step, inputFP); replayed {
				return result, 0, nil // token-free: zero LLM tool calls
			} else if found {
				actionExisted = true
			}
			// Parameterized replay on VARYING input (Phase 3 #1738): match by
			// the input's structural template fingerprint and re-bind params.
			if replayed, result, found := a.tryParameterizedReplay(ctx, step); replayed {
				return result, 0, nil // token-free
			} else if found {
				actionExisted = true
			}
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
		out, err := a.engine.InvokeAIChatWithFilteredToolsOpts(ctx, template, data, nil, opts)
		// Action-library trace capture (#1735): persist the captured
		// capability sequence as a v1:actions:candidate. Best-effort and
		// non-fatal -- a trace-write error never fails the step, and a
		// failed step's partial trace is still recorded (useful provenance).
		a.recordActionCandidate(ctx, step, traceSink)
		if err != nil {
			return nil, sink.toolCalls, err
		}
		result := map[string]any{"text": out}
		// Mint a replayable action from this successful run, but only when no
		// action already existed for this input (avoids duplicate rows on a
		// stale-replay fall-through). #1736.
		if replayEnabled && !actionExisted {
			a.mintActionFromRun(ctx, step, inputFP, traceSink, result)
		}
		return result, sink.toolCalls, nil
	}
}

// dispatchActionStep replays a planner-emitted action-library step (#1758).
// The step input is `{type:"action", ref:"id@version", args:{...},
// surface?:"..."}`; we hand it to the merged ActionExecutor, which resolves
// the action row, re-binds its captured capability calls from `args`, and
// recurses into composite child refs -- all token-free (no LLM). The
// step's recorded result is the executor's `{replayed, ref, results}`
// payload, surfaced as the harness step output so downstream steps + recall
// see what ran. Returns 0 tool calls (replay makes no SI calls).
func (a *App) dispatchActionStep(ctx context.Context, step harness.StepView) (map[string]any, int, error) {
	ref, _ := step.Input["ref"].(string)
	if ref == "" {
		return nil, 0, fmt.Errorf("action step %q: missing ref", step.ID)
	}
	cfg := &automations.ActionStepConfig{Ref: ref}
	if m, ok := step.Input["args"].(map[string]any); ok {
		cfg.Args = m
	}
	if s, ok := step.Input["surface"].(string); ok {
		cfg.Surface = s
	}
	astep := &automations.Step{ID: step.ID, Type: automations.StepTypeAction, Action: cfg}
	sctx := &automations.StepContext{Engine: a.engine, Logger: a.Logger}

	res, err := (&automationSteps.ActionExecutor{}).Execute(ctx, astep, sctx)
	if err != nil {
		return nil, 0, fmt.Errorf("action step %q (ref %q): %w", step.ID, ref, err)
	}
	out := map[string]any{}
	if m, ok := res.Result.(map[string]any); ok {
		out = m
	} else if res.Result != nil {
		out["result"] = res.Result
	}
	return out, 0, nil
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
