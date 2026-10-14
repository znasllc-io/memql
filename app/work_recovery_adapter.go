package app

import "context"

func (a *CognitionEngineAdapter) WorkModelCallBudget(ctx context.Context) (int, int, error) {
	return a.Engine.WorkModelCallBudget(ctx)
}
