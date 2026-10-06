package memql

import (
	"context"
	"fmt"
	"strings"
)

// CheckpointResumeRequest identifies a persisted suspension. The integration
// must verify its durable checkpoint and bind execution to that checkpoint;
// returning true is not a declaration that the capability may simply repeat.
type CheckpointResumeRequest struct {
	RunID, StepKey, ApprovalID string
}

type CheckpointResumePreparer interface {
	PrepareCheckpointResume(context.Context, string, CheckpointResumeRequest) (context.Context, bool, error)
}

// PrepareCheckpointResume resolves the actual registered executor, never an
// authored kind label or a function-name allowlist. Unknown capabilities and
// compound statements have no implicit resumability.
func (e *MemQLEngine) PrepareCheckpointResume(ctx context.Context, name string, request CheckpointResumeRequest) (context.Context, bool, error) {
	if e == nil || e.Functions() == nil {
		return ctx, false, nil
	}
	fn, ok := e.Functions().Lookup(name)
	if !ok || fn == nil || !fn.IsBuiltin() {
		return ctx, false, nil
	}
	parts := strings.Split(fn.Executor, ".")
	if len(parts) != 3 || parts[0] != "integration" {
		return ctx, false, nil
	}
	if e.integrations == nil {
		return ctx, false, nil
	}
	if handler, registered := e.integrations.Get(fn.Executor); !registered || handler == nil {
		return ctx, false, nil
	}
	resumer, ok := e.IntegrationByName(parts[1]).(CheckpointResumePreparer)
	if !ok {
		return ctx, false, nil
	}
	prepared, accepted, err := resumer.PrepareCheckpointResume(ctx, parts[2], request)
	if err != nil {
		return ctx, false, err
	}
	if !accepted {
		return ctx, false, nil
	}
	if prepared == nil {
		return ctx, false, fmt.Errorf("checkpoint preparer returned no execution context")
	}
	return prepared, true, nil
}
