package steps

// child_dispatch.go -- how a parallel step resolves the executor for its
// branches (memql#2943).
//
// The Gate-2 dry-run sandbox (sandbox_registry.go) wraps the step registry at
// the automations.StepExecutorRegistry seam -- a single Execute method -- and
// the automation Executor drives it instead of the real registry. That is
// enough for a top-level step, and it was not enough for a nested one: a
// container that resolved its children through the CONCRETE *Registry sent a
// nested write to the production executor, out of the sandbox's sight. So a
// dry run of a write inside a container wrote to the live graph.
//
// A parallel resolves its branches through resolveChild, which prefers an
// injected Dispatch over the registry. The sandbox builds its parallel
// executor with Dispatch set to its own Execute, so each branch re-enters the
// SAME interception layer; the branch's list then runs through the sequence
// runner, whose registry is the sandbox too. A `for` has no Dispatch: its
// list runs through the sequence runner directly.
//
// Dispatch is nil in production, where resolveChild is exactly
// e.Registry.Get.
//
// Note the deliberate asymmetry in the `ok` result: with Dispatch set,
// resolveChild reports true for EVERY step type, including one the registry
// does not know. That is not a bug -- it routes the unknown type into the
// sandbox, whose job is to refuse what it cannot classify.

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

// resolveChild picks the executor for a parallel's branch: the injected
// dispatcher when present, otherwise the registry.
func resolveChild(dispatch StepDispatchFunc, registry *Registry, stepType automations.StepType) (Executor, bool) {
	if dispatch != nil {
		return dispatch, true
	}
	if registry == nil {
		return nil, false
	}
	return registry.Get(stepType)
}
