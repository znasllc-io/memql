package router

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
)

// Engine is the narrow engine surface the router needs to write
// v1:router:call rows. Signature matches *memql.MemQLEngine.Execute so
// the concrete engine can be passed directly; no adapter required.
type Engine interface {
	Execute(ctx context.Context, query string) (*memql.ExecuteResult, error)
}

// Router is the memQL SI Router. It resolves a ResolveRequest to a
// provider (or a policy-driven fallback chain), wraps that provider
// with observability, and writes one v1:router:call row per call.
// Concurrent-safe; a single instance is shared across every node that
// makes SI calls.
type Router struct {
	providers *memql.ProviderRegistry
	policies  *memql.PolicyRegistry
	engine    Engine
	logger    *slog.Logger

	// recordsDropped tracks how many v1:router:call writes were
	// skipped because the engine was unavailable or returned an
	// error. Exposed for health metrics; not on the hot path.
	recordsDropped atomic.Uint64
}

// New constructs a Router. Provider registry is required; policies
// registry is optional (nil is treated as "no policies registered" and
// the router falls through to single-provider resolution). The engine
// is required for ledger writes. The logger may be nil.
func New(providers *memql.ProviderRegistry, policies *memql.PolicyRegistry, engine Engine, logger *slog.Logger) *Router {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Router{
		providers: providers,
		policies:  policies,
		engine:    engine,
		logger:    logger,
	}
}

// ResolveStreamWithTools picks a provider chain for a streaming
// chat-with-tools call and returns a wrapped provider. On pre-flight
// error the wrapper automatically advances down the fallback chain,
// recording each failed attempt with outcome="fallback_used".
func (r *Router) ResolveStreamWithTools(req ResolveRequest) (common.ChatStreamWithToolsProvider, Resolved, error) {
	chain, resolved, err := r.resolveChain(req, modalityStreamTools)
	if err != nil {
		return nil, Resolved{}, err
	}
	req = r.stampRequestId(req)
	resolved.Chain = chain
	return &fallbackStreamWithTools{
		router: r,
		chain:  chain,
		req:    req,
	}, resolved, nil
}

// ResolveWithTools picks a provider chain for a NON-streaming
// request/response chat-with-tools call -- the background / batch
// execution lane (memql#896). A planner-dispatched plan/task turn has no
// human watching tokens arrive, so it runs through the synchronous
// CallChatWithTools surface (one request, one response, one overall
// timeout) instead of the interactive streaming path + idle watchdog.
// Fallback chain semantics mirror ResolveStreamWithTools: a pre-flight
// error advances down the chain, recording each failed attempt as
// outcome="fallback_used".
func (r *Router) ResolveWithTools(req ResolveRequest) (common.ToolCallingChatAIProvider, Resolved, error) {
	chain, resolved, err := r.resolveChain(req, modalityTools)
	if err != nil {
		return nil, Resolved{}, err
	}
	req = r.stampRequestId(req)
	resolved.Chain = chain
	return &fallbackWithTools{
		router: r,
		chain:  chain,
		req:    req,
	}, resolved, nil
}

// ResolveChat picks a provider for a non-streaming synchronous chat call
// (suggest endpoints, voice-path InvokeAI turns) and returns the wrapped
// provider. Fallback chain semantics mirror ResolveStreamWithTools.
func (r *Router) ResolveChat(req ResolveRequest) (common.ChatAIProvider, Resolved, error) {
	chain, resolved, err := r.resolveChain(req, modalityChat)
	if err != nil {
		return nil, Resolved{}, err
	}
	req = r.stampRequestId(req)
	resolved.Chain = chain
	return &fallbackChat{
		router: r,
		chain:  chain,
		req:    req,
	}, resolved, nil
}

type providerModality int

const (
	modalityStreamTools providerModality = iota
	modalityChat
	// modalityTools is the non-streaming request/response tool-calling
	// surface (common.ToolCallingChatAIProvider) used by the background
	// execution lane (memql#896). Both the streaming providers
	// (anthropicStreamProvider / openAIStreamProvider) and the plain
	// non-streaming providers implement CallChatWithTools, so any chain
	// entry that serves modalityStreamTools also serves modalityTools.
	modalityTools
)

// resolveChain picks the ordered list of provider names to try for a
// request, along with metadata for the primary (first available)
// provider. The chain is then used by the fallback wrappers to walk
// down the list on error.
//
// Precedence:
//  1. ExplicitProvider  -- single-entry chain
//  2. PolicyName        -- policy's primary + fallbacks
//  3. DefaultProvider   -- single-entry chain
//  4. Registry default  -- single-entry chain (last resort)
func (r *Router) resolveChain(req ResolveRequest, mod providerModality) ([]string, Resolved, error) {
	var chain []string
	var policyName string

	switch {
	case strings.TrimSpace(req.ExplicitProvider) != "":
		chain = []string{strings.TrimSpace(req.ExplicitProvider)}
	case strings.TrimSpace(req.PolicyName) != "" && r.policies != nil:
		if policy, ok := r.policies.Lookup(req.PolicyName); ok {
			chain = policy.ProviderChain()
			policyName = policy.Name
		}
	}
	if len(chain) == 0 {
		if def := strings.TrimSpace(req.DefaultProvider); def != "" {
			chain = []string{def}
		} else if d := r.providers.Default(); d != "" {
			chain = []string{d}
		}
	}
	if len(chain) == 0 {
		return nil, Resolved{}, fmt.Errorf("router: no provider resolved (no explicit, no policy, no default)")
	}

	// Pick the first available provider in the chain to stamp as the
	// "primary" on the initial Resolved. The fallback wrapper may
	// advance past this if the primary errors pre-flight.
	for _, name := range chain {
		entry, ok := r.providers.Entry(name)
		if !ok || !entry.Available {
			continue
		}
		// Confirm interface support for the requested modality.
		switch mod {
		case modalityStreamTools:
			if _, ok := entry.Client.(common.ChatStreamWithToolsProvider); !ok {
				continue
			}
		case modalityTools:
			if _, ok := entry.Client.(common.ToolCallingChatAIProvider); !ok {
				continue
			}
		default:
			if _, ok := entry.Client.(common.ChatAIProvider); !ok {
				continue
			}
		}
		return chain, Resolved{
			ProviderName: entry.Config.Name,
			Vendor:       vendorFromType(entry.Config.Type),
			Model:        entry.Config.Model,
			Pricing:      entry.Config.Pricing(),
			Streaming:    mod == modalityStreamTools,
			PolicyName:   policyName,
		}, nil
	}
	// Every entry in the chain was unregistered, unavailable, or
	// lacked the requested modality -- e.g. a policy whose @primary +
	// every @fallback provider is @disabled. Name the policy (when the
	// chain came from one) so the empty-chain error is actionable.
	if policyName != "" {
		return nil, Resolved{}, fmt.Errorf("router: policy %q resolved no available provider; every entry in its chain %v is disabled/unavailable for the requested modality", policyName, chain)
	}
	return nil, Resolved{}, fmt.Errorf("router: no provider in chain %v is available for the requested modality", chain)
}

// providerLookup resolves a chain entry by name into its client +
// metadata + interface check for the requested modality. Returns
// (nil, zero, false) when the provider is unregistered, unavailable,
// or doesn't implement the interface.
func (r *Router) providerLookup(name string, mod providerModality) (any, Resolved, bool) {
	entry, ok := r.providers.Entry(name)
	if !ok || !entry.Available || entry.Client == nil {
		return nil, Resolved{}, false
	}
	resolved := Resolved{
		ProviderName: entry.Config.Name,
		Vendor:       vendorFromType(entry.Config.Type),
		Model:        entry.Config.Model,
		Pricing:      entry.Config.Pricing(),
		Streaming:    mod == modalityStreamTools,
	}
	switch mod {
	case modalityStreamTools:
		if c, ok := entry.Client.(common.ChatStreamWithToolsProvider); ok {
			return c, resolved, true
		}
	case modalityTools:
		if c, ok := entry.Client.(common.ToolCallingChatAIProvider); ok {
			return c, resolved, true
		}
	case modalityChat:
		if c, ok := entry.Client.(common.ChatAIProvider); ok {
			return c, resolved, true
		}
	}
	return nil, Resolved{}, false
}

// buildRouterCallArgs assembles the mutationRecordRouterCall arg map for one
// call. callId is the row's own shortId -- a freshly-minted bare slug, NOT the
// requestId. The requestId is a fully-qualified utterance id
// (v1:cognition:utterance:<uuid>); using it as the v1:router:call shortId is
// rejected by canonical-id validation (one concept's full id can't be another
// concept's shortId), which silently dropped every ledger write (memql#1244).
// The originating utterance id is preserved on the requestId field for
// correlation; minting a fresh id (vs. extracting the bare UUID) also avoids
// an id collision when one request fans out to a fallback call.
func buildRouterCallArgs(rec CallRecord, callId string) map[string]any {
	return map[string]any{
		"callId":             callId,
		"requestId":          rec.RequestId,
		"agentId":            rec.AgentId,
		"userId":             rec.UserId,
		"promptName":         rec.PromptName,
		"policyName":         rec.PolicyName,
		"vendor":             rec.Vendor,
		"model":              rec.Model,
		"providerName":       rec.ProviderName,
		"inputTokens":        rec.InputTokens,
		"outputTokens":       rec.OutputTokens,
		"cachedInputTokens":  rec.CachedInputTokens,
		"tokensEstimated":    rec.TokensEstimated,
		"inputCost":          rec.InputCost,
		"outputCost":         rec.OutputCost,
		"cachedInputCost":    rec.CachedInputCost,
		"totalCost":          rec.TotalCost,
		"pricingConfigured":  rec.PricingConfigured,
		"timeToFirstTokenMs": rec.TimeToFirstTokenMs,
		"totalDurationMs":    rec.TotalDurationMs,
		"tokensPerSec":       rec.TokensPerSec,
		"streaming":          rec.Streaming,
		"outcome":            rec.Outcome,
		"errorCategory":      rec.ErrorCategory,
		"errorMessage":       rec.ErrorMessage,
		"fallbackFromModel":  rec.FallbackFromModel,
	}
}

// recordCall writes one v1:router:call row via the router mutation.
// Fire-and-forget: runs on a detached goroutine with a fresh context so
// the caller's cancellation never interrupts observability. A failed
// write is logged and counted; it does not propagate.
//
// The ledger mutation goes through `insert()` which requires an actor
// (see component/memql/executor.go:mutationActor). We stamp a
// synthetic "system:router" principal onto the detached context so
// the write succeeds -- the alternative was every call emitting a
// "no actor found in context" warning on every turn.
func (r *Router) recordCall(rec CallRecord) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ctx = auth.ContextWithToken(ctx, &auth.TokenInfo{
			Subject: "system:router",
			Claims:  map[string]any{"sub": "system:router"},
		})

		args := buildRouterCallArgs(rec, id.NewShortId())
		payload, err := json.Marshal(args)
		if err != nil {
			r.recordsDropped.Add(1)
			r.logger.Warn("router: marshal record args failed", "error", err, "requestId", rec.RequestId)
			return
		}
		query := fmt.Sprintf("mutationRecordRouterCall(%s)", string(payload))
		if _, err := r.engine.Execute(ctx, query); err != nil {
			r.recordsDropped.Add(1)
			r.logger.Warn("router: failed to write v1:router:call row",
				"error", err,
				"requestId", rec.RequestId,
				"provider", rec.ProviderName,
				"outcome", rec.Outcome,
			)
		}
	}()
}

// RecordsDropped returns the cumulative count of ledger writes the router
// has skipped due to engine unavailability or marshal errors. Exposed
// for health endpoints and tests.
func (r *Router) RecordsDropped() uint64 {
	return r.recordsDropped.Load()
}

func (r *Router) stampRequestId(req ResolveRequest) ResolveRequest {
	if req.RequestId == "" {
		req.RequestId = id.NewShortId()
	}
	return req
}

// vendorFromType maps a provider .memql @type to a vendor family name.
// Kept inline rather than as a table so new vendors only need a case
// here when we add their client.
func vendorFromType(typeName string) string {
	lower := strings.ToLower(strings.TrimSpace(typeName))
	switch {
	case strings.Contains(lower, "openai"):
		return "openai"
	case strings.Contains(lower, "anthropic"):
		return "anthropic"
	default:
		return lower
	}
}

// firstNonEmpty returns the first non-empty trimmed string, or "".
// Used by callers that want to compose a precedence without nested ifs.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
