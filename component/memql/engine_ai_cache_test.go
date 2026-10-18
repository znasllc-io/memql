package memql

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStructuredCacheHitsRespectAndChargeTheOriginalRunCeiling(t *testing.T) {
	e, model, _ := overrideSeamEngine(t, aiCacheConfig{DefaultEnabled: true, MaxTTLSeconds: 300})
	guard := &contextReserveGuard{limit: 2}
	e.SetRunCeilingGuard(guard)
	invoke := func(ctx context.Context) (string, error) {
		return e.InvokeAIStructured(ctx, "draftReport", map[string]any{"week": "38"}, "draft", draftSchema, true)
	}
	for range 2 {
		_, err := invoke(stepCtx(nil))
		require.NoError(t, err)
	}
	require.Equal(t, 1, model.calls, "the second identical answer must actually come from cache")
	require.Equal(t, 2, guard.used, "both answered requests count against the loop ceiling")
	reply, err := invoke(stepCtx(nil))
	var ceiling *RunCeilingError
	require.ErrorAs(t, err, &ceiling)
	require.Empty(t, reply)
	require.Equal(t, 1, model.calls)
	ctx, cancel := context.WithCancel(stepCtx(nil))
	cancel()
	_, err = invoke(ctx)
	require.ErrorIs(t, err, context.Canceled)
}
