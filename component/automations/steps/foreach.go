package steps

import (
	"context"

	"github.com/znasllc-io/memql/component/automations"
)

// ForEachExecutor runs a `for` statement: its list once per item, through the
// sequence runner, in a frame per iteration (forEachStatements,
// statements.go).
type ForEachExecutor struct{}

// Execute runs a forEach step.
func (e *ForEachExecutor) Execute(ctx context.Context, step *automations.Step, stepCtx *Context) (*automations.StepResult, error) {
	return forEachStatements(ctx, step, stepCtx)
}
