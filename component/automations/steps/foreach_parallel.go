package steps

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"

	"github.com/znasllc-io/memql/component/automations"
)

// A loop creates only its bounded worker set, never one goroutine per item.
// Each item retains its source index in journal keys and its own name frame.
// This bounds one invocation; machine and cluster admission remain separate.
func parallelForStatements(ctx context.Context, step *automations.Step, stepCtx *Context, items []any,
	result *automations.StepResult, finish func(error) (*automations.StepResult, error),
) (*automations.StepResult, error) {
	cfg := step.ForEach
	type outcome struct {
		processed bool
		fatal     bool
		err       error
	}
	outcomes := make([]outcome, len(items))
	workCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var admission sync.Mutex
	next := 0
	take := func() (int, bool) {
		admission.Lock()
		defer admission.Unlock()
		if workCtx.Err() != nil || next == len(items) {
			return 0, false
		}
		index := next
		next++
		return index, true
	}
	run := func(index int) outcome {
		iter := stepCtx.Evaluator.ChildFrame()
		iter.Bind(cfg.As, items[index])
		if step.Exprs.Filter != nil {
			keep, err := iter.EvalV1Condition(workCtx, step.Exprs.Filter)
			if err != nil {
				return outcome{fatal: true, err: fmt.Errorf("filter: %w", err)}
			}
			if !keep {
				return outcome{}
			}
		}
		returned, _, err := automations.RunStatementBody(workCtx, step.ID+"/"+strconv.Itoa(index), cfg.Do, stepCtx, iter)
		if returned && err == nil {
			// The authored compiler refuses this. Keep the runtime boundary
			// honest for a compiled definition supplied by another loader.
			err = fmt.Errorf("parallel loop iterations cannot return from their enclosing body")
		}
		return outcome{processed: err == nil, fatal: returned, err: err}
	}
	var workers sync.WaitGroup
	for range min(cfg.Concurrency, len(items)) {
		workers.Go(func() {
			for {
				index, ok := take()
				if !ok {
					return
				}
				out := run(index)
				outcomes[index] = out // each index belongs to exactly one worker
				if out.err != nil && (out.fatal || step.OnError != automations.ErrorStrategyContinue || isHumanWait(out.err) || errors.Is(out.err, automations.ErrJournalRequired)) {
					cancel(out.err)
				}
			}
		})
	}
	// No child may outlive its parent's receipt or a following stage.
	workers.Wait()
	processed, failed := 0, 0
	var lastErr error
	for _, out := range outcomes {
		if out.processed {
			processed++
		}
		if out.err != nil {
			failed++
			lastErr = out.err // deterministic source order, not completion order
		}
	}
	result.Metadata = map[string]any{"itemCount": len(items), "processedCount": processed}
	if failed > 0 {
		result.Metadata["failedCount"] = failed
		result.Metadata["lastError"] = lastErr.Error()
	}
	return finish(context.Cause(workCtx))
}
