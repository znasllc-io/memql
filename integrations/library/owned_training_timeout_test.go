package library

import (
	"context"
	"errors"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestOptionalTrainingVectorBoundsOnlyItsOwnWait(t *testing.T) {
	block := func(ctx context.Context) ([]float32, error) { <-ctx.Done(); return nil, ctx.Err() }
	ctx := context.Background()
	_, unavailable, err := trainingVectorWithin(ctx, 10*time.Millisecond, block)
	require.NoError(t, err)
	require.True(t, unavailable)
	require.NoError(t, ctx.Err())
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, unavailable, err = trainingVectorWithin(cancelled, time.Second, block)
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, unavailable)
	fault := errors.New("provider refused")
	_, unavailable, err = trainingVectorWithin(ctx, time.Second, func(context.Context) ([]float32, error) { return nil, fault })
	require.ErrorIs(t, err, fault)
	require.False(t, unavailable)
	vector, unavailable, err := trainingVectorWithin(ctx, time.Second, func(context.Context) ([]float32, error) { return []float32{1, 2}, nil })
	require.NoError(t, err)
	require.False(t, unavailable)
	require.Equal(t, []float32{1, 2}, vector)
}
