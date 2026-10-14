package memql

import (
	"context"
	"fmt"
)

type workContextRecoveryReserveKey struct{}

// ContextWithWorkContextRecoveryReserve carries a selected compaction policy
// within the owned execution. It never changes admission, usage or a ceiling.
func ContextWithWorkContextRecoveryReserve(ctx context.Context, calls int) context.Context {
	return context.WithValue(ctx, workContextRecoveryReserveKey{}, calls)
}

// WorkContextRecoveryReserve is the number of remaining calls compaction must
// leave untouched. Zero means no additional compaction preference was selected.
func WorkContextRecoveryReserve(ctx context.Context) int {
	calls, _ := ctx.Value(workContextRecoveryReserveKey{}).(int)
	return calls
}

func (e *MemQLEngine) workContextNeedsExactArchive(ctx context.Context) (bool, error) {
	reserve := WorkContextRecoveryReserve(ctx)
	if reserve < 0 {
		return false, fmt.Errorf("context recovery reserve must be nonnegative; history retained")
	}
	if reserve == 0 {
		return false, nil
	}
	limit, used, err := e.WorkModelCallBudget(ctx)
	if err != nil {
		return false, fmt.Errorf("context recovery budget is unreadable; history retained: %w", err)
	}
	if limit < 0 || used < 0 {
		return false, fmt.Errorf("context recovery budget is invalid; history retained")
	}
	return limit > 0 && limit-used <= reserve, nil
}
