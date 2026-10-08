package memql

import (
	"context"
	"errors"
	"fmt"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// WorkflowOperation is one bounded operation within an already-authorized call.
// It is never an authority token, a serialized capability, or a wire endpoint.
type WorkflowOperation func(context.Context, map[string]any) (any, error)

// ScopedWorkflowRunner is implemented by the ordinary automation interpreter.
// The engine cannot import that package; bootstrap supplies this runtime seam.
type ScopedWorkflowRunner func(context.Context, string, map[string]any, map[string]WorkflowOperation) (any, error)

// ScopedSnapshotRunner is the cycle-free interpreter seam for frozen phases.
// Source is data; the native caller separately binds the permitted operations.
type ScopedSnapshotRunner func(context.Context, map[string]any, string, string, map[string]any, map[string]WorkflowOperation) (any, error)

var ErrScopedSnapshotUnwired = errors.New("scoped snapshot runtime is not wired")

func (e *MemQLEngine) SetScopedSnapshotRunner(run ScopedSnapshotRunner) { e.scopedSnapshot = run }

func (e *MemQLEngine) RunScopedSnapshot(ctx context.Context, snapshot map[string]any, contract, entry string, args map[string]any, operations map[string]WorkflowOperation) (any, error) {
	if e.scopedSnapshot == nil {
		return nil, fmt.Errorf("%w for %s", ErrScopedSnapshotUnwired, entry)
	}
	return e.scopedSnapshot(ctx, snapshot, contract, entry, args, operations)
}

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
