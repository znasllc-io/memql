package steps

import (
	"context"
	"fmt"
	"time"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/memql"
)

// QueryExecutor executes MemQL queries and mutations.
type QueryExecutor struct{}

// Execute runs a MemQL query step.
func (e *QueryExecutor) Execute(ctx context.Context, step *automations.Step, stepCtx *Context) (*automations.StepResult, error) {
	result := &automations.StepResult{
		StepId:    step.ID,
		StartedAt: time.Now(),
	}

	if step.Query == nil {
		result.Status = "failed"
		result.Error = "query configuration is required"
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, fmt.Errorf("query configuration is required")
	}

	if stepCtx.Engine == nil {
		result.Status = "failed"
		result.Error = "MemQL engine not configured"
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, fmt.Errorf("MemQL engine not configured")
	}

	// Evaluate $ expressions in the query, using query-aware formatting
	// that properly quotes strings containing operator characters (like UUIDs)
	query, err := stepCtx.Evaluator.EvaluateStringForQuery(step.Query.Query)
	if err != nil {
		result.Status = "failed"
		result.Error = fmt.Sprintf("failed to evaluate query: %v", err)
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, fmt.Errorf("failed to evaluate query: %w", err)
	}

	if stepCtx.Logger != nil {
		stepCtx.Logger.Debug("executing query step",
			"step", step.ID,
			"query", query,
		)
	}

	// Execute the query
	execResult, err := stepCtx.Engine.Execute(ctx, query)
	if err != nil {
		result.Status = "failed"
		result.Error = err.Error()
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, fmt.Errorf("query execution failed: %w", err)
	}

	// Convert result to a usable format
	result.Status = "success"
	result.Result = convertExecuteResult(execResult)
	result.CompletedAt = time.Now()
	result.Duration = result.CompletedAt.Sub(result.StartedAt)

	// Extract item count from result for condition evaluation
	itemCount := extractItemCount(execResult)

	// Debug: log query results
	if stepCtx.Logger != nil {
		stepCtx.Logger.Info("query: executed",
			"step", step.ID,
			"query", query,
			"itemCount", itemCount,
			"resultType", fmt.Sprintf("%T", result.Result),
		)
	}

	// Build the result query (for queries, it's the same; for mutations, it retrieves the inserted record)
	resultQuery := BuildResultQuery("query", query, execResult)

	result.Metadata = map[string]any{
		"query":       query,
		"itemCount":   itemCount,
		"resultQuery": resultQuery,
	}

	// Record step execution in the database
	runId := ""
	if stepCtx.Execution != nil {
		runId = stepCtx.Execution.ID
	}
	stepRecordQuery := RecordStepExecution(ctx, stepCtx.Engine, StepRecordData{
		RunId:       runId,
		StepId:      step.ID,
		StepType:    "query",
		Status:      result.Status,
		Query:       query,
		ResultQuery: resultQuery,
		ItemCount:   itemCount,
		Duration:    float64(result.Duration.Milliseconds()),
	})

	if stepCtx.Logger != nil {
		stepCtx.Logger.Debug("query step completed",
			"step", step.ID,
			"resultQuery", resultQuery,
			"itemCount", itemCount,
			"stepRecord", stepRecordQuery,
			"duration", formatDuration(result.Duration),
		)
	}

	return result, nil
}

// convertExecuteResult converts a MemQL ExecuteResult to an automation-friendly format.
func convertExecuteResult(execResult any) any {
	if execResult == nil {
		return nil
	}

	// Try to extract bundle if available
	// For now, return the raw result - the shape step can transform it
	return execResult
}

// extractItemCount extracts the number of result items from an ExecuteResult.
// Returns 0 if the result is nil or empty.
func extractItemCount(execResult any) int {
	if execResult == nil {
		return 0
	}

	// Handle the actual memql.ExecuteResult type
	if er, ok := execResult.(*memql.ExecuteResult); ok {
		if er == nil || er.Bundle == nil {
			return 0
		}
		return len(er.Bundle.GetNodes())
	}

	// Fallback: try JSON marshaling approach
	// This handles cases where the result is already converted to map[string]any
	switch v := execResult.(type) {
	case map[string]any:
		if bundle, ok := v["Bundle"].(map[string]any); ok {
			if nodes, ok := bundle["nodes"].([]any); ok {
				return len(nodes)
			}
		}
	}

	return 0
}
