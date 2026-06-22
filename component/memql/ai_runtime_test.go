package memql

import (
	"context"
	"testing"
	"text/template"

	"github.com/stretchr/testify/require"
)

type mockAIProvider struct {
	calls int
}

func (m *mockAIProvider) Call(_ context.Context, prompt string) (any, error) {
	m.calls++
	return prompt, nil
}

func TestAIRuntimeCacheDefaultEnabled(t *testing.T) {
	prompts := newPromptRegistry()
	tmpl := template.Must(template.New("test").Parse("hello {{.name}}"))
	prompts.set(&PromptTemplate{
		Name:            "testPrompt",
		TemplateSource:  "hello {{.name}}",
		tmpl:            tmpl,
		DefaultProvider: "mock",
	})

	providers := newProviderRegistry("")
	mock := &mockAIProvider{}
	providers.setEntry(&ProviderConfigEntry{
		Config: ProviderConfig{
			Name: "mock",
			Type: "test",
		},
		Client:    mock,
		Available: true,
	})

	runtime := newAIRuntime(nil, prompts, providers, aiCacheConfig{
		DefaultEnabled: true,
		MaxTTLSeconds:  120,
	})
	require.NotNil(t, runtime)

	invocation := &AIInvocation{TemplateId: "testPrompt"}
	data := map[string]any{"name": "Ada"}

	result, err := runtime.Invoke(context.Background(), invocation, data)
	require.NoError(t, err)
	require.Equal(t, "hello Ada", result)
	require.Equal(t, 1, mock.calls)

	// Second invocation hits cache.
	result, err = runtime.Invoke(context.Background(), invocation, data)
	require.NoError(t, err)
	require.Equal(t, "hello Ada", result)
	require.Equal(t, 1, mock.calls)

	// Explicit TTL override of 0 disables caching for this invocation.
	invocation.CacheSeconds = optionalInt(0)
	_, err = runtime.Invoke(context.Background(), invocation, data)
	require.NoError(t, err)
	require.Equal(t, 2, mock.calls)

	// High TTL values are clamped to max but still cached (new prompt to avoid previous entry).
	invocation.CacheSeconds = optionalInt(600)
	data = map[string]any{"name": "Bea"}
	_, err = runtime.Invoke(context.Background(), invocation, data)
	require.NoError(t, err)
	require.Equal(t, 3, mock.calls)

	_, err = runtime.Invoke(context.Background(), invocation, data)
	require.NoError(t, err)
	require.Equal(t, 3, mock.calls)
}

// TestAIRuntimeCacheStatsBaseline is the 5.8 synthetic baseline: it
// proves the exact-hash AI cache is live on the Invoke path AND that
// the Phase-0 Stats() hit/miss counters actually move. A repeated
// identical AI call must hit; a different rendered prompt must miss.
// If this regresses, the cache is silently not caching (or the
// telemetry is broken) and any hit-ratio we report from BFF logs is a
// lie. See docs/internal/planning/cache-audit-phase-0.md.
func TestAIRuntimeCacheStatsBaseline(t *testing.T) {
	prompts := newPromptRegistry()
	tmpl := template.Must(template.New("baseline").Parse("greeting {{.name}}"))
	prompts.set(&PromptTemplate{
		Name:            "baselinePrompt",
		TemplateSource:  "greeting {{.name}}",
		tmpl:            tmpl,
		DefaultProvider: "mock",
	})

	providers := newProviderRegistry("")
	mock := &mockAIProvider{}
	providers.setEntry(&ProviderConfigEntry{
		Config: ProviderConfig{
			Name: "mock",
			Type: "test",
		},
		Client:    mock,
		Available: true,
	})

	runtime := newAIRuntime(nil, prompts, providers, aiCacheConfig{
		DefaultEnabled: true,
		MaxTTLSeconds:  120,
	})
	require.NotNil(t, runtime)

	// Counters start at zero on a fresh cache.
	require.Equal(t, AICacheStats{}, runtime.cache.Stats())

	invocation := &AIInvocation{TemplateId: "baselinePrompt"}
	ada := map[string]any{"name": "Ada"}

	// 1) First call for "Ada": cold cache -> miss + set + provider call.
	_, err := runtime.Invoke(context.Background(), invocation, ada)
	require.NoError(t, err)
	statsAfterFirst := runtime.cache.Stats()
	require.Equal(t, int64(0), statsAfterFirst.Hits, "first call must not hit")
	require.Equal(t, int64(1), statsAfterFirst.Misses, "first call must record a miss")
	require.Equal(t, int64(1), statsAfterFirst.Sets, "first call must populate the cache")
	require.Equal(t, 1, statsAfterFirst.Size)
	require.Equal(t, 1, mock.calls)

	// 2) Identical call for "Ada": same rendered prompt -> cache HIT, no
	//    new provider call. This is the property the whole cache exists for.
	_, err = runtime.Invoke(context.Background(), invocation, ada)
	require.NoError(t, err)
	statsAfterHit := runtime.cache.Stats()
	require.Equal(t, int64(1), statsAfterHit.Hits, "identical call must hit the cache")
	require.Equal(t, int64(1), statsAfterHit.Misses, "identical call must not add a miss")
	require.Equal(t, 1, mock.calls, "identical call must not reach the provider")

	// 3) Different prompt content ("Bea"): different hash key -> MISS.
	//    Exact-hash means near-duplicate inputs never hit (the 5.9
	//    semantic-cache motivation).
	bea := map[string]any{"name": "Bea"}
	_, err = runtime.Invoke(context.Background(), invocation, bea)
	require.NoError(t, err)
	statsAfterMiss := runtime.cache.Stats()
	require.Equal(t, int64(1), statsAfterMiss.Hits, "different prompt must not hit")
	require.Equal(t, int64(2), statsAfterMiss.Misses, "different prompt must record a miss")
	require.Equal(t, 2, mock.calls, "different prompt must reach the provider")

	// HitRatio reflects the counters: 1 hit / 3 lookups.
	require.InDelta(t, 1.0/3.0, statsAfterMiss.HitRatio, 1e-9)
}

func TestAIRuntimeCacheOverrideWhenDisabled(t *testing.T) {
	prompts := newPromptRegistry()
	tmpl := template.Must(template.New("test2").Parse("ping {{.value}}"))
	prompts.set(&PromptTemplate{
		Name:            "ttlPrompt",
		TemplateSource:  "ping {{.value}}",
		tmpl:            tmpl,
		DefaultProvider: "mock",
	})

	providers := newProviderRegistry("")
	mock := &mockAIProvider{}
	providers.setEntry(&ProviderConfigEntry{
		Config: ProviderConfig{
			Name: "mock",
			Type: "test",
		},
		Client:    mock,
		Available: true,
	})

	runtime := newAIRuntime(nil, prompts, providers, aiCacheConfig{
		DefaultEnabled: false,
		MaxTTLSeconds:  120,
	})
	require.NotNil(t, runtime)

	invocation := &AIInvocation{
		TemplateId:   "ttlPrompt",
		CacheSeconds: optionalInt(30),
	}
	data := map[string]any{"value": "pong"}

	result, err := runtime.Invoke(context.Background(), invocation, data)
	require.NoError(t, err)
	require.Equal(t, 1, mock.calls)
	require.Equal(t, "ping pong", result)

	_, err = runtime.Invoke(context.Background(), invocation, data)
	require.NoError(t, err)
	require.Equal(t, 1, mock.calls)

	// When TTL pointer cleared and default disabled, caching stops.
	invocation.CacheSeconds = nil
	_, err = runtime.Invoke(context.Background(), invocation, data)
	require.NoError(t, err)
	require.Equal(t, 2, mock.calls)
}
