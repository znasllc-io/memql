package router

import (
	"context"
	"errors"
	"time"

	"github.com/znasllc-io/memql/core/common"
)

// observedStreamChat wraps a ChatStreamProvider. It starts
// a timer on CallChatStream, watches the delta channel for the
// first text chunk (time-to-first-token), accumulates output character
// count, and on stream close emits one CallRecord via the router.
type observedStreamChat struct {
	inner    common.ChatStreamProvider
	router   *Router
	resolved Resolved
	req      ResolveRequest
}

// CallChatStream implements common.ChatStreamProvider.
// Token counts are Phase-1 estimates; see estimate.go.
func (o *observedStreamChat) CallChatStream(
	ctx context.Context,
	messages []common.ChatMessage,
) (<-chan common.StreamChunk, error) {
	start := time.Now()
	ctx = startObservation(ctx, o.req, o.resolved, start)
	inputTokens := EstimateMessageTokens(messages)

	innerCh, err := o.inner.CallChatStream(ctx, messages)
	if err != nil {
		o.router.recordObserved(ctx, buildRecord(o.req, o.resolved, o.inner, inputTokens, 0, 0, start, time.Time{}, time.Now(), true, err, ctx.Err()))
		return nil, err
	}

	observedCh := make(chan common.StreamChunk)
	go func() {
		defer close(observedCh)
		var firstTokenAt time.Time
		var outputChars int
		var chunkErr error
		recorded, terminal := false, false
		record := func() {
			if recorded {
				return
			}
			recorded = true
			if !terminal && chunkErr == nil && ctx.Err() == nil {
				chunkErr = errors.New("provider stream closed before completion")
			}
			end := time.Now()
			outputTokens := EstimateTokensFromChars(outputChars)
			rec := buildRecord(o.req, o.resolved, o.inner, inputTokens, outputTokens, 0, start, firstTokenAt, end, true, chunkErr, ctx.Err())
			if duration := end.Sub(start).Seconds(); duration > 0.1 {
				rec.TokensPerSec = float64(outputTokens) / duration
			}
			o.router.recordObserved(ctx, rec)
		}
		defer record()
		for {
			var chunk common.StreamChunk
			var ok bool
			select {
			case <-ctx.Done():
				return
			case chunk, ok = <-innerCh:
				if !ok {
					return
				}
			}
			if chunk.Error != nil && chunkErr == nil {
				chunkErr = chunk.Error
			}
			if firstTokenAt.IsZero() && chunk.Content != "" {
				firstTokenAt = time.Now()
			}
			outputChars += len(chunk.Content)
			if chunk.Done || chunk.Error != nil {
				terminal = true
				record()
			}
			select {
			case observedCh <- chunk:
			case <-ctx.Done():
				return
			}
			if chunk.Done {
				return
			}
		}
	}()
	return observedCh, nil
}
