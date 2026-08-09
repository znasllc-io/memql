package automations

import "context"

// run_observer.go carries the step-trace seam (memql#3310).
//
// The VS Code runtime panel needs a per-step trace of a run it requested:
// step name, status, duration, output or error, streamed as the run
// progresses. The requirement that shaped this file is that the trace must be
// an OBSERVER OF THE EXISTING EXECUTION rather than a second execution path.
// A run that took a different code path from a real trigger would prove
// nothing about the automation -- which is the entire reason the invoke path
// exists.
//
// So the observer rides the CONTEXT rather than the Executor. The scheduler
// owns exactly two long-lived Executors (event + schedule) that every trigger
// shares; hanging a callback off either would make the trace global and
// racy across concurrent runs. A context value is scoped to the one
// Execute call the caller made, is nil for every other run through the same
// Executor, and costs a single map lookup per step on the untraced path.

type stepObserverKey struct{}

// StepObserver is called once per step as the step's result is recorded,
// with the executor's own *StepResult -- the same value that lands in
// AutomationExecution.Steps and feeds the next step's condition evaluation.
//
// It is called SYNCHRONOUSLY on the executing goroutine, in step order, and
// must therefore not block: a slow observer slows the automation. The run
// relay's observer does a non-blocking send onto a buffered channel and drops
// on overflow, which is the right shape for anything wired here.
//
// The *StepResult must not be mutated. It is live: later steps read it back
// through the evaluator as $steps.<id>.
type StepObserver func(*StepResult)

// ContextWithStepObserver returns a context that carries obs, so every step
// recorded by an Execute* call descending from it is reported. A nil obs
// returns ctx unchanged.
func ContextWithStepObserver(ctx context.Context, obs StepObserver) context.Context {
	if obs == nil {
		return ctx
	}
	return context.WithValue(ctx, stepObserverKey{}, obs)
}

// stepObserverFrom returns the observer carried by ctx, or nil.
func stepObserverFrom(ctx context.Context) StepObserver {
	if ctx == nil {
		return nil
	}
	obs, _ := ctx.Value(stepObserverKey{}).(StepObserver)
	return obs
}

// NotifyStepObserver reports one step result to the context's observer, if
// any.
//
// Exported because the observer is part of the LocalAutomationRunner contract
// (run_relay.go): anything standing in for the scheduler -- another dispatch
// implementation, or a test double in another package -- has to be able to
// drive the trace, and a contract only the executor can satisfy is not a
// contract. The executor's own call sites go through the unexported alias
// below.
func NotifyStepObserver(ctx context.Context, result *StepResult) {
	notifyStepObserver(ctx, result)
}

// notifyStepObserver reports one step result to the context's observer, if
// any. Deliberately tolerant: a panicking observer must not take the
// automation down with it, because the observer is a diagnostic and the run
// is the work.
func notifyStepObserver(ctx context.Context, result *StepResult) {
	if result == nil {
		return
	}
	obs := stepObserverFrom(ctx)
	if obs == nil {
		return
	}
	defer func() { _ = recover() }()
	obs(result)
}
