package steps

// child_dispatch.go -- how a container step (forEach / parallel / switch)
// resolves the executor for its nested steps (memql#2943).
//
// # THE DEFECT THIS EXISTS TO FIX
//
// The Gate-2 dry-run sandbox (sandbox_registry.go) wraps the step registry at
// the automations.StepExecutorRegistry seam -- a single Execute method -- and
// the automation Executor drives it instead of the real registry. That is
// enough for TOP-LEVEL steps.
//
// It was not enough for nested ones. ForEachExecutor, ParallelExecutor and
// SwitchExecutor each held the CONCRETE *Registry and resolved children with
// e.Registry.Get(...), which is the production registry the sandbox wraps --
// not the sandbox. A nested step therefore reached the production executor
// directly, and the interception layer never saw it:
//
//	forEach:                 <- delegated to the real ForEachExecutor
//	  steps:
//	    - mutation: ...      <- resolved via the REAL registry -> engine.Execute
//
// So a dry-run of `forEach { mutation }` wrote to the live graph, even though
// `mutation` is precisely the step type the sandbox does intercept. Wrapping
// the outermost seam bought nothing the moment a container sat above the write.
//
// # THE FIX
//
// A container resolves children through `resolve`, which prefers an injected
// Dispatch over the registry. The sandbox builds its own container executors
// with Dispatch set to its own Execute, so nested steps re-enter the SAME
// interception layer -- recursively, for containers nested inside containers.
//
// Dispatch is nil in production, where resolve is exactly the old
// e.Registry.Get and behaviour is unchanged.
//
// Note the deliberate asymmetry in the `ok` result: with Dispatch set, resolve
// reports true for EVERY step type, including one the registry does not know.
// That is not a bug -- it routes the unknown type into the sandbox, whose job
// is to refuse what it cannot classify. Returning false here would send an
// unrecognised nested step down the container's own "unknown step type" path
// and out of the sandbox's sight, which is the class of hole this file closes.

import (
	"context"

	"github.com/znasllc-io/memql/component/automations"
)

// StepDispatchFunc runs one step. It matches Executor.Execute and the
// automations.StepExecutorRegistry seam, so a sandbox registry's own Execute
// satisfies it directly.
type StepDispatchFunc func(ctx context.Context, step *automations.Step, stepCtx *Context) (*automations.StepResult, error)

// Execute lets a StepDispatchFunc stand in for an Executor, so the container
// call sites keep their existing shape.
func (f StepDispatchFunc) Execute(ctx context.Context, step *automations.Step, stepCtx *Context) (*automations.StepResult, error) {
	return f(ctx, step, stepCtx)
}

// resolveChild picks the executor for a nested step: the injected dispatcher
// when present, otherwise the registry. Shared by the three container
// executors so they cannot drift apart on the rule that matters.
func resolveChild(dispatch StepDispatchFunc, registry *Registry, stepType automations.StepType) (Executor, bool) {
	if dispatch != nil {
		return dispatch, true
	}
	if registry == nil {
		return nil, false
	}
	return registry.Get(stepType)
}
