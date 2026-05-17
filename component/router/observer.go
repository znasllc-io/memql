package router

import (
	"context"
	"errors"
	"time"

	"github.com/znasllc-io/memql/core/common"
)

// observedStreamWithTools wraps a ChatStreamWithToolsProvider. It starts
// a timer on CallChatStreamWithTools, watches the delta channel for the
// first text chunk (time-to-first-token), accumulates output character
// count, and on stream close emits one CallRecord via the router.
type observedStreamWithTools struct {
	inner    common.ChatStreamWithToolsProvider
	router   *Router
	resolved Resolved
	req      ResolveRequest
}

// CallChatStreamWithTools implements common.ChatStreamWithToolsProvider.
// Token counts are Phase-1 estimates; see estimate.go.
func (o *observedStreamWithTools) CallChatStreamWithTools(
	ctx context.Context,
	messages []common.ChatMessage,
	tools []common.ToolDefinition,
) (<-chan common.StreamToolChunk, error) {
	start := time.Now()
	inputTokens := EstimateMessageTokens(messages)

	innerCh, err := o.inner.CallChatStreamWithTools(ctx, messages, tools)
	if err != nil {
		o.router.recordCall(buildRecord(o.req, o.resolved, inputTokens, 0, 0, start, time.Time{}, time.Now(), true, err, ctx.Err()))
		return nil, err
	}

	observedCh := make(chan common.StreamToolChunk)
	go func() {
		defer close(observedCh)

		var (
			firstTokenAt time.Time
			outputChars  int
			chunkErr     error
		)

		for chunk := range innerCh {
			if chunk.Error != nil && chunkErr == nil {
				chunkErr = chunk.Error
			}
			if firstTokenAt.IsZero() && chunk.Content != "" {
				firstTokenAt = time.Now()
			}
			outputChars += len(chunk.Content)
			// Tool-call argument fragments also count as output work.
			for _, tc := range chunk.ToolCalls {
				outputChars += len(tc.Arguments) + len(tc.Name)
			}
			select {
			case observedCh <- chunk:
			case <-ctx.Done():
				// Forward-drop remaining chunks if the consumer left; we
				// still record the call to capture cancellation.
				chunkErr = ctx.Err()
				return
			}
		}

		end := time.Now()
		outputTokens := EstimateTokensFromChars(outputChars)
		rec := buildRecord(o.req, o.resolved, inputTokens, outputTokens, 0, start, firstTokenAt, end, true, chunkErr, ctx.Err())
		// Tokens per second is only meaningful for streaming calls
		// with a non-trivial duration. Guard both so voice-path
		// one-second replies don't produce gigatokens/sec noise.
		durSec := end.Sub(start).Seconds()
		if outputTokens > 0 && durSec > 0.1 {
			rec.TokensPerSec = float64(outputTokens) / durSec
		}
		o.router.recordCall(rec)
	}()
	return observedCh, nil
}

// observedChat wraps a common.ChatSIProvider -- the non-streaming
// synchronous chat surface used by suggest endpoints and the voice
// (non-tool-calling) InvokeSI path.
type observedChat struct {
	inner    common.ChatSIProvider
	router   *Router
	resolved Resolved
	req      ResolveRequest
}

func (o *observedChat) CallChat(ctx context.Context, messages []common.ChatMessage) (string, error) {
	start := time.Now()
	inputTokens := EstimateMessageTokens(messages)

	reply, err := o.inner.CallChat(ctx, messages)
	outputTokens := EstimateTokensFromChars(len(reply))
	o.router.recordCall(buildRecord(o.req, o.resolved, inputTokens, outputTokens, 0, start, time.Time{}, time.Now(), false, err, ctx.Err()))
	return reply, err
}

// buildRecord assembles the CallRecord for the ledger given what the
// observer learned during the call. outcome is derived from err and
// ctx.Err(): a non-nil err wins, then ctx cancellation, otherwise "ok".
func buildRecord(
	req ResolveRequest,
	resolved Resolved,
	inputTokens, outputTokens, cachedInputTokens int,
	start, firstTokenAt, end time.Time,
	streaming bool,
	err error,
	ctxErr error,
) CallRecord {
	outcome := "ok"
	var errCat, errMsg string
	switch {
	case err != nil:
		if errors.Is(err, context.Canceled) || errors.Is(ctxErr, context.Canceled) {
			outcome = "cancelled"
		} else {
			outcome = "error"
		}
		errCat = CategorizeError(err)
		errMsg = TruncateError(err.Error(), 500)
	case ctxErr != nil:
		outcome = "cancelled"
		errCat = CategorizeError(ctxErr)
	}

	in, out, cached, total := resolved.Pricing.CostFor(inputTokens, outputTokens, cachedInputTokens)

	ttftMs := 0
	if !firstTokenAt.IsZero() {
		ttftMs = int(firstTokenAt.Sub(start).Milliseconds())
	}

	return CallRecord{
		RequestId:          req.RequestId,
		Partition:          req.Partition,
		AgentId:            req.AgentId,
		UserId:             req.UserId,
		SpaceId:            req.SpaceId,
		PromptName:         req.PromptName,
		Vendor:             resolved.Vendor,
		Model:              resolved.Model,
		ProviderName:       resolved.ProviderName,
		InputTokens:        inputTokens,
		OutputTokens:       outputTokens,
		CachedInputTokens:  cachedInputTokens,
		TokensEstimated:    true,
		InputCost:          in,
		OutputCost:         out,
		CachedInputCost:    cached,
		TotalCost:          total,
		PricingConfigured:  resolved.Pricing.Configured(),
		StartedAt:          start,
		TimeToFirstTokenMs: ttftMs,
		TotalDurationMs:    int(end.Sub(start).Milliseconds()),
		Streaming:          streaming,
		Outcome:            outcome,
		ErrorCategory:      errCat,
		ErrorMessage:       errMsg,
	}
}
