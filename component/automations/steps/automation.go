package steps

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/visionarys-io/memql/component/automations"
)

// AutomationExecutor invokes other automations.
type AutomationExecutor struct{}

// Execute runs an automation step.
func (e *AutomationExecutor) Execute(ctx context.Context, step *automations.Step, stepCtx *Context) (*automations.StepResult, error) {
	result := &automations.StepResult{
		StepId:    step.ID,
		StartedAt: time.Now(),
	}

	if step.Automation == nil {
		result.Status = "failed"
		result.Error = "automation configuration is required"
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, fmt.Errorf("automation configuration is required")
	}

	if stepCtx.AutomationTrigger == nil {
		result.Status = "failed"
		result.Error = "automation trigger not configured"
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, fmt.Errorf("automation trigger not configured")
	}

	automationName := strings.TrimSpace(step.Automation.Name)
	if automationName == "" {
		result.Status = "failed"
		result.Error = "automation name is required"
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, fmt.Errorf("automation name is required")
	}

	// Check for recursive invocation
	if stepCtx.Execution != nil && stepCtx.Execution.AutomationName == automationName {
		result.Status = "failed"
		result.Error = fmt.Sprintf("recursive automation invocation detected: %s", automationName)
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, fmt.Errorf("recursive automation invocation detected: %s", automationName)
	}

	if stepCtx.Logger != nil {
		stepCtx.Logger.Debug("executing automation step",
			"step", step.ID,
			"targetAutomation", automationName,
			"async", step.Automation.Async,
		)
	}

	// Async execution: fire and forget
	if step.Automation.Async {
		go func() {
			_, err := stepCtx.AutomationTrigger.TriggerAutomation(context.Background(), automationName)
			if err != nil && stepCtx.Logger != nil {
				stepCtx.Logger.Warn("async automation trigger failed",
					"targetAutomation", automationName,
					"error", err,
				)
			}
		}()

		result.Status = "success"
		result.Result = map[string]any{
			"automation": automationName,
			"async":      true,
			"triggered":  true,
		}
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		result.Metadata = map[string]any{
			"automation": automationName,
			"async":      true,
		}

		// Record step execution in the database
		runId := ""
		if stepCtx.Execution != nil {
			runId = stepCtx.Execution.ID
		}
		stepRecordQuery := RecordStepExecution(ctx, stepCtx.Engine, StepRecordData{
			RunId:    runId,
			StepId:   step.ID,
			StepType: "automation",
			Status:   result.Status,
			Duration: float64(result.Duration.Milliseconds()),
		})

		if stepCtx.Logger != nil {
			stepCtx.Logger.Debug("automation step triggered (async)",
				"step", step.ID,
				"targetAutomation", automationName,
				"stepRecord", stepRecordQuery,
			)
		}

		return result, nil
	}

	// Synchronous execution: wait for completion. If the step carries
	// args, dispatch via TriggerAutomationWithArgs so the
	// sub-automation sees the call args as its input envelope
	// (matching how event-triggered invocations see the event
	// payload). Without args, the no-args path preserves the legacy
	// "manual trigger" behaviour.
	var (
		execResult *automations.AutomationExecution
		err        error
	)
	if len(step.Automation.Args) > 0 {
		execResult, err = stepCtx.AutomationTrigger.TriggerAutomationWithArgs(ctx, automationName, step.Automation.Args)
	} else {
		execResult, err = stepCtx.AutomationTrigger.TriggerAutomation(ctx, automationName)
	}
	if err != nil {
		result.Status = "failed"
		result.Error = fmt.Sprintf("automation %q execution failed: %v", automationName, err)
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, fmt.Errorf("automation %q execution failed: %w", automationName, err)
	}

	result.Status = "success"
	result.Result = map[string]any{
		"automation":  automationName,
		"executionId": execResult.ID,
		"status":      execResult.Status,
		"duration":    execResult.Duration.Milliseconds(),
		"stepCount":   len(execResult.Steps),
	}
	result.CompletedAt = time.Now()
	result.Duration = result.CompletedAt.Sub(result.StartedAt)
	result.Metadata = map[string]any{
		"automation":  automationName,
		"executionId": execResult.ID,
	}

	// Record step execution in the database
	runId := ""
	if stepCtx.Execution != nil {
		runId = stepCtx.Execution.ID
	}
	// Build result query to retrieve the triggered automation's run record
	resultQuery := fmt.Sprintf(`concept==v1:memql:automation:run;id==%s)`, jsonString(execResult.ID))
	stepRecordQuery := RecordStepExecution(ctx, stepCtx.Engine, StepRecordData{
		RunId:       runId,
		StepId:      step.ID,
		StepType:    "automation",
		Status:      result.Status,
		ResultQuery: resultQuery,
		Duration:    float64(result.Duration.Milliseconds()),
	})

	if stepCtx.Logger != nil {
		stepCtx.Logger.Debug("automation step completed",
			"step", step.ID,
			"targetAutomation", automationName,
			"executionId", execResult.ID,
			"stepRecord", stepRecordQuery,
			"duration", formatDuration(result.Duration),
		)
	}

	return result, nil
}
