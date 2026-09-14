package steps

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/automations"
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

	// Record step execution in the database
	runId := ""
	if stepCtx.Execution != nil {
		runId = stepCtx.Execution.ID
	}
	stepRecordQuery := RecordStepExecution(ctx, stepCtx.Engine, StepRecordData{
		RunId:        runId,
		StepId:       step.ID,
		StepType:     "function",
		Status:       result.Status,
		Query:        query,
		FunctionName: funcName,
		ItemCount:    itemCount,
		Duration:     float64(result.Duration.Milliseconds()),
	})

	if stepCtx.Logger != nil {
		stepCtx.Logger.Debug("function step completed",
			"step", step.ID,
			"function", funcName,
			"stepRecord", stepRecordQuery,
			"duration", formatDuration(result.Duration),
		)
	}

	return result, nil
}
