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

// Called only on a provider-return error before any tool from that response
// executes. A partial stream may be checkpointed, but never retried invisibly.
func (r *Replier) recoverModelCeiling(ctx context.Context, failure error, messages []common.ChatMessage, attempt int, emptyResponse bool) (handled bool, err error) {
	if failure == nil || !work.IsModelCallCeiling(failure.Error()) || !isOwnedWorkExecution(ctx) {
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
	if !emptyResponse {
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
