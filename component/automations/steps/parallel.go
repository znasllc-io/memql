package steps

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/automations"
)

// ParallelExecutor executes multiple steps concurrently.
type ParallelExecutor struct {
	Registry *Registry
}

// Execute runs a parallel step.
func (e *ParallelExecutor) Execute(ctx context.Context, step *automations.Step, stepCtx *Context) (*automations.StepResult, error) {
	result := &automations.StepResult{
		StepId:    step.ID,
		StartedAt: time.Now(),
		Children:  make([]*automations.StepResult, 0),
	}

	if step.Parallel == nil {
		result.Status = "failed"
		result.Error = "parallel configuration is required"
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, fmt.Errorf("parallel configuration is required")
	}

	parallelCfg := step.Parallel
	branches := parallelCfg.Branches

	if len(branches) == 0 {
		result.Status = "success"
		result.Result = []any{}
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, nil
	}

	if stepCtx.Logger != nil {
		stepCtx.Logger.Debug("executing parallel step",
			"step", step.ID,
			"branchCount", len(branches),
			"wait", parallelCfg.Wait,
		)
	}

	// Determine wait strategy
	wait := parallelCfg.Wait
	if wait == "" {
		wait = "all"
	}

	// Create cancellable context for failFast
	execCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Execute branches concurrently
	type branchResult struct {
		index  int
		result *automations.StepResult
		err    error
	}

	resultsChan := make(chan branchResult, len(branches))
	var wg sync.WaitGroup
	var firstError error
	var firstErrorMu sync.Mutex

	// All parallel branches link to the same parent chain head
	parallelChainHead := stepCtx.PreviousChainHead

	for i, branch := range branches {
		wg.Add(1)
		go func(idx int, branchStep *automations.Step) {
			defer wg.Done()

			// Create unique step ID for this branch
			branchStepId := fmt.Sprintf("%s.%s", step.ID, branchStep.ID)
			branchCopy := *branchStep
			branchCopy.ID = branchStepId

			// Create branch-specific step context with chain tracking
			branchCtx := &Context{
				Logger:               stepCtx.Logger,
				Engine:               stepCtx.Engine,
				EventBus:             stepCtx.EventBus,
				Evaluator:            stepCtx.Evaluator.Clone(),
				Execution:            stepCtx.Execution,
				ChainTrackingEnabled: stepCtx.ChainTrackingEnabled,
				PreviousChainHead:    parallelChainHead, // Same for all parallel branches
			}

			// Evaluate branch step condition (Automation executor does this, but parallel branches
			// run via executors directly, so we must handle conditions here too).
			if strings.TrimSpace(branchCopy.Condition) != "" {
				if stepCtx.Logger != nil {
					stepCtx.Logger.Debug("evaluating step condition",
						"step", branchCopy.ID,
						"condition", branchCopy.Condition,
					)
				}
				shouldRun, err := branchCtx.Evaluator.EvaluateCondition(branchCopy.Condition)
				if err != nil {
					if stepCtx.Logger != nil {
						stepCtx.Logger.Warn("step condition evaluation failed",
							"step", branchCopy.ID,
							"condition", branchCopy.Condition,
							"error", err,
						)
					}
					shouldRun = false
				}
				if stepCtx.Logger != nil {
					stepCtx.Logger.Debug("step condition evaluated",
						"step", branchCopy.ID,
						"condition", branchCopy.Condition,
						"result", shouldRun,
					)
				}
				if !shouldRun {
					resultsChan <- branchResult{
						index: idx,
						result: &automations.StepResult{
							StepId:      branchCopy.ID,
							Status:      "skipped",
							StartedAt:   time.Now(),
							CompletedAt: time.Now(),
						},
						err: nil,
					}
					return
				}
			}

			// Execute the branch
			executor, ok := e.Registry.Get(branchStep.Type)
			if !ok {
				resultsChan <- branchResult{
					index: idx,
					result: &automations.StepResult{
						StepId:      branchStepId,
						Status:      "failed",
						Error:       fmt.Sprintf("unknown step type: %s", branchStep.Type),
						StartedAt:   time.Now(),
						CompletedAt: time.Now(),
					},
					err: fmt.Errorf("unknown step type: %s", branchStep.Type),
				}
				return
			}

			execResult, err := executor.Execute(execCtx, &branchCopy, branchCtx)

			// Compute chain linkage if tracking enabled
			if stepCtx.ChainTrackingEnabled && execResult != nil {
				execResult.PreviousChainHead = parallelChainHead // Same for all parallel
				execResult.ContentId = automations.ComputeBranchFingerprint(step.ID, branchStep.ID, execResult)
			}

			if err != nil && parallelCfg.FailFast {
				firstErrorMu.Lock()
				if firstError == nil {
					firstError = err
					cancel() // Cancel other branches
				}
				firstErrorMu.Unlock()
			}

			resultsChan <- branchResult{
				index:  idx,
				result: execResult,
				err:    err,
			}
		}(i, branch)
	}

	// Handle results based on wait strategy
	var childResults []*automations.StepResult
	var branchResults []any
	var errors []error
	collected := 0
	needed := len(branches)

	switch wait {
	case "none":
		// Fire and forget - don't wait
		go func() {
			wg.Wait()
			close(resultsChan)
		}()
		result.Status = "success"
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		result.Metadata = map[string]any{
			"branchCount": len(branches),
			"wait":        "none",
		}
		return result, nil

	case "any":
		// Wait for first successful result
		go func() {
			wg.Wait()
			close(resultsChan)
		}()

		for res := range resultsChan {
			childResults = append(childResults, res.result)
			if res.err == nil {
				// Got a successful result
				branchResults = append(branchResults, res.result.Result)
				result.Children = childResults
				result.Status = "success"
				result.Result = branchResults
				result.CompletedAt = time.Now()
				result.Duration = result.CompletedAt.Sub(result.StartedAt)
				result.Metadata = map[string]any{
					"branchCount": len(branches),
					"wait":        "any",
					"firstResult": res.index,
				}
				return result, nil
			}
			errors = append(errors, res.err)
		}
		// All failed
		result.Children = childResults
		result.Status = "failed"
		result.Error = fmt.Sprintf("all %d branches failed", len(errors))
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, errors[0]

	default: // "all"
		// Wait for all results
		go func() {
			wg.Wait()
			close(resultsChan)
		}()

		resultsMap := make(map[int]branchResult)
		for res := range resultsChan {
			resultsMap[res.index] = res
			collected++
			if res.err != nil {
				errors = append(errors, res.err)
			}
		}

		// Reassemble in order
		for i := 0; i < needed; i++ {
			if res, ok := resultsMap[i]; ok {
				childResults = append(childResults, res.result)
				if res.result != nil {
					branchResults = append(branchResults, res.result.Result)
				}
			}
		}
	}

	result.Children = childResults

	// Collect child fingerprints (sorted for determinism - parallel order is nondeterministic)
	if stepCtx.ChainTrackingEnabled {
		childFPs := make([]string, 0, len(childResults))
		for _, child := range childResults {
			if child != nil && child.ContentId != "" {
				childFPs = append(childFPs, child.ContentId)
			}
		}
		sort.Strings(childFPs)
		result.ChildFingerprints = childFPs
	}

	if len(errors) > 0 && parallelCfg.FailFast {
		result.Status = "failed"
		result.Error = fmt.Sprintf("%d branches failed", len(errors))
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, errors[0]
	}

	result.Status = "success"
	result.Result = branchResults
	result.CompletedAt = time.Now()
	result.Duration = result.CompletedAt.Sub(result.StartedAt)
	result.Metadata = map[string]any{
		"branchCount":  len(branches),
		"wait":         wait,
		"successCount": len(branchResults),
		"errorCount":   len(errors),
	}

	// Record step execution in the database
	runId := ""
	if stepCtx.Execution != nil {
		runId = stepCtx.Execution.ID
	}
	stepRecordQuery := RecordStepExecution(ctx, stepCtx.Engine, StepRecordData{
		RunId:     runId,
		StepId:    step.ID,
		StepType:  "parallel",
		Status:    result.Status,
		ItemCount: len(branchResults),
		Duration:  float64(result.Duration.Milliseconds()),
	})

	if stepCtx.Logger != nil {
		stepCtx.Logger.Debug("parallel step completed",
			"step", step.ID,
			"branches", len(branches),
			"errors", len(errors),
			"stepRecord", stepRecordQuery,
			"duration", formatDuration(result.Duration),
		)
	}

	return result, nil
}
