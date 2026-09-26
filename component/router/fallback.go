package router

import (
	"context"
	"errors"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

// fallbackStreamWithTools walks a provider chain on CallChatStreamWithTools.
// Pre-flight error on a chain entry -> record outcome="fallback_used" +
// advance to the next entry. The successful entry is wrapped with an
// observedStreamWithTools, which handles normal end-of-stream recording.
//
// A typed FleetUnavailable may arrive as the first error chunk: fleet calls
// dispatch asynchronously, so their machine eligibility check runs after this
// method returns. It is safe to retry only before content or tool output. Raw
// runtime errors and errors after output never replay a started generation.
type fallbackStreamWithTools struct {
	router   *Router
	chain    []string
	req      ResolveRequest
	resolved Resolved
}

func (f *fallbackStreamWithTools) CallChatStreamWithTools(
	ctx context.Context,
	messages []common.ChatMessage,
	tools []common.ToolDefinition,
) (<-chan common.StreamToolChunk, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var lastErr error
	var lastFailedResolved Resolved

	for i, name := range f.chain {
		client, resolved, ok := f.router.providerLookup(ctx, f.req, name, modalityStreamTools)
		if !ok {
			continue
		}
		resolved = resolved.withDecisionFrom(f.resolved)
		inner := client.(common.ChatStreamWithToolsProvider)

		// If the previous attempt failed pre-flight, that failure was
		// recorded as an "error" row by the observer. Append a
		// fallback_used row attributed to the failed provider so the
		// dashboard shows the retry trail with the provider that
		// failed identified.
		if lastErr != nil {
			f.router.recordObserved(ctx, fallbackRecord(f.req, lastFailedResolved, lastErr))
		}

		observed := &observedStreamWithTools{
			inner:    inner,
			router:   f.router,
			resolved: resolved,
			req:      f.req,
		}
		ch, err := observed.CallChatStreamWithTools(ctx, messages, tools)
		if err == nil {
			return f.retryUnstartedStream(ctx, ch, messages, tools, i+1, resolved), nil
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
func (f *fallbackStreamWithTools) retryUnstartedStream(
	ctx context.Context, stream <-chan common.StreamToolChunk,
	messages []common.ChatMessage, tools []common.ToolDefinition,
	next int, failed Resolved,
) <-chan common.StreamToolChunk {
	out := make(chan common.StreamToolChunk)
	go func() {
		defer close(out)
		emitted := false
		for {
			var chunk common.StreamToolChunk
			var ok bool
			select {
			case <-ctx.Done():
				return
			case chunk, ok = <-stream:
				if !ok {
					return
				}
			}
			emitted = emitted || chunk.Content != "" || len(chunk.ToolCalls) > 0
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
				retry, err := remaining.CallChatStreamWithTools(ctx, messages, tools)
				if err != nil {
					if errors.Is(err, errNoChainEntryAvailable) {
						err = chunk.Error
					}
					select {
					case out <- common.StreamToolChunk{Error: err, Done: true}:
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

// fallbackWithTools walks a provider chain on CallChatWithTools -- the
// non-streaming request/response tool-calling surface used by the
// background execution lane (memql#896). Mirrors fallbackStreamWithTools:
// a pre-flight error on a chain entry records outcome="fallback_used" and
// advances to the next entry; the successful entry is wrapped with an
// observedWithTools that records the terminal CallRecord. There is no
// mid-stream concept here -- the call either returns a result or an error.
type fallbackWithTools struct {
	router   *Router
	chain    []string
	req      ResolveRequest
	resolved Resolved
}

func (f *fallbackWithTools) CallChatWithTools(
	ctx context.Context,
	messages []common.ChatMessage,
	tools []common.ToolDefinition,
) (*common.ToolCallingChatResult, error) {
	var lastErr error
	var lastFailedResolved Resolved

	for _, name := range f.chain {
		client, resolved, ok := f.router.providerLookup(ctx, f.req, name, modalityTools)
		if !ok {
			continue
		}
		resolved = resolved.withDecisionFrom(f.resolved)
		inner := client.(common.ToolCallingChatAIProvider)

		if lastErr != nil {
			f.router.recordObserved(ctx, fallbackRecord(f.req, lastFailedResolved, lastErr))
		}

		observed := &observedWithTools{
			inner:    inner,
			router:   f.router,
			resolved: resolved,
			req:      f.req,
		}
		result, err := observed.CallChatWithTools(ctx, messages, tools)
		if err == nil {
			return result, nil
		}
		lastErr = err
		lastFailedResolved = resolved
	}

	if lastErr != nil {
		return nil, lastErr
	}
	return nil, errNoChainEntryAvailable
}

// fallbackChat mirrors fallbackStreamWithTools for the non-streaming
// synchronous ChatAIProvider path.
type fallbackChat struct {
	router   *Router
	chain    []string
	req      ResolveRequest
	resolved Resolved
}

func (f *fallbackChat) CallChat(ctx context.Context, messages []common.ChatMessage) (string, error) {
	var lastErr error
	var lastFailedResolved Resolved

	for _, name := range f.chain {
		client, resolved, ok := f.router.providerLookup(ctx, f.req, name, modalityChat)
		if !ok {
			continue
		}
		resolved = resolved.withDecisionFrom(f.resolved)
		inner := client.(common.ChatAIProvider)

		if lastErr != nil {
			f.router.recordObserved(ctx, fallbackRecord(f.req, lastFailedResolved, lastErr))
		}

		observed := &observedChat{
			inner:    inner,
			router:   f.router,
			resolved: resolved,
			req:      f.req,
		}
		reply, err := observed.CallChat(ctx, messages)
		if err == nil {
			return reply, nil
		}
		lastErr = err
		lastFailedResolved = resolved
	}

	if lastErr != nil {
		return "", lastErr
	}
	return "", errNoChainEntryAvailable
}

// fallbackRecord builds a CallRecord for a failed pre-flight attempt
// that was retried via the fallback chain. These rows appear in the
// ledger so operators can see the retry path, attributed to the
// provider that failed rather than the one that eventually served the
// reply.
func fallbackRecord(req ResolveRequest, failedResolved Resolved, err error) CallRecord {
	now := time.Now()
	return CallRecord{
		RequestId:         req.RequestId,
		Partition:         req.Partition,
		AgentId:           req.AgentId,
		UserId:            req.UserId,
		CallerKind:        req.CallerKind,
		PromptName:        req.PromptName,
		Vendor:            failedResolved.Vendor,
		Model:             failedResolved.Model,
		ProviderName:      failedResolved.ProviderName,
		TokensEstimated:   true,
		PricingConfigured: failedResolved.Pricing.Configured(),
		StartedAt:         now,
		TotalDurationMs:   0,
		Streaming:         failedResolved.Streaming,
		Outcome:           "fallback_used",
		ErrorCategory:     CategorizeError(err),
		ErrorMessage:      TruncateError(errOrString(err), 500),
		FallbackFromModel: failedResolved.Model,

		// Every attempt keeps the rule/level decision that chose this chain;
		// its resolved metadata names the provider and door actually tried.
		PolicyName:         failedResolved.PolicyName,
		Level:              string(failedResolved.Decision.Level),
		RequestedLevel:     string(failedResolved.Decision.RequestedLevel),
		ServedLevel:        string(failedResolved.Decision.ServedLevel),
		Degraded:           failedResolved.Decision.Degraded,
		Rule:               failedResolved.Decision.Rule,
		Policy:             failedResolved.Decision.Policy,
		Door:               failedResolved.Decision.Door,
		Considered:         failedResolved.Decision.Considered,
		Touches:            failedResolved.Decision.Touches,
		MinContextTokens:   failedResolved.Decision.MinContextTokens,
		MachineOwnerUserId: failedResolved.Decision.MachineOwnerUserId,
	}
}

func errOrString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// errNoChainEntryAvailable is returned when every provider in the
// resolution chain was unregistered or unavailable for the requested
// modality. Distinct from an upstream error so callers can surface a
// clearer message.
var errNoChainEntryAvailable = &chainUnavailableError{}

type chainUnavailableError struct{}

func (*chainUnavailableError) Error() string {
	return "router: no provider in the resolution chain is currently available for this modality"
}

// A fresh provider lookup supplies this attempt's client, model and pricing;
// it cannot recreate the rule decision that produced the fallback chain.
// Preserve that decision for every observer and retry record, while naming
// the door of the provider actually attempted (which may differ on fallback).
//
// THE ATTEMPT'S DOOR COMES FROM THE LOOKUP, NOT FROM THE NAME, and since epic
// memql#5391 it cannot come from the name: `app:claude-code` is the same entry
// for two different doors. It serves a chat turn as `app` and takes a whole
// step as a `session`, and only the resolution knows which happened. Deriving
// it here would quietly record every session as an ordinary app turn -- which
// is what this did, and what the ledger test now catches.
func (resolved Resolved) withDecisionFrom(selection Resolved) Resolved {
	attemptDoor := resolved.Decision.Door
	resolved.PolicyName = selection.PolicyName
	resolved.Decision = selection.Decision
	resolved.Decision.Door = attemptDoor
	if resolved.ProviderName != selection.ProviderName {
		// Each call can walk the same wrapper concurrently. Copy before
		// appending so neither another call nor the caller's resolution changes.
		resolved.Decision.Considered = append([]airoute.ConsideredEntry(nil), selection.Decision.Considered...)
		resolved.Decision.Considered = append(resolved.Decision.Considered, airoute.ConsideredEntry{
			Entry: resolved.ProviderName, Door: resolved.Decision.Door, Reason: "selected from fallback chain",
		})
		// The original machine's owner is not evidence about a new attempt.
		resolved.Decision.MachineOwnerUserId = ""
	}
	return resolved
}
