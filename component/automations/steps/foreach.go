package steps

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/automations"
)

// ForEachExecutor iterates over collections.
type ForEachExecutor struct {
	Registry *Registry
}

// Execute runs a forEach step.
func (e *ForEachExecutor) Execute(ctx context.Context, step *automations.Step, stepCtx *Context) (*automations.StepResult, error) {
	result := &automations.StepResult{
		StepId:    step.ID,
		StartedAt: time.Now(),
		Children:  make([]*automations.StepResult, 0),
	}

	if step.ForEach == nil {
		result.Status = "failed"
		result.Error = "forEach configuration is required"
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, fmt.Errorf("forEach configuration is required")
	}

	forEachCfg := step.ForEach

	// Resolve the source collection
	// Use EvaluateStepReference to auto-resolve bare step names like "stepName.result"
	// to "$steps.stepName.result" for cleaner .memql syntax
	sourceValue, err := stepCtx.Evaluator.EvaluateStepReference(forEachCfg.Source)
	if err != nil {
		result.Status = "failed"
		result.Error = fmt.Sprintf("failed to evaluate source: %v", err)
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, fmt.Errorf("failed to evaluate source: %w", err)
	}

	// Debug: log what the source resolved to
	if stepCtx.Logger != nil {
		stepCtx.Logger.Info("forEach: source resolved",
			"step", step.ID,
			"sourceExpr", forEachCfg.Source,
			"sourceType", fmt.Sprintf("%T", sourceValue),
			"sourceIsNil", sourceValue == nil,
		)
	}

	// Convert to slice
	items, err := automations.ToSlice(sourceValue)
	if err != nil {
		result.Status = "failed"
		result.Error = fmt.Sprintf("source is not iterable: %v", err)
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, fmt.Errorf("source is not iterable: %w", err)
	}

	if len(items) == 0 {
		result.Status = "success"
		result.Result = []any{}
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		result.Metadata = map[string]any{
			"itemCount":      0,
			"processedCount": 0,
		}
		return result, nil
	}

	// Determine item variable name
	itemName := forEachCfg.As
	if itemName == "" {
		itemName = "item"
	}

	if stepCtx.Logger != nil {
		stepCtx.Logger.Debug("executing forEach step",
			"step", step.ID,
			"itemCount", len(items),
			"concurrency", forEachCfg.Concurrency,
		)
	}

	// Filter items if filter expression is provided
	var filteredItems []any
	if forEachCfg.Filter != "" {
		for _, item := range items {
			// Create a scoped evaluator for filter evaluation
			filterEval := stepCtx.Evaluator.Clone()
			filterEval.SetItem(item, itemName)

			matches, err := filterEval.EvaluateCondition(forEachCfg.Filter)
			if err != nil {
				// Log warning but continue
				if stepCtx.Logger != nil {
					stepCtx.Logger.Warn("filter evaluation failed",
						"step", step.ID,
						"error", err,
					)
				}
				continue
			}
			if matches {
				filteredItems = append(filteredItems, item)
			}
		}
	} else {
		filteredItems = items
	}

	if len(filteredItems) == 0 {
		result.Status = "success"
		result.Result = []any{}
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		result.Metadata = map[string]any{
			"itemCount":      len(items),
			"filteredCount":  0,
			"processedCount": 0,
		}
		return result, nil
	}

	// Execute steps for each item
	var processedResults []any
	var childResults []*automations.StepResult
	var processingErrors []error

	// Track chain for sequential child execution
	childChainHead := stepCtx.PreviousChainHead

	concurrency := forEachCfg.Concurrency
	if concurrency <= 1 {
		// Sequential execution - children chain to each other
		for i, item := range filteredItems {
			iterationResult, itemResult, err := e.executeForItem(ctx, step, stepCtx, item, itemName, i, childChainHead)

			// Compute chain linkage for this iteration if tracking enabled
			if stepCtx.ChainTrackingEnabled && iterationResult != nil {
				iterationResult.PreviousChainHead = childChainHead
				iterationResult.ContentId = automations.ComputeChildFingerprint(step.ID, i, iterationResult)
				// Advance chain for next iteration (sequential chaining)
				childChainHead = automations.AdvanceChain(childChainHead, iterationResult.ContentId)
			}

			if iterationResult != nil {
				childResults = append(childResults, iterationResult)
			}
			if itemResult != nil {
				processedResults = append(processedResults, itemResult)
			}
			if err != nil {
				processingErrors = append(processingErrors, err)
				// Check error strategy
				if step.OnError != automations.ErrorStrategyContinue {
					break
				}
			}
		}
	} else {
		// Parallel execution with concurrency limit
		// All parallel children link to the same parent chain head
		type itemResult struct {
			index           int
			iterationResult *automations.StepResult
			result          any
			err             error
		}

		resultsChan := make(chan itemResult, len(filteredItems))
		semaphore := make(chan struct{}, concurrency)

		var wg sync.WaitGroup
		for i, item := range filteredItems {
			wg.Add(1)
			go func(idx int, itm any) {
				defer wg.Done()
				semaphore <- struct{}{}        // Acquire
				defer func() { <-semaphore }() // Release

				// All parallel iterations get the same parent chain head
				iterRes, res, err := e.executeForItem(ctx, step, stepCtx, itm, itemName, idx, childChainHead)

				// Compute chain linkage if tracking enabled
				if stepCtx.ChainTrackingEnabled && iterRes != nil {
					iterRes.PreviousChainHead = childChainHead // Same for all parallel
					iterRes.ContentId = automations.ComputeChildFingerprint(step.ID, idx, iterRes)
				}

				resultsChan <- itemResult{
					index:           idx,
					iterationResult: iterRes,
					result:          res,
					err:             err,
				}
			}(i, item)
		}

		// Wait for all goroutines to complete
		go func() {
			wg.Wait()
			close(resultsChan)
		}()

		// Collect results
		resultsMap := make(map[int]itemResult)
		for res := range resultsChan {
			resultsMap[res.index] = res
		}

		// Reassemble in order
		for i := 0; i < len(filteredItems); i++ {
			if res, ok := resultsMap[i]; ok {
				if res.iterationResult != nil {
					childResults = append(childResults, res.iterationResult)
				}
				if res.result != nil {
					processedResults = append(processedResults, res.result)
				}
				if res.err != nil {
					processingErrors = append(processingErrors, res.err)
				}
			}
		}
	}

	// Build final result
	result.Children = childResults

	// Collect child fingerprints for parent fingerprint computation
	if stepCtx.ChainTrackingEnabled {
		childFPs := make([]string, 0, len(childResults))
		for _, child := range childResults {
			if child != nil && child.ContentId != "" {
				childFPs = append(childFPs, child.ContentId)
			}
		}
		result.ChildFingerprints = childFPs
	}

	if len(processingErrors) > 0 && step.OnError != automations.ErrorStrategyContinue {
		result.Status = "failed"
		result.Error = fmt.Sprintf("forEach failed: %d errors", len(processingErrors))
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, processingErrors[0]
	}

	result.Status = "success"
	result.Result = processedResults
	result.CompletedAt = time.Now()
	result.Duration = result.CompletedAt.Sub(result.StartedAt)
	result.Metadata = map[string]any{
		"itemCount":      len(items),
		"filteredCount":  len(filteredItems),
		"processedCount": len(processedResults),
		"errorCount":     len(processingErrors),
	}

	// Record step execution in the database
	runId := ""
	if stepCtx.Execution != nil {
		runId = stepCtx.Execution.ID
	}
	stepRecordQuery := RecordStepExecution(ctx, stepCtx.Engine, StepRecordData{
		RunId:     runId,
		StepId:    step.ID,
		StepType:  "forEach",
		Status:    result.Status,
		ItemCount: len(processedResults),
		Duration:  float64(result.Duration.Milliseconds()),
	})

	if stepCtx.Logger != nil {
		stepCtx.Logger.Debug("forEach step completed",
			"step", step.ID,
			"processed", len(processedResults),
			"errors", len(processingErrors),
			"stepRecord", stepRecordQuery,
			"duration", formatDuration(result.Duration),
		)
	}

	return result, nil
}

// executeForItem executes the nested steps for a single item and returns an iteration result.
func (e *ForEachExecutor) executeForItem(
	ctx context.Context,
	step *automations.Step,
	stepCtx *Context,
	item any,
	itemName string,
	index int,
	chainHead string,
) (*automations.StepResult, any, error) {
	iterationStart := time.Now()
	iterationId := fmt.Sprintf("%s[%d]", step.ID, index)

	// Create a scoped evaluator with the item set
	itemEval := stepCtx.Evaluator.Clone()
	itemEval.SetItem(item, itemName)
	itemEval.SetCustom("index", index)

	// Create scoped step context with chain tracking info
	itemStepCtx := &Context{
		Logger:               stepCtx.Logger,
		Engine:               stepCtx.Engine,
		EventBus:             stepCtx.EventBus,
		Evaluator:            itemEval,
		Execution:            stepCtx.Execution,
		ChainTrackingEnabled: stepCtx.ChainTrackingEnabled,
		PreviousChainHead:    chainHead,
	}

	var nestedResults []*automations.StepResult
	var lastResult any
	var iterationErr error

	// Execute each nested step
	for _, nestedStep := range step.ForEach.Do {
		// Generate unique step ID for this iteration
		iteratedStepId := fmt.Sprintf("%s.%s[%d]", step.ID, nestedStep.ID, index)
		iteratedStep := *nestedStep
		iteratedStep.ID = iteratedStepId

		// Evaluate nested step condition (Automation executor does this, but nested steps
		// run via executors directly, so we must handle conditions here too).
		if strings.TrimSpace(iteratedStep.Condition) != "" {
			if stepCtx.Logger != nil {
				stepCtx.Logger.Debug("evaluating step condition",
					"step", iteratedStep.ID,
					"condition", iteratedStep.Condition,
				)
			}
			shouldRun, err := itemEval.EvaluateCondition(iteratedStep.Condition)
			if err != nil {
				if stepCtx.Logger != nil {
					stepCtx.Logger.Warn("step condition evaluation failed",
						"step", iteratedStep.ID,
						"condition", iteratedStep.Condition,
						"error", err,
					)
				}
				shouldRun = false
			}
			if stepCtx.Logger != nil {
				stepCtx.Logger.Debug("step condition evaluated",
					"step", iteratedStep.ID,
					"condition", iteratedStep.Condition,
					"result", shouldRun,
				)
			}
			if !shouldRun {
				nestedResult := &automations.StepResult{
					StepId:      iteratedStep.ID,
					Status:      "skipped",
					StartedAt:   time.Now(),
					CompletedAt: time.Now(),
				}
				if stepCtx.Logger != nil {
					stepCtx.Logger.Debug("step skipped due to condition",
						"step", iteratedStep.ID,
						"condition", iteratedStep.Condition,
					)
				}
				nestedResults = append(nestedResults, nestedResult)
				itemEval.SetStepResult(nestedStep.ID, nestedResult)
				continue
			}
		}

		executor, ok := e.Registry.Get(nestedStep.Type)
		if !ok {
			nestedResult := &automations.StepResult{
				StepId:      iteratedStepId,
				Status:      "failed",
				Error:       fmt.Sprintf("unknown step type: %s", nestedStep.Type),
				StartedAt:   time.Now(),
				CompletedAt: time.Now(),
			}
			nestedResults = append(nestedResults, nestedResult)
			iterationErr = fmt.Errorf("unknown step type: %s", nestedStep.Type)
			break
		}

		nestedResult, err := executor.Execute(ctx, &iteratedStep, itemStepCtx)
		if nestedResult != nil {
			nestedResults = append(nestedResults, nestedResult)
			itemEval.SetStepResult(nestedStep.ID, nestedResult)
			lastResult = nestedResult.Result
		}

		if err != nil {
			iterationErr = err
			break
		}
	}

	// Build the iteration result wrapper
	iterationResult := &automations.StepResult{
		StepId:      iterationId,
		StartedAt:   iterationStart,
		CompletedAt: time.Now(),
		Children:    nestedResults,
		Result:      lastResult,
	}
	iterationResult.Duration = iterationResult.CompletedAt.Sub(iterationResult.StartedAt)

	if iterationErr != nil {
		iterationResult.Status = "failed"
		iterationResult.Error = iterationErr.Error()
		return iterationResult, lastResult, iterationErr
	}

	iterationResult.Status = "success"
	return iterationResult, lastResult, nil
}
