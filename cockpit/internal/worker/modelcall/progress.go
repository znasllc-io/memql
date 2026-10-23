package modelcall

import "context"

type runtimeProgressKey struct{}

// withRuntimeProgress carries a liveness signal, never the model's private
// reasoning. Runtime adapters report actual reasoning/tool output separately
// from answer deltas so the watchdog can distinguish work from a stalled stream.
func withRuntimeProgress(ctx context.Context, progress func()) context.Context {
	return context.WithValue(ctx, runtimeProgressKey{}, progress)
}

func reportRuntimeProgress(ctx context.Context) {
	if progress, ok := ctx.Value(runtimeProgressKey{}).(func()); ok {
		progress()
	}
}
