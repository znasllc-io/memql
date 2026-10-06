package memql

import (
	"context"
	"fmt"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// WorkflowOperation is one bounded operation within an already-authorized call.
// It is never an authority token, a serialized capability, or a wire endpoint.
type WorkflowOperation func(context.Context, map[string]any) (any, error)

// ScopedWorkflowRunner is implemented by the ordinary automation interpreter.
// The engine cannot import that package; bootstrap supplies this runtime seam.
type ScopedWorkflowRunner func(context.Context, string, map[string]any, map[string]WorkflowOperation) (any, error)

func (e *MemQLEngine) SetScopedWorkflowRunner(run ScopedWorkflowRunner) { e.scopedWorkflow = run }

func (e *MemQLEngine) runScopedWorkflow(ctx context.Context, name string, args map[string]any, operations map[string]WorkflowOperation) (any, error) {
	if e.scopedWorkflow == nil {
		return nil, fmt.Errorf("scoped workflow runtime is not wired for %s", name)
	}
	return e.scopedWorkflow(ctx, name, args, operations)
}

func scopedRoutingOperation(context.Context, map[string]any, int) ([]memorynodes.MemoryNode, error) {
	return nil, fmt.Errorf("routing proposal operation requires its authorized workflow scope")
}
