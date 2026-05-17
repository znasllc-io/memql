package steps

import (
	"context"
	"fmt"
	"time"

	"github.com/znasllc-io/memql/component/automations"
)

// SwitchExecutor provides conditional branching.
type SwitchExecutor struct {
	Registry *Registry
}

// Execute runs a switch step.
func (e *SwitchExecutor) Execute(ctx context.Context, step *automations.Step, stepCtx *Context) (*automations.StepResult, error) {
	result := &automations.StepResult{
		StepId:    step.ID,
		StartedAt: time.Now(),
		Children:  make([]*automations.StepResult, 0),
	}

	if step.Switch == nil {
		result.Status = "failed"
		result.Error = "switch configuration is required"
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, fmt.Errorf("switch configuration is required")
	}

	switchCfg := step.Switch

	// Evaluate the switch expression
	exprValue, err := stepCtx.Evaluator.EvaluateValue(switchCfg.Expression)
	if err != nil {
		result.Status = "failed"
		result.Error = fmt.Sprintf("failed to evaluate expression: %v", err)
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, fmt.Errorf("failed to evaluate expression: %w", err)
	}

	// Convert to string for case matching
	exprStr := automations.FormatValue(exprValue)

	if stepCtx.Logger != nil {
		stepCtx.Logger.Debug("executing switch step",
			"step", step.ID,
			"expression", switchCfg.Expression,
			"value", exprStr,
		)
	}

	// Find matching case
	var selectedCase *automations.SwitchCase
	var matchedValue string

	if switchCase, ok := switchCfg.Cases[exprStr]; ok {
		selectedCase = switchCase
		matchedValue = exprStr
	} else if switchCfg.Default != nil {
		selectedCase = switchCfg.Default
		matchedValue = "default"
	}

	if selectedCase == nil {
		// No matching case and no default
		result.Status = "success"
		result.Result = nil
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		result.Metadata = map[string]any{
			"expression": switchCfg.Expression,
			"value":      exprStr,
			"matched":    false,
		}

		// Record step execution in the database
		runId := ""
		if stepCtx.Execution != nil {
			runId = stepCtx.Execution.ID
		}
		stepRecordQuery := RecordStepExecution(ctx, stepCtx.Engine, StepRecordData{
			RunId:    runId,
			StepId:   step.ID,
			StepType: "switch",
			Status:   result.Status,
			Duration: float64(result.Duration.Milliseconds()),
		})

		if stepCtx.Logger != nil {
			stepCtx.Logger.Debug("switch step completed (no match)",
				"step", step.ID,
				"value", exprStr,
				"stepRecord", stepRecordQuery,
			)
		}

		return result, nil
	}

	// Execute the selected case
	var stepsToExecute []*automations.Step

	if selectedCase.Step != nil {
		stepsToExecute = []*automations.Step{selectedCase.Step}
	} else if len(selectedCase.Steps) > 0 {
		stepsToExecute = selectedCase.Steps
	}

	var lastResult any
	for i, caseStep := range stepsToExecute {
		// Create unique step ID
		caseStepId := fmt.Sprintf("%s.case[%s].%d", step.ID, matchedValue, i)
		caseStepCopy := *caseStep
		caseStepCopy.ID = caseStepId

		executor, ok := e.Registry.Get(caseStep.Type)
		if !ok {
			childResult := &automations.StepResult{
				StepId:      caseStepId,
				Status:      "failed",
				Error:       fmt.Sprintf("unknown step type: %s", caseStep.Type),
				StartedAt:   time.Now(),
				CompletedAt: time.Now(),
			}
			result.Children = append(result.Children, childResult)
			result.Status = "failed"
			result.Error = childResult.Error
			result.CompletedAt = time.Now()
			result.Duration = result.CompletedAt.Sub(result.StartedAt)
			return result, fmt.Errorf("unknown step type: %s", caseStep.Type)
		}

		childResult, err := executor.Execute(ctx, &caseStepCopy, stepCtx)
		if childResult != nil {
			result.Children = append(result.Children, childResult)
			stepCtx.Evaluator.SetStepResult(caseStep.ID, childResult)
			lastResult = childResult.Result
		}

		if err != nil {
			result.Status = "failed"
			result.Error = err.Error()
			result.CompletedAt = time.Now()
			result.Duration = result.CompletedAt.Sub(result.StartedAt)
			return result, err
		}
	}

	result.Status = "success"
	result.Result = lastResult
	result.CompletedAt = time.Now()
	result.Duration = result.CompletedAt.Sub(result.StartedAt)
	result.Metadata = map[string]any{
		"expression":    switchCfg.Expression,
		"value":         exprStr,
		"matched":       true,
		"matchedCase":   matchedValue,
		"stepsExecuted": len(stepsToExecute),
	}

	// Record step execution in the database
	runId := ""
	if stepCtx.Execution != nil {
		runId = stepCtx.Execution.ID
	}
	stepRecordQuery := RecordStepExecution(ctx, stepCtx.Engine, StepRecordData{
		RunId:     runId,
		StepId:    step.ID,
		StepType:  "switch",
		Status:    result.Status,
		ItemCount: len(stepsToExecute),
		Duration:  float64(result.Duration.Milliseconds()),
	})

	if stepCtx.Logger != nil {
		stepCtx.Logger.Debug("switch step completed",
			"step", step.ID,
			"matchedCase", matchedValue,
			"stepsExecuted", len(stepsToExecute),
			"stepRecord", stepRecordQuery,
			"duration", formatDuration(result.Duration),
		)
	}

	return result, nil
}
