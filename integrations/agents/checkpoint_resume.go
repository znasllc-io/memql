package agents

import (
	"context"
	"fmt"

	"github.com/znasllc-io/memql/component/memql"
)

// Only the synchronous tool-loop capability restores a saved continuation.
// Invoking a new agent, or an arbitrary AI call, has no such contract.
func (i *Integration) PrepareCheckpointResume(ctx context.Context, capability string, request memql.CheckpointResumeRequest) (context.Context, bool, error) {
	if capability != "runAgentTurn" {
		return ctx, false, nil
	}
	preparer, ok := i.engine.(interface {
		PrepareWorkContinuation(context.Context, memql.CheckpointResumeRequest) (context.Context, error)
	})
	if !ok {
		return ctx, false, fmt.Errorf("durable agent continuation is unavailable")
	}
	prepared, err := preparer.PrepareWorkContinuation(ctx, request)
	return prepared, err == nil, err
}

var _ memql.CheckpointResumePreparer = (*Integration)(nil)
