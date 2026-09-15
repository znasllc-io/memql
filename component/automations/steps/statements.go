package steps

// statements.go -- the steps of an edition-2026 statement body that hold
// lists of their own (epic memql#5370, task memql#5372): a `for`, and a
// parallel's branches. Each runs its list through the automation's sequence
// runner (automations.RunStatementBody) in a frame of its own, so a name bound
// inside exists only inside, and a `return` inside ends the body the statement
// is in.

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/znasllc-io/memql/component/automations"
)

// BlockExecutor runs a block step: a parallel statement's branch, a list of
// statements in a scope of its own.
type BlockExecutor struct{}

func (e *BlockExecutor) Execute(ctx context.Context, step *automations.Step, stepCtx *Context) (*automations.StepResult, error) {
	result := &automations.StepResult{StepId: step.ID, StartedAt: time.Now()}
	finish := func(err error) (*automations.StepResult, error) {
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		if err != nil {
			result.Status, result.Error = "failed", err.Error()
			return result, err
		}
		result.Status = "success"
		return result, nil
	}
	if step.Block == nil {
		return finish(fmt.Errorf("block configuration is required"))
	}
	if stepCtx.Evaluator == nil || !stepCtx.Evaluator.InStatementBody() {
		return finish(fmt.Errorf("a block step runs only in a statement body"))
	}
	returned, value, err := automations.RunStatementBody(ctx, step.ID, step.Block.Steps, stepCtx, stepCtx.Evaluator.ChildFrame())
	if returned {
		result.Result = value
		result.Metadata = map[string]any{"returned": true}
	}
	return finish(err)
}

// forEachStatements runs a statement body's `for x in <source> [if <cond>]`:
// each item binds the loop variable in a frame of its own, the filter is read
// in that frame, and the body runs through the sequence runner. A failed
// iteration fails the loop unless the statement carries `on error continue`,
// which records it and goes on. A `return` inside ends the loop and the body
// around it: the result carries the returned value.
func forEachStatements(ctx context.Context, step *automations.Step, stepCtx *Context) (*automations.StepResult, error) {
	result := &automations.StepResult{StepId: step.ID, StartedAt: time.Now()}
	finish := func(err error) (*automations.StepResult, error) {
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		if err != nil {
			result.Status, result.Error = "failed", err.Error()
			return result, err
		}
		result.Status = "success"
		return result, nil
	}
	cfg := step.ForEach
	if cfg == nil || step.Exprs == nil || step.Exprs.Source == nil {
		return finish(fmt.Errorf("a statement `for` needs its source"))
	}
	source, err := v1Value(ctx, stepCtx.Evaluator, step.Exprs.Source)
	if err != nil {
		return finish(fmt.Errorf("failed to evaluate source: %w", err))
	}
	items, err := automations.ToSlice(source)
	if err != nil {
		return finish(fmt.Errorf("source is not iterable: %w", err))
	}
	processed, failed := 0, 0
	var lastErr error
	for i, item := range items {
		iter := stepCtx.Evaluator.ChildFrame()
		iter.Bind(cfg.As, item)
		if step.Exprs.Filter != nil {
			keep, ferr := iter.EvalV1Condition(ctx, step.Exprs.Filter)
			if ferr != nil {
				return finish(fmt.Errorf("filter: %w", ferr))
			}
			if !keep {
				continue
			}
		}
		// Keyed by the item's place in the source, so each iteration's keys
		// are its own.
		returned, value, rerr := automations.RunStatementBody(ctx, step.ID+"/"+strconv.Itoa(i), cfg.Do, stepCtx, iter)
		if rerr != nil {
			failed++
			lastErr = rerr
			if step.OnError != automations.ErrorStrategyContinue {
				result.Metadata = map[string]any{"itemCount": len(items), "processedCount": processed, "failedCount": failed}
				return finish(rerr)
			}
			continue
		}
		processed++
		if returned {
			result.Result = value
			result.Metadata = map[string]any{"itemCount": len(items), "processedCount": processed, "returned": true}
			return finish(nil)
		}
	}
	result.Metadata = map[string]any{"itemCount": len(items), "processedCount": processed}
	if failed > 0 {
		result.Metadata["failedCount"] = failed
		result.Metadata["lastError"] = lastErr.Error()
	}
	return finish(nil)
}
