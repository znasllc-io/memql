package automations

import (
	"context"
	"fmt"
	"time"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/core/id"
)

// ResumeOptions configures resume behavior.
type ResumeOptions struct {
	// FromStep overrides the resume point (defaults to the failed step).
	FromStep string

	// AllowSideEffects permits retrying mutation/webhook steps.
	// Without this flag, resuming from a non-retryable step returns an error.
	AllowSideEffects bool
}

// ResumeFrom resumes execution from a checkpoint.
// It rehydrates the evaluator with completed step results from the checkpoint,
// then continues execution from the specified step (or the failed step if not specified).
func (e *Executor) ResumeFrom(
	ctx context.Context,
	checkpoint *ExecutionCheckpoint,
	automation *Automation,
	opts *ResumeOptions,
) (*AutomationExecution, error) {
	if checkpoint == nil {
		return nil, fmt.Errorf("checkpoint is nil")
	}
	if automation == nil {
		return nil, fmt.Errorf("automation is nil")
	}
	if opts == nil {
		opts = &ResumeOptions{}
	}

	// Validate checkpoint against current automation
	if err := ValidateCheckpoint(checkpoint, automation, fingerprintEngine); err != nil {
		return nil, err
	}

	// Determine the resume point
	resumeStepId := checkpoint.FailedAt.StepId
	if opts.FromStep != "" {
		resumeStepId = opts.FromStep
	}

	// Find the step index to resume from
	resumeIndex := -1
	var resumeStep *Step
	for i, step := range automation.Steps {
		if step.ID == resumeStepId {
			resumeIndex = i
			resumeStep = step
			break
		}
	}
	if resumeIndex == -1 {
		return nil, fmt.Errorf("step %q not found in automation", resumeStepId)
	}

	// Check if resume step is retryable
	if !IsStepRetryable(resumeStep.Type) && !opts.AllowSideEffects {
		return nil, fmt.Errorf("%w: step %q is type %s, set AllowSideEffects to retry",
			ErrNonRetryableStep, resumeStepId, resumeStep.Type)
	}

	// Inject system actor for automation execution
	ctx = contextWithSystemActor(ctx, automation.Name)

	// Create new execution tracking the resume
	triggeredBy := fmt.Sprintf("resumed:%s", checkpoint.ExecutionId)
	exec := NewExecution(automation.Name, triggeredBy)

	// Set up evaluator
	evaluator := NewEvaluator()
	evaluator.SetVariableResolver(e.createVariableResolver())
	evaluator.SetSystemVariableResolver(e.createSystemVariableResolver())
	evaluator.SetSecretResolver(e.createSecretResolver())
	evaluator.SetSystemSecretResolver(e.createSystemSecretResolver())
	evaluator.SetCanonicalIdResolver(e.createCanonicalIdResolver())
	evaluator.SetLogger(e.logger)
	evaluator.SetCustom("timestamp", time.Now().UTC().Format(time.RFC3339))

	// Restore input from checkpoint
	if checkpoint.Input != nil {
		exec.Input = checkpoint.Input
		evaluator.SetInput(checkpoint.Input)
	}

	// Restore trigger context from checkpoint. When the checkpoint has
	// no triggering event (cron / manual / startup resume), seed a
	// synthetic object envelope so step args that pass `event: event`
	// still resolve to an object rather than the unresolved literal
	// `event` token (which the engine coerces to a string and the
	// receiving logic function rejects -- see executor.go / issue #418).
	if checkpoint.TriggerContext != nil && checkpoint.TriggerContext.Event != nil {
		evaluator.SetCustom("event", checkpoint.TriggerContext.Event)
	} else {
		evaluator.SetCustom("event", buildEventEnvelope(nil, "resume", "resume"))
	}

	// Rehydrate evaluator with completed step results from checkpoint
	for stepId, minResult := range checkpoint.StepResults {
		if minResult == nil {
			continue
		}
		// Convert MinimalStepResult to StepResult for evaluator
		fullResult := minimalToStepResult(minResult)
		evaluator.SetStepResult(stepId, fullResult)
		exec.AddStepResult(fullResult)
	}

	if e.logger != nil {
		e.logger.Info("resuming automation execution",
			"component", ComponentName,
			"automation", automation.Name,
			"executionId", exec.ID,
			"originalExecutionId", checkpoint.ExecutionId,
			"resumeFromStep", resumeStepId,
			"resumeIndex", resumeIndex,
			"restoredSteps", len(checkpoint.StepResults),
		)
	}

	// Publish automation resumed event
	e.publishEvent("automation.resumed", events.KindTelemetry, map[string]any{
		"automationName":      automation.Name,
		"executionId":         exec.ID,
		"originalExecutionId": checkpoint.ExecutionId,
		"resumeFromStep":      resumeStepId,
		"restoredSteps":       len(checkpoint.StepResults),
	})

	// Chain tracking - start from checkpoint's chain head
	// Include completed steps from checkpoint in StepOrder for chain verification
	// Only copy steps BEFORE the resume point to avoid duplicates
	var chainHead string
	if e.chainTrackingEnabled {
		// Copy only steps before resumeIndex (not including the failed step)
		// The failed step will be added when it executes
		if len(checkpoint.StepOrder) > 0 && resumeIndex > 0 {
			// Find how many steps from checkpoint.StepOrder to keep
			// This is the minimum of resumeIndex and the checkpoint's step count
			stepsToKeep := resumeIndex
			if stepsToKeep > len(checkpoint.StepOrder) {
				stepsToKeep = len(checkpoint.StepOrder)
			}
			exec.StepOrder = make([]string, stepsToKeep, len(automation.Steps))
			copy(exec.StepOrder, checkpoint.StepOrder[:stepsToKeep])
		} else {
			exec.StepOrder = make([]string, 0, len(automation.Steps))
		}
		exec.InitialChainHead = checkpoint.InitialChainHead
		chainHead = checkpoint.ChainHead // Resume from checkpoint's chain position
		if chainHead == "" {
			chainHead = checkpoint.InitialChainHead
		}
	}

	// Set up step context
	// SkipCache ensures resumed steps fetch fresh data instead of stale cached results
	stepCtx := &StepContext{
		Logger:               e.logger,
		Engine:               e.engine,
		EventBus:             e.eventBus,
		Evaluator:            evaluator,
		Execution:            exec,
		AutomationTrigger:    e.automationTrigger,
		ChainTrackingEnabled: e.chainTrackingEnabled,
		SkipCache:            true,
	}

	// Execute steps starting from resumeIndex
	for i := resumeIndex; i < len(automation.Steps); i++ {
		step := automation.Steps[i]

		// Track step order for chain verification
		if e.chainTrackingEnabled {
			exec.StepOrder = append(exec.StepOrder, step.ID)
		}

		// Check for cancellation
		select {
		case <-ctx.Done():
			exec.Cancel()
			return exec, ctx.Err()
		default:
		}

		// Evaluate step condition if present
		if step.Condition != "" {
			if e.logger != nil {
				e.logger.Debug("evaluating step condition",
					"step", step.ID,
					"condition", step.Condition,
				)
			}
			shouldRun, err := evaluator.EvaluateCondition(step.Condition)
			if err != nil {
				if e.logger != nil {
					e.logger.Warn("step condition evaluation failed",
						"step", step.ID,
						"condition", step.Condition,
						"error", err,
					)
				}
				shouldRun = false
			}
			if !shouldRun {
				// Record skipped step
				skipResult := &StepResult{
					StepId:      step.ID,
					Status:      "skipped",
					StartedAt:   time.Now(),
					CompletedAt: time.Now(),
				}
				exec.AddStepResult(skipResult)
				evaluator.SetStepResult(step.ID, skipResult)
				continue
			}
		}

		// Set current chain head in context
		if e.chainTrackingEnabled {
			stepCtx.PreviousChainHead = chainHead
		}

		// Execute the step
		result, err := e.executeStep(ctx, step, stepCtx)
		if result != nil {
			// Compute chain linkage if tracking enabled
			if e.chainTrackingEnabled {
				result.PreviousChainHead = chainHead
				result.ContentId = StepDeterministicFingerprint(step, result)
				chainHead = string(fingerprintEngine.Combine(
					id.ID(chainHead),
					id.ID(result.ContentId),
				))
			}

			exec.AddStepResult(result)
			evaluator.SetStepResult(step.ID, result)
		}

		if err != nil {
			// Handle error based on strategy
			switch step.OnError {
			case ErrorStrategyContinue:
				if e.logger != nil {
					e.logger.Warn("step failed, continuing",
						"component", ComponentName,
						"step", step.ID,
						"error", err,
					)
				}
				continue
			case ErrorStrategyRetry:
				// Retry logic
				retried := false
				for attempt := 1; attempt <= step.RetryCount && !retried; attempt++ {
					if e.logger != nil {
						e.logger.Info("retrying step",
							"component", ComponentName,
							"step", step.ID,
							"attempt", attempt,
						)
					}
					result, err = e.executeStep(ctx, step, stepCtx)
					if err == nil {
						if e.chainTrackingEnabled && result != nil {
							result.PreviousChainHead = chainHead
							result.ContentId = StepDeterministicFingerprint(step, result)
							chainHead = string(fingerprintEngine.Combine(
								id.ID(chainHead),
								id.ID(result.ContentId),
							))
						}
						exec.AddStepResult(result)
						evaluator.SetStepResult(step.ID, result)
						retried = true
					}
				}
				if !retried {
					exec.Fail(err)
					// Note: We don't save a new checkpoint here to avoid loops.
					// The original checkpoint remains valid for retry.
					return exec, err
				}
			default: // ErrorStrategyStop
				exec.Fail(err)
				return exec, err
			}
		}
	}

	// Execute onComplete hook if defined
	if automation.OnComplete != nil {
		_, err := e.executeStep(ctx, automation.OnComplete, stepCtx)
		if err != nil && e.logger != nil {
			e.logger.Warn("onComplete hook failed",
				"component", ComponentName,
				"error", err,
			)
		}
	}

	exec.Complete()

	// Finalize chain tracking
	if e.chainTrackingEnabled {
		exec.ChainHead = chainHead
	}

	// Publish automation completed event
	completedPayload := map[string]any{
		"automationName":      automation.Name,
		"executionId":         exec.ID,
		"originalExecutionId": checkpoint.ExecutionId,
		"resumed":             true,
		"duration":            exec.Duration.Milliseconds(),
		"stepCount":           len(exec.Steps),
	}
	if e.chainTrackingEnabled && exec.ChainHead != "" {
		completedPayload["chainHead"] = exec.ChainHead
	}
	e.publishEvent(events.TopicAutomationCompleted, events.KindAutomationCompleted, completedPayload)

	if e.logger != nil {
		e.logger.Info("resumed automation execution completed",
			"component", ComponentName,
			"automation", automation.Name,
			"executionId", exec.ID,
			"originalExecutionId", checkpoint.ExecutionId,
			"status", exec.Status,
			"duration", exec.Duration,
		)
	}

	return exec, nil
}

// minimalToStepResult converts a MinimalStepResult back to a full StepResult.
// This is used to rehydrate the evaluator during resume.
func minimalToStepResult(min *MinimalStepResult) *StepResult {
	if min == nil {
		return nil
	}

	result := &StepResult{
		StepId:    min.StepId,
		Status:    min.Status,
		Result:    min.Result,
		Error:     min.Error,
		ContentId: min.ContentId,
	}

	// Copy metadata
	if min.Metadata != nil {
		result.Metadata = make(map[string]any, len(min.Metadata))
		for k, v := range min.Metadata {
			result.Metadata[k] = v
		}
	}

	return result
}
