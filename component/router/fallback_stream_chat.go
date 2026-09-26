package router

import (
	"context"
	"errors"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

// fallbackStreamChat walks a provider chain on CallChatStream.
// Pre-flight error on a chain entry -> record outcome="fallback_used" +
// advance to the next entry. The successful entry is wrapped with an
// observedStreamChat, which handles normal end-of-stream recording.
//
// A typed FleetUnavailable may arrive as the first error chunk: fleet calls
// dispatch asynchronously, so their machine eligibility check runs after this
// method returns. It is safe to retry only before content. Raw
// runtime errors and errors after output never replay a started generation.
type fallbackStreamChat struct {
	router   *Router
	chain    []string
	req      ResolveRequest
	resolved Resolved
}

func (f *fallbackStreamChat) CallChatStream(
	ctx context.Context,
	messages []common.ChatMessage,
) (<-chan common.StreamChunk, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var lastErr error
	var lastFailedResolved Resolved

	for i, name := range f.chain {
		client, resolved, ok := f.router.providerLookup(ctx, f.req, name, modalityStreamChat)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !ok {
			continue
		}
		resolved = resolved.withDecisionFrom(f.resolved)
		inner := client.(common.ChatStreamProvider)

		// If the previous attempt failed pre-flight, that failure was
		// recorded as an "error" row by the observer. Append a
		// fallback_used row attributed to the failed provider so the
		// dashboard shows the retry trail with the provider that
		// failed identified.
		if lastErr != nil {
			f.router.recordObserved(ctx, fallbackRecord(f.req, lastFailedResolved, lastErr))
		}

		observed := &observedStreamChat{
			inner:    inner,
			router:   f.router,
			resolved: resolved,
			req:      f.req,
		}
		ch, err := observed.CallChatStream(ctx, messages)
		if err == nil {
			return f.retryUnstartedStream(ctx, ch, messages, i+1, resolved), nil
		}
		lastErr = err
		lastFailedResolved = resolved
	}

	if lastErr != nil {
		return nil, lastErr
	}
	return nil, errNoChainEntryAvailable
}

// retryUnstartedStream relays without buffering model output. The concrete
// FleetUnavailable type proves no machine started; the broader unavailable
// sentinel is insufficient because it may wrap other runtime failures.
func (f *fallbackStreamChat) retryUnstartedStream(
	ctx context.Context, stream <-chan common.StreamChunk,
	messages []common.ChatMessage,
	next int, failed Resolved,
) <-chan common.StreamChunk {
	out := make(chan common.StreamChunk)
	go func() {
		defer close(out)
		emitted := false
		for {
			var chunk common.StreamChunk
			var ok bool
			select {
			case <-ctx.Done():
				return
			case chunk, ok = <-stream:
				if !ok {
					return
				}
			}
			emitted = emitted || chunk.Content != ""
			var unavailable *memql.FleetUnavailable
			if !emitted && next < len(f.chain) && errors.As(chunk.Error, &unavailable) {
				// Let the observer finish recording this refusal before advancing.
				// A fleet refusal closes its stream without starting any generation.
				for stream != nil {
					select {
					case <-ctx.Done():
						return
					case _, ok := <-stream:
						if !ok {
							stream = nil
						}
					}
				}
				if ctx.Err() != nil {
					return
				}
				f.router.recordObserved(ctx, fallbackRecord(f.req, failed, chunk.Error))
				remaining := *f
				remaining.chain = f.chain[next:]
				retry, err := remaining.CallChatStream(ctx, messages)
				if err != nil {
					if errors.Is(err, errNoChainEntryAvailable) {
						err = chunk.Error
					}
					select {
					case out <- common.StreamChunk{Error: err, Done: true}:
					case <-ctx.Done():
					}
					return
				}
				for c := range retry {
					select {
					case out <- c:
					case <-ctx.Done():
						return
					}
				}
				return
			}
			select {
			case out <- chunk:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

func (r *Router) resolveStreamChat(ctx context.Context, req ResolveRequest) (common.ChatStreamProvider, Resolved, error) {
	chain, resolved, err := r.resolveChain(ctx, req, modalityStreamChat)
	if err != nil {
		return nil, Resolved{}, err
	}
	req = r.stampRequestId(req)
	resolved.Chain = chain
	return &fallbackStreamChat{router: r, chain: chain, req: req, resolved: resolved}, resolved, nil
}
