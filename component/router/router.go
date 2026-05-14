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

	"github.com/google/uuid"

	"github.com/visionarys-io/memql/component/auth"
	"github.com/visionarys-io/memql/component/memql"
	"github.com/visionarys-io/memql/core/common"
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

// ResolveChat picks a provider for a non-streaming synchronous chat call
// (suggest endpoints, voice-path InvokeSI turns) and returns the wrapped
// provider. Fallback chain semantics mirror ResolveStreamWithTools.
func (r *Router) ResolveChat(req ResolveRequest) (common.ChatSIProvider, Resolved, error) {
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
		if mod == modalityStreamTools {
			if _, ok := entry.Client.(common.ChatStreamWithToolsProvider); !ok {
				continue
			}
		} else {
			if _, ok := entry.Client.(common.ChatSIProvider); !ok {
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
	case modalityChat:
		if c, ok := entry.Client.(common.ChatSIProvider); ok {
			return c, resolved, true
		}
	}
	return nil, Resolved{}, false
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

		args := map[string]any{
			"requestId":          rec.RequestId,
			"agentId":            rec.AgentId,
			"userId":             rec.UserId,
			"spaceId":            rec.SpaceId,
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
		req.RequestId = uuid.NewString()
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
	case strings.Contains(lower, "google"), strings.Contains(lower, "gemini"):
		return "google"
	case strings.Contains(lower, "xai"), strings.Contains(lower, "grok"):
		return "xai"
	case strings.Contains(lower, "groq"):
		return "groq"
	case strings.Contains(lower, "mistral"):
		return "mistral"
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
