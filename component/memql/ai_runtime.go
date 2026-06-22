package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/events"
)

type aiRuntime struct {
	logger    *slog.Logger
	prompts   *PromptRegistry
	providers *ProviderRegistry
	cache     *aiResponseCache
	cacheCfg  aiCacheConfig
	eventBus  *events.Bus
}

func newAIRuntime(logger *slog.Logger, prompts *PromptRegistry, providers *ProviderRegistry, cfg aiCacheConfig) *aiRuntime {
	if prompts == nil && providers == nil {
		return nil
	}
	return &aiRuntime{
		logger:    logger,
		prompts:   prompts,
		providers: providers,
		cache:     newAIResponseCache(),
		cacheCfg:  cfg,
	}
}

// SetEventBus wires the event bus for SI completion events.
func (r *aiRuntime) SetEventBus(bus *events.Bus) {
	if r == nil {
		return
	}
	r.eventBus = bus
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

	entry, ok := r.providers.Entry(providerName)
	if !ok || entry == nil || !entry.Available || entry.Client == nil {
		return nil, fmt.Errorf("provider %q not available", providerName)
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

	// Emit completion started event
	r.publishEvent(events.TopicAICompletionStarted, events.KindAICompletionStarted, map[string]any{
		"templateId": invocation.TemplateId,
		"provider":   providerName,
	})

	result, err := entry.Client.Call(ctx, text)
	if err != nil {
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
	return "", fmt.Errorf("provider %q not available", "(default)")
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
