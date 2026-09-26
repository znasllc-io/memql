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
	ctx = startObservation(ctx, o.req, o.resolved, start)
	inputTokens := EstimateMessageTokens(messages)

	innerCh, err := o.inner.CallChatStreamWithTools(ctx, messages, tools)
	if err != nil {
		o.router.recordObserved(ctx, buildRecord(o.req, o.resolved, o.inner, inputTokens, 0, 0, start, time.Time{}, time.Now(), true, err, ctx.Err()))
		return nil, err
	}

	observedCh := make(chan common.StreamToolChunk)
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
			var chunk common.StreamToolChunk
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
			for _, tc := range chunk.ToolCalls {
				outputChars += len(tc.Arguments) + len(tc.Name)
			}
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

// observedWithTools wraps a common.ToolCallingChatAIProvider -- the
// non-streaming request/response tool-calling surface used by the
// background execution lane (memql#896). One call == one model step; the
// observer records a single non-streaming CallRecord (no time-to-first-
// token, since the whole response arrives at once). Output work counts
// both the assistant text and the requested tool-call argument bytes so
// the ledger's token estimate reflects a tool-heavy step, not just prose.
type observedWithTools struct {
	inner    common.ToolCallingChatAIProvider
	router   *Router
	resolved Resolved
	req      ResolveRequest
}

func (o *observedWithTools) CallChatWithTools(
	ctx context.Context,
	messages []common.ChatMessage,
	tools []common.ToolDefinition,
) (*common.ToolCallingChatResult, error) {
	start := time.Now()
	ctx = startObservation(ctx, o.req, o.resolved, start)
	inputTokens := EstimateMessageTokens(messages)

	result, err := o.inner.CallChatWithTools(ctx, messages, tools)

	outputChars := 0
	if result != nil {
		outputChars += len(result.AssistantText)
		for _, tc := range result.ToolCalls {
			outputChars += len(tc.Arguments) + len(tc.Name)
		}
	}
	outputTokens := EstimateTokensFromChars(outputChars)
	// streaming=false, firstTokenAt=zero: a synchronous call has no
	// meaningful TTFT, and buildRecord leaves timeToFirstTokenMs at 0.
	o.router.recordObserved(ctx, buildRecord(o.req, o.resolved, o.inner, inputTokens, outputTokens, 0, start, time.Time{}, time.Now(), false, err, ctx.Err()))
	return result, err
}

// observedChat wraps a common.ChatAIProvider -- the non-streaming
// synchronous chat surface used by suggest endpoints and the voice
// (non-tool-calling) InvokeAI path.
type observedChat struct {
	inner    common.ChatAIProvider
	router   *Router
	resolved Resolved
	req      ResolveRequest
}

func (o *observedChat) CallChat(ctx context.Context, messages []common.ChatMessage) (string, error) {
	start := time.Now()
	ctx = startObservation(ctx, o.req, o.resolved, start)
	inputTokens := EstimateMessageTokens(messages)

	reply, err := o.inner.CallChat(ctx, messages)
	outputTokens := EstimateTokensFromChars(len(reply))
	o.router.recordObserved(ctx, buildRecord(o.req, o.resolved, o.inner, inputTokens, outputTokens, 0, start, time.Time{}, time.Now(), false, err, ctx.Err()))
	return reply, err
}

// buildRecord assembles the CallRecord for the ledger given what the
// observer learned during the call. outcome is derived from err and
// ctx.Err(): a non-nil err wins, then ctx cancellation, otherwise "ok".
func buildRecord(
	req ResolveRequest,
	resolved Resolved,
	// inner is the provider that actually served the call, taken only to ask
	// it where the call RAN (execution_surface.go). It is deliberately `any`:
	// the four observers wrap four different provider interfaces, and the one
	// question asked of it here is satisfied structurally by whichever of them
	// can answer.
	inner any,
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

	servedModel, servedEffort := servedModelOf(inner)

	return CallRecord{
		RequestId:          req.RequestId,
		Partition:          req.Partition,
		AgentId:            req.AgentId,
		UserId:             req.UserId,
		CallerKind:         req.CallerKind,
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

		// THE DECISION travels with the row from the resolution that made it.
		// It is copied rather than re-derived: the router already answered
		// which rule decided and what it passed over, and a second answer
		// computed here from the provider name would be a guess that agrees
		// most of the time.
		PolicyName:       resolved.PolicyName,
		Level:            string(resolved.Decision.Level),
		RequestedLevel:   string(resolved.Decision.RequestedLevel),
		ServedLevel:      string(resolved.Decision.ServedLevel),
		Degraded:         resolved.Decision.Degraded,
		Rule:             resolved.Decision.Rule,
		Policy:           resolved.Decision.Policy,
		Door:             resolved.Decision.Door,
		Considered:       resolved.Decision.Considered,
		Touches:          resolved.Decision.Touches,
		MinContextTokens: resolved.Decision.MinContextTokens,
		// WHOSE MACHINE. Asked of the PROVIDER and falling back to the
		// resolution, in that order, because only the provider knows: the
		// router resolves a door and a model, and which machine in a fleet
		// answered is decided after that, by the fleet's own routing. The
		// resolution's value is kept as the fallback rather than dropped --
		// a future door that decides the machine up front would set it there,
		// and a provider that says nothing must not blank what it knew.
		MachineOwnerUserId: firstNonEmpty(machineOwnerOf(inner), resolved.Decision.MachineOwnerUserId),

		// WHERE IT RAN. See execution_surface.go: the field and the column
		// both predate this line, and nothing ever assigned it -- so every
		// decision row carried "" and the sharing ledger, which folds the
		// calls that ran on ONE machine, had nothing to fold on.
		ExecutionSurface: surfaceOf(inner),

		// WHAT ACTUALLY SERVED IT (design D9). Asked of the provider rather
		// than copied off the resolution: `resolved.Model` is what the chain
		// picked, and for an app door that is the door's own name. What the
		// app ran is the app's to report, and a surface that reports nothing
		// leaves both empty rather than being filled in from the request.
		ServedModel:  servedModel,
		ServedEffort: servedEffort,

		// WHO PAID. Empty stays empty and the row reads it as metered, which
		// is the conservative direction. A provider that knows -- an app door
		// or a session, both running on somebody's own subscription -- says so
		// rather than having it inferred from the surface string.
		Billing: billingOf(inner),
	}
}
