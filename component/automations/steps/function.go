package steps

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/memql"
)

// FunctionExecutor invokes MemQL functions.
type FunctionExecutor struct{}

// Execute runs a function step.
func (e *FunctionExecutor) Execute(ctx context.Context, step *automations.Step, stepCtx *Context) (*automations.StepResult, error) {
	result := &automations.StepResult{
		StepId:    step.ID,
		StartedAt: time.Now(),
	}

	if step.Function == nil {
		result.Status = "failed"
		result.Error = "function configuration is required"
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, fmt.Errorf("function configuration is required")
	}

	if stepCtx.Engine == nil {
		result.Status = "failed"
		result.Error = "MemQL engine not configured"
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, fmt.Errorf("MemQL engine not configured")
	}

	funcName := strings.TrimSpace(step.Function.Name)
	if funcName == "" {
		result.Status = "failed"
		result.Error = "function name is required"
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, fmt.Errorf("function name is required")
	}

	if stepCtx.Logger != nil {
		stepCtx.Logger.Debug("executing function step",
			"step", step.ID,
			"function", funcName,
		)
	}

	// The call: a construct call for the engine, its arguments evaluated
	// over the run and rendered as literals (memql#5367) -- a value is data,
	// never reference text. An expression builtin (coalesce, concat, ...) is
	// a catalog function inside an expression, which the query executor
	// evaluates; a function step never names one.
	args, resolveErr := stepCtx.Evaluator.ResolveV1Map(ctx, step.Function.Args)
	if resolveErr != nil {
		result.Status = "failed"
		result.Error = fmt.Sprintf("function %q argument resolution failed: %v", funcName, resolveErr)
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, fmt.Errorf("function %q argument resolution failed: %w", funcName, resolveErr)
	}
	// A person's inputs for this version (epic memql#5414) replace the
	// evaluated arguments they name; the rest keep their values.
	args = withOverrideInputs(ctx, args)
	query := funcName + "(" + renderV1CallArgs(args) + ")"
	execResult, err := stepCtx.Engine.Execute(ctx, query)
	if err != nil {
		result.Status = "failed"
		result.Error = fmt.Sprintf("function %q execution failed: %v", funcName, err)
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, fmt.Errorf("function %q execution failed: %w", funcName, err)
	}

	result.Status = "success"
	result.Result = execResult
	result.CompletedAt = time.Now()
	result.Duration = result.CompletedAt.Sub(result.StartedAt)

	itemCount := extractItemCount(execResult)

	result.Metadata = map[string]any{
		"function":  funcName,
		"itemCount": itemCount,
	}

	if stepCtx.Logger != nil {
		stepCtx.Logger.Debug("function step completed",
			"step", step.ID,
			"function", funcName,
			"duration", formatDuration(result.Duration),
		)
	}

	return result, nil
}

// extractItemCount is the number of rows an engine result carries, 0 when it
// carries none.
func extractItemCount(execResult any) int {
	if execResult == nil {
		return 0
	}
	if er, ok := execResult.(*memql.ExecuteResult); ok {
		if er == nil || er.Bundle == nil {
			return 0
		}
		return len(er.Bundle.GetNodes())
	}
	if v, ok := execResult.(map[string]any); ok {
		if bundle, ok := v["Bundle"].(map[string]any); ok {
			if nodes, ok := bundle["nodes"].([]any); ok {
				return len(nodes)
			}
		}
	}
	return 0
}
