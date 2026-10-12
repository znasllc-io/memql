package memql

import (
	"context"
	"sync/atomic"
)

type countCompletenessKey struct{}
type countCompleteness struct{ truncated atomic.Bool }

// Count completeness is a call-local observation of every source window, before
// latest-version, staging and authorization folds can shorten the result. A
// short final set is not evidence that the source scan was exhausted.
func noteCountWindow(ctx context.Context, size, limit int) {
	if limit > 0 && size >= limit {
		if state, ok := ctx.Value(countCompletenessKey{}).(*countCompleteness); ok {
			state.truncated.Store(true)
		}
	}
}
