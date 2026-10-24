package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

// Called only before tool dispatch: after bounded context recovery or a failed
// provider return. A capacity refusal suspends the exact exchange, rather than
// turning into a generic failure whose Retry restarts the current stage.
func (r *Replier) recoverModelCall(ctx context.Context, failure error, messages []common.ChatMessage, attempt int, emptyResponse bool) (handled bool, err error) {
	if failure == nil || (!work.IsModelCallCeiling(failure.Error()) && !work.IsContextCapacityFailure(failure.Error())) || !isOwnedWorkExecution(ctx) {
		return false, nil
	}
	if budget := r.workCallFailure(ctx); budget != nil {
		return true, r.pauseAtBudgetBoundary(ctx, budget, messages)
	}
	if err := ctx.Err(); err != nil {
		return true, err
	}
	runtime, ok := r.engine.(interface {
		RecoverWorkModelCall(context.Context, string, int, int, []common.ChatMessage) (bool, error)
	})
	if !ok {
		return true, fmt.Errorf("%w: model recovery is unavailable", memql.ErrWorkCheckpointRequired)
	}
	maximum := streamTransientMaxRetries
	if !emptyResponse || work.IsContextCapacityFailure(failure.Error()) {
		maximum = 0
	}
	retry, err := runtime.RecoverWorkModelCall(ctx, failure.Error(), attempt, maximum, closePendingCalls(messages))
	if err != nil {
		var wait *work.HumanWait
		if errors.As(err, &wait) {
			return true, err
		}
		if budget := r.workCallFailure(ctx); budget != nil {
			return true, r.pauseAtBudgetBoundary(ctx, budget, messages)
		}
		return true, fmt.Errorf("%w: %v", memql.ErrWorkCheckpointRequired, err)
	}
	if !retry || attempt >= maximum {
		return true, fmt.Errorf("%w: invalid provider retry decision", memql.ErrWorkCheckpointRequired)
	}
	timer := time.NewTimer(transientRetryBackoff(attempt + 1))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		if budget := r.workCallFailure(ctx); budget != nil {
			return true, r.pauseAtBudgetBoundary(ctx, budget, messages)
		}
		return true, ctx.Err()
	case <-timer.C:
		return true, nil
	}
}

// Compaction errors arrive before inference. Recognized capacity refusals use
// the same durable suspension as exhausted model calls; unrelated failures and
// run-budget refusals retain their separate failure/feedback paths.
func (r *Replier) pauseAfterContextRecovery(ctx context.Context, failure error, messages []common.ChatMessage) error {
	if handled, err := r.recoverModelCall(ctx, failure, messages, 0, false); handled {
		return err
	}
	return r.pauseAtBudgetBoundary(ctx, failure, messages)
}
