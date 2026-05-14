package memql

import (
	"context"
	"testing"
	"text/template"

	"github.com/stretchr/testify/require"
)

type mockSIProvider struct {
	calls int
}

func (m *mockSIProvider) Call(_ context.Context, prompt string) (any, error) {
	m.calls++
	return prompt, nil
}

func TestSIRuntimeCacheDefaultEnabled(t *testing.T) {
	prompts := newPromptRegistry()
	tmpl := template.Must(template.New("test").Parse("hello {{.name}}"))
	prompts.set(&PromptTemplate{
		Name:            "testPrompt",
		TemplateSource:  "hello {{.name}}",
		tmpl:            tmpl,
		DefaultProvider: "mock",
	})

	providers := newProviderRegistry("")
	mock := &mockSIProvider{}
	providers.setEntry(&ProviderConfigEntry{
		Config: ProviderConfig{
			Name: "mock",
			Type: "test",
		},
		Client:    mock,
		Available: true,
	})

	runtime := newSIRuntime(nil, prompts, providers, siCacheConfig{
		DefaultEnabled: true,
		MaxTTLSeconds:  120,
	})
	require.NotNil(t, runtime)

	invocation := &SIInvocation{TemplateId: "testPrompt"}
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

func TestSIRuntimeCacheOverrideWhenDisabled(t *testing.T) {
	prompts := newPromptRegistry()
	tmpl := template.Must(template.New("test2").Parse("ping {{.value}}"))
	prompts.set(&PromptTemplate{
		Name:            "ttlPrompt",
		TemplateSource:  "ping {{.value}}",
		tmpl:            tmpl,
		DefaultProvider: "mock",
	})

	providers := newProviderRegistry("")
	mock := &mockSIProvider{}
	providers.setEntry(&ProviderConfigEntry{
		Config: ProviderConfig{
			Name: "mock",
			Type: "test",
		},
		Client:    mock,
		Available: true,
	})

	runtime := newSIRuntime(nil, prompts, providers, siCacheConfig{
		DefaultEnabled: false,
		MaxTTLSeconds:  120,
	})
	require.NotNil(t, runtime)

	invocation := &SIInvocation{
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
