package memql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/core/common"
)

type aiRuntime struct {
	logger    *slog.Logger
	prompts   *PromptRegistry
	providers *ProviderRegistry
	cache     *aiResponseCache
	cacheCfg  aiCacheConfig
	eventBus  *events.Bus

	// seam is the shared model-call journal seam (memql#4999), the same
	// value the engine holds. Nil-safe: a runtime built without one calls
	// straight through.
	seam *modelSeam

	// semantic is the optional vector (similarity) cache layer (5.9). It
	// sits AFTER the exact-hash cache: exact-hash check first (cheap),
	// then a semantic nearest-neighbour lookup on exact-miss for ENABLED
	// namespaces only. nil until SetSemanticCache wires an embedder + store;
	// when nil (or for a disabled/unset namespace) the AI path is exactly
	// the pre-5.9 exact-hash/fresh behaviour.
	semantic *semanticAICache
}

func newAIRuntime(logger *slog.Logger, prompts *PromptRegistry, providers *ProviderRegistry, cfg aiCacheConfig) *aiRuntime {
	if prompts == nil && providers == nil {
		return nil
	}
	return &aiRuntime{
		logger:    logger,
		prompts:   prompts,
		providers: providers,
		cache:     aiResponseCacheWithMetrics(cfg.MaxEntries),
		cacheCfg:  cfg,
	}
}

// SetEventBus wires the event bus for AI completion events.
func (r *aiRuntime) SetEventBus(bus *events.Bus) {
	if r == nil {
		return
	}
	r.eventBus = bus
}

// SetSemanticCache wires the optional semantic (vector) cache layer. Safe to
// pass nil (leaves the runtime exact-hash-only). Called from engine bootstrap
// once the embedding provider + DB getter are available.
func (r *aiRuntime) SetSemanticCache(s *semanticAICache) {
	if r == nil {
		return
	}
	r.semantic = s
}

// publishEvent emits an event to the event bus if configured.
func (r *aiRuntime) publishEvent(topic string, kind events.Kind, payload map[string]any) {
	if r == nil || r.eventBus == nil {
		return
	}
	event := events.NewEvent(topic, kind, payload)
	r.eventBus.Publish(event)
}

func (r *aiRuntime) Invoke(ctx context.Context, invocation *AIInvocation, data any) (any, error) {
	startTime := time.Now()

	if invocation == nil {
		return nil, fmt.Errorf("ai invocation is not defined")
	}
	if r == nil || r.prompts == nil {
		return nil, fmt.Errorf("ai() runtime is not configured")
	}

	prompt, err := r.resolvePrompt(invocation.TemplateId)
	if err != nil {
		return nil, err
	}

	payload, err := normalizeAIData(data)
	if err != nil {
		return nil, fmt.Errorf("data for prompt %q invalid: %w", prompt.Name, err)
	}
	if err := prompt.ValidateData(payload); err != nil {
		return nil, fmt.Errorf("data for prompt %q invalid: %w", prompt.Name, err)
	}

	text, err := prompt.Render(payload)
	if err != nil {
		return nil, fmt.Errorf("executing prompt template %q: %w", prompt.Name, err)
	}

	providerName, err := r.resolveProviderName(prompt, invocation)
	if err != nil {
		return nil, err
	}

	// EntryForContext, not Entry: a `fleet:<modelId>` provider resolves
	// against the ACTING USER'S machines (epic memql#4676), and the caller's
	// actor is on ctx. Resolving it context-free would answer for the
	// shared-inference set instead -- reporting a user's own awake laptop as
	// unavailable.
	entry, ok := r.providers.EntryForContext(ctx, providerName)
	if !ok || entry == nil || !entry.Available || entry.Client == nil {
		// A FLEET provider that is unavailable is the TYPED refusal, not a
		// generic one. The difference is the whole of design D2 on this path:
		// a planner Task PARKS on `no_local_model_available` and resumes when
		// a machine wakes, where a generic error FAILS the plan and makes the
		// user start over for a laptop that was merely asleep. errors.As is
		// what the planner matches on, so the shape has to survive here.
		if modelId, isFleet := IsFleetReference(providerName); isFleet {
			return nil, r.providers.FleetRefusal(ctx, actingUserFromContext(ctx), modelId)
		}
		return nil, ErrProviderUnavailable(providerName)
	}

	// Validate provider supports text modality for ai() expressions
	if !entry.Config.SupportsText() {
		return nil, fmt.Errorf("provider %q (modality: %s) cannot be used in ai() expressions; only text-based providers are supported",
			providerName, entry.Config.ResolvedModality())
	}

	ttl := r.cacheTTL(invocation)
	var cacheKey string
	if ttl > 0 && r.cache != nil {
		cacheKey = buildAICacheKey(invocation.TemplateId, providerName, text)
		if cached, ok := r.cache.get(cacheKey); ok {
			// Emit completion finished event for cached result
			r.publishEvent(events.TopicAICompletionFinished, events.KindAICompletionFinished, map[string]any{
				"templateId": invocation.TemplateId,
				"provider":   providerName,
				"durationMs": time.Since(startTime).Milliseconds(),
				"cached":     true,
			})
			return cached, nil
		}
	}

	// Semantic (vector) cache lookup -- AFTER the exact-hash miss above, and
	// only for an invocation that opted into an ENABLED classification
	// namespace. A near-duplicate prompt ("thanks!" vs "thanks") that the
	// exact-hash cache missed can still hit here, gated by the namespace's
	// similarity threshold (the wrong-answer guard). Disabled/unset
	// namespaces skip this entirely (no embed call, no behaviour change).
	namespace := strings.TrimSpace(invocation.SemanticNamespace)
	semanticEnabled := namespace != "" && r.semantic.Enabled(namespace)
	if semanticEnabled {
		if cached, ok := r.semantic.Lookup(ctx, namespace, text); ok {
			r.publishEvent(events.TopicAICompletionFinished, events.KindAICompletionFinished, map[string]any{
				"templateId": invocation.TemplateId,
				"provider":   providerName,
				"durationMs": time.Since(startTime).Milliseconds(),
				"cached":     true,
				"cacheKind":  "semantic",
			})
			return cached, nil
		}
	}

	// Emit completion started event
	r.publishEvent(events.TopicAICompletionStarted, events.KindAICompletionStarted, map[string]any{
		"templateId": invocation.TemplateId,
		"provider":   providerName,
	})

	// THE JOURNAL SEAM (memql#4999). This is how a WORK RUN reaches a model:
	// a DSL `ai(...)` inside an automation step, through the step registry and
	// the shape evaluator, neither of which knows what a run is. The seam
	// reads the run off the context, asks work.DecideServe once, and either
	// serves the journaled answer without calling the provider at all or
	// calls through and records what came back.
	//
	// IT SITS BELOW BOTH CACHES DELIBERATELY. The exact-hash and semantic
	// caches above are a within-process optimisation and a cache hit is not a
	// model call; journaling one would record a call that never happened, and
	// serving a replay from a cache that may be cold is exactly the
	// nondeterminism a replay exists to remove.
	//
	// `fleet:` providers answer on the user's own machine and MemQL is not
	// billed, which is what `served: "local"` records -- the scorecard counts
	// subscription and local spend separately from the dollar ceiling.
	_, isFleet := IsFleetReference(providerName)
	req := common.ModelRequest{
		Provider: providerName,
		Model:    entry.Config.Model,
		Settings: answerAffectingParams(entry.Config.Params),
		Messages: []common.ChatMessage{{Role: "user", Content: text}},
	}
	result, err := r.seam.serve(ctx, req, invocation.TemplateId, func(ctx context.Context) (modelCallOutcome, error) {
		v, callErr := entry.Client.Call(ctx, text)
		return modelCallOutcome{Value: v, Local: isFleet}, callErr
	})
	if err != nil {
		// A strict replay that could not be served is NOT an ai() failure and
		// must not be reported as one: the provider was never asked, and
		// wrapping it here would bury the step key the divergence names.
		var diverged *DivergenceError
		if errors.As(err, &diverged) {
			return nil, err
		}
		// Emit completion error event
		r.publishEvent(events.TopicAICompletionError, events.KindAICompletionError, map[string]any{
			"templateId": invocation.TemplateId,
			"provider":   providerName,
			"durationMs": time.Since(startTime).Milliseconds(),
			"error":      err.Error(),
		})
		return nil, fmt.Errorf("ai call via provider %q failed: %w", providerName, err)
	}

	// Emit completion finished event
	r.publishEvent(events.TopicAICompletionFinished, events.KindAICompletionFinished, map[string]any{
		"templateId": invocation.TemplateId,
		"provider":   providerName,
		"durationMs": time.Since(startTime).Milliseconds(),
		"cached":     false,
	})

	if ttl > 0 && r.cache != nil {
		r.cache.set(cacheKey, result, ttl)
	}
	// Store the fresh result back into the semantic cache so the next
	// near-duplicate prompt hits. No-op for disabled/unset namespaces.
	if semanticEnabled {
		r.semantic.Store(ctx, namespace, text, result)
	}
	return result, nil
}

func (r *aiRuntime) cacheTTL(invocation *AIInvocation) time.Duration {
	if r == nil {
		return 0
	}
	maxSeconds := clampAICacheSeconds(r.cacheCfg.MaxTTLSeconds)
	if maxSeconds <= 0 {
		return 0
	}
	if invocation != nil && invocation.CacheSeconds != nil {
		requested := clampAICacheSeconds(*invocation.CacheSeconds)
		if requested <= 0 {
			return 0
		}
		return time.Duration(requested) * time.Second
	}
	if !r.cacheCfg.DefaultEnabled {
		return 0
	}
	return time.Duration(maxSeconds) * time.Second
}

func (r *aiRuntime) resolveProviderName(prompt *PromptTemplate, invocation *AIInvocation) (string, error) {
	if r == nil || r.providers == nil {
		return "", fmt.Errorf("provider registry is not configured")
	}
	if invocation != nil && invocation.ProviderOverride != nil {
		if trimmed := strings.TrimSpace(*invocation.ProviderOverride); trimmed != "" {
			return trimmed, nil
		}
	}
	if prompt != nil && strings.TrimSpace(prompt.DefaultProvider) != "" {
		return strings.TrimSpace(prompt.DefaultProvider), nil
	}
	if defaultName := r.providers.Default(); strings.TrimSpace(defaultName) != "" {
		return strings.TrimSpace(defaultName), nil
	}
	return "", ErrProviderUnavailable("(default)")
}

func (r *aiRuntime) resolvePrompt(id string) (*PromptTemplate, error) {
	if r == nil || r.prompts == nil {
		return nil, fmt.Errorf("prompt registry is not initialized")
	}
	name := strings.TrimSpace(id)
	if name == "" {
		return nil, fmt.Errorf("ai() requires a prompt template ID")
	}
	if prompt, ok := r.prompts.Get(name); ok && prompt != nil {
		return prompt, nil
	}
	return nil, fmt.Errorf("unknown prompt template %q", name)
}

func normalizeAIData(data any) (map[string]any, error) {
	if data == nil {
		return map[string]any{}, nil
	}
	if payload, ok := data.(map[string]any); ok && payload != nil {
		// jsonschema expects encoding/json-native container types:
		// - objects: map[string]any
		// - arrays:  []any
		//
		// Callers often pass typed Go slices (e.g. []map[string]any), which are valid JSON
		// but not recognized by the validator as "jsonType: array". Normalize by round-tripping.
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("ai() data json encode: %w", err)
		}
		var normalized map[string]any
		if err := json.Unmarshal(b, &normalized); err != nil {
			return nil, fmt.Errorf("ai() data json decode: %w", err)
		}
		if normalized == nil {
			return map[string]any{}, nil
		}
		return normalized, nil
	}
	return nil, fmt.Errorf("ai() data object must be a JSON object, got %T", data)
}
