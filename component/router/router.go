package router

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
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

// Router is the MemQL AI Router. It resolves a ResolveRequest to a
// provider (or a policy-driven fallback chain), wraps that provider
// with observability, and writes one v1:router:call row per call.
// Concurrent-safe; a single instance is shared across every node that
// makes AI calls.
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
		// EntryForUser, not Entry: a `fleet:<modelId>` entry resolves against
		// the ACTING USER'S machines (epic memql#4676), and resolving it
		// against the system catalog instead would report a live laptop as
		// unavailable -- which, with an authored @fallback, is a silent cloud
		// call for a user whose machine was awake the whole time.
		entry, ok := r.providers.EntryForUser(context.Background(), req.UserId, name)
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
	// A FLEET PRIMARY WITH NO WORKING ALTERNATIVE IS A REFUSAL, NOT AN ERROR
	// (epic memql#4676, design D2). The chain is exhausted, and if it started
	// with a fleet model the honest answer is "your machines cannot serve
	// this", carrying which machines were considered and why each was ruled
	// out -- not "the router resolved no provider", which describes a
	// registry lookup and tells an operator nothing they can act on.
	//
	// Reaching HERE is already the proof that no cloud fallback was authored:
	// had one been written into the policy it would have been in this chain
	// and, being available, would have been returned above. That is what makes
	// the no-silent-spend property structural rather than a rule somebody has
	// to remember -- there is no branch here that could choose a paid provider,
	// because choosing one would mean picking a name the policy never
	// mentioned.
	if refusal := r.fleetRefusalFor(req.UserId, chain); refusal != nil {
		// The one exception, and it is a person's decision rather than a
		// code path: the caller carries explicit consent to use a paid
		// provider for this call. The surface that set it showed the refusal
		// first and got a yes; nothing here can set it on its own.
		if req.CloudConsent {
			if client, resolved, ok := r.consentedCloudFallback(mod); ok {
				r.logger.Info("router: local model unavailable and the user consented to cloud for this call",
					"model", refusal.ModelId, "provider", resolved.ProviderName)
				_ = client
				return []string{resolved.ProviderName}, resolved, nil
			}
			// Consent given and nothing to spend it on. The refusal stands,
			// which is more useful than a generic "no provider": the person
			// said yes to something the cluster does not have.
			refusal.LastError = "cloud was approved for this call, but this cluster has no configured cloud provider"
		}
		return nil, Resolved{}, refusal
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
func (r *Router) providerLookup(ctx context.Context, userId, name string, mod providerModality) (any, Resolved, bool) {
	entry, ok := r.providers.EntryForUser(ctx, userId, name)
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

// buildRouterCallArgs assembles the recordRouterCall arg map for one
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
		"billing":            billingOrMetered(rec.Billing),
		"executionSurface":   rec.ExecutionSurface,
	}
}

// billingOrMetered normalizes a record's billing for the ledger. An
// empty value reads as metered, which keeps every writer that predates
// memql#4362 meaning exactly what it did -- and is the conservative
// direction besides: unattributed spend counts against the dollar
// ceiling rather than disappearing into the covered bucket.
func billingOrMetered(billing string) string {
	switch billing {
	case BillingSubscription, BillingLocal, BillingUnknown:
		return billing
	}
	return BillingMetered
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
	// A router with no engine cannot write the ledger, and that is a state
	// RecordsDropped's own doc anticipates ("engine unavailability"). Without
	// this check the detached goroutine dereferences nil and takes the WHOLE
	// PROCESS with it -- a panic on a goroutine nobody recovers is fatal, so
	// the failure mode of an unwired observability path was a crash rather
	// than a missing row.
	if r == nil || r.engine == nil {
		if r != nil {
			r.recordsDropped.Add(1)
		}
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ctx = auth.ContextWithToken(ctx, &auth.TokenInfo{
			Subject: "system:router",
			Claims:  map[string]any{"sub": "system:router"},
		})

		args := buildRouterCallArgs(rec, id.NewShortId())
		query, err := langparser.RenderCall("recordRouterCall", args)
		if err != nil {
			r.recordsDropped.Add(1)
			r.logger.Warn("router: rendering the record call failed", "error", err, "requestId", rec.RequestId)
			return
		}
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

// fleetRefusalFor builds the typed no_local_model_available refusal when an
// exhausted chain named a fleet model, or nil when it did not.
//
// It reports on the FIRST fleet entry in the chain, which is the one the
// operator wrote as the primary and therefore the one whose machines they
// want to hear about. A chain naming several fleet models is unusual; naming
// them all would bury the answer.
func (r *Router) fleetRefusalFor(userId string, chain []string) *memql.FleetUnavailable {
	if r == nil || r.providers == nil {
		return nil
	}
	for _, name := range chain {
		modelId, isFleet := memql.IsFleetReference(name)
		if !isFleet {
			continue
		}
		return r.providers.FleetRefusal(context.Background(), userId, modelId)
	}
	return nil
}

// consentedCloudFallback finds a paid provider to honour a one-shot consent.
//
// It takes the registry DEFAULT rather than scanning for anything that
// answers, because the default is the provider an operator configured as the
// cluster's ordinary choice -- and a consent that landed on whichever entry
// happened to sort first would be a different decision from the one the person
// thought they were making.
func (r *Router) consentedCloudFallback(mod providerModality) (any, Resolved, bool) {
	if r == nil || r.providers == nil {
		return nil, Resolved{}, false
	}
	name := strings.TrimSpace(r.providers.Default())
	if name == "" {
		return nil, Resolved{}, false
	}
	if _, isFleet := memql.IsFleetReference(name); isFleet {
		// A fully-local cluster's default is a fleet model, and consenting to
		// cloud cannot resolve to the same local model that was unavailable.
		return nil, Resolved{}, false
	}
	return r.providerLookup(context.Background(), "", name, mod)
}

// Providers exposes the registry this router resolves against, so a caller
// that already holds a Router can ask about provider availability without
// being handed a second reference to keep in sync.
func (r *Router) Providers() *memql.ProviderRegistry {
	if r == nil {
		return nil
	}
	return r.providers
}
