// Package steps provides step executors for the automation engine.
package steps

import (
	"context"
	"fmt"
	"time"

	"github.com/znasllc-io/memql/component/automations"
)

// formatDuration returns a human-readable duration string in milliseconds.
// Examples: "1.234ms", "456.789ms", "1234.567ms"
func formatDuration(d time.Duration) string {
	ms := float64(d.Nanoseconds()) / float64(time.Millisecond)
	return fmt.Sprintf("%.3fms", ms)
}

// Context is an alias for automations.StepContext for convenience.
type Context = automations.StepContext

// Executor is the interface that all step executors implement.
type Executor interface {
	// Execute runs the step and returns the result.
	Execute(ctx context.Context, step *automations.Step, stepCtx *Context) (*automations.StepResult, error)
}

// Registry holds all registered step executors.
type Registry struct {
	executors map[automations.StepType]Executor
}

// NewRegistry creates a new step executor registry with all built-in executors.
func NewRegistry() *Registry {
	r := &Registry{
		executors: make(map[automations.StepType]Executor),
	}

	// One executor per step kind a statement compiles to (epic memql#5370).
	// A statement body's expression and return steps never reach the
	// registry: runSequence evaluates them itself.
	r.Register(automations.StepTypeEvent, &EventExecutor{})
	r.Register(automations.StepTypeFunction, &FunctionExecutor{})
	r.Register(automations.StepTypeAction, &ActionExecutor{})
	r.Register(automations.StepTypeAutomation, &AutomationExecutor{})
	r.Register(automations.StepTypeForEach, &ForEachExecutor{})
	r.Register(automations.StepTypeParallel, &ParallelExecutor{Registry: r})
	r.Register(automations.StepTypeBlock, &BlockExecutor{})

	return r
}

// Register adds an executor for a step type.
func (r *Registry) Register(stepType automations.StepType, executor Executor) {
	r.executors[stepType] = executor
}

// Get retrieves an executor for a step type.
func (r *Registry) Get(stepType automations.StepType) (Executor, bool) {
	exec, ok := r.executors[stepType]
	return exec, ok
}

// Execute finds and runs the appropriate executor for a step.
func (r *Registry) Execute(ctx context.Context, step *automations.Step, stepCtx *Context) (*automations.StepResult, error) {
	executor, ok := r.Get(step.Type)
	if !ok {
		return nil, &UnknownStepTypeError{Type: step.Type}
	}
	return executor.Execute(ctx, step, stepCtx)
}

// UnknownStepTypeError indicates an unregistered step type.
type UnknownStepTypeError struct {
	Type automations.StepType
}

func (e *UnknownStepTypeError) Error() string {
	return "unknown step type: " + string(e.Type)
}
