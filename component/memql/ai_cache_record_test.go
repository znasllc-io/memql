package memql

import (
	"context"
	"testing"
	"text/template"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/core/airoute"
)

type recordedCacheHit struct {
	Req        airoute.ResolveRequest
	Resolution airoute.Resolution
	Kind       string
}

func cachingRuntime(t *testing.T, hits *[]recordedCacheHit) (*aiRuntime, *mockAIProvider) {
	t.Helper()
	prompts := newPromptRegistry()
	prompts.set(&PromptTemplate{
		Level:           "fast",
		Name:            "cachedPrompt",
		TemplateSource:  "hello {{.name}}",
		tmpl:            template.Must(template.New("cached").Parse("hello {{.name}}")),
		DefaultProvider: "mock",
	})
	providers := newProviderRegistry()
	mock := &mockAIProvider{}
	providers.setEntry(&ProviderConfigEntry{
		Config:    ProviderConfig{Name: "mock", Type: "test"},
		Client:    mock,
		Available: true,
	})
	runtime := newTestAIRuntime(prompts, providers, aiCacheConfig{DefaultEnabled: true, MaxTTLSeconds: 120})
	require.NotNil(t, runtime)
	runtime.recordCacheServed = func(_ context.Context, req airoute.ResolveRequest, res airoute.Resolution, kind string, _ time.Time) {
		*hits = append(*hits, recordedCacheHit{Req: req, Resolution: res, Kind: kind})
	}
	return runtime, mock
}

// A CACHE HIT IS RECORDED AND A PROVIDER CALL IS NOT RECORDED TWICE
// (memql#5581).
//
// The exact-hash cache is consulted AFTER the router resolves, because its key
// folds in the resolved provider name. So the hit is a call that walked a
// chain, matched a rule and took a door -- and before this the whole decision
// was dropped with the stack frame and no ledger row was written at all, which
// made a warm page and an idle cluster the same picture.
func TestExactCacheHitRecordsItsDecisionAndAMissDoesNot(t *testing.T) {
	var hits []recordedCacheHit
	runtime, mock := cachingRuntime(t, &hits)

	invocation := &AIInvocation{TemplateId: "cachedPrompt"}
	data := map[string]any{"name": "Ada"}

	// The MISS reaches the provider and records no cache row: the row for a
	// provider call is the observer's, not this path's.
	result, err := runtime.Invoke(context.Background(), invocation, data)
	require.NoError(t, err)
	require.Equal(t, "hello Ada", result)
	require.Equal(t, 1, mock.calls)
	require.Empty(t, hits, "a provider call must not be recorded as a cache hit")

	// The HIT reaches no provider and records exactly one decision.
	result, err = runtime.Invoke(context.Background(), invocation, data)
	require.NoError(t, err)
	require.Equal(t, "hello Ada", result)
	require.Equal(t, 1, mock.calls, "the cache answered, so no provider was called")
	require.Len(t, hits, 1, "the hit wrote no decision record: a warm page is indistinguishable from an idle cluster")
	require.Equal(t, airoute.CacheKindExact, hits[0].Kind)
	require.Equal(t, "mock", hits[0].Resolution.ProviderName,
		"the record must name the provider whose answer is being replayed")
	require.Equal(t, "cachedPrompt", hits[0].Req.PromptName)
}

// A RUNTIME WITH NO RECORDER STILL SERVES THE ANSWER. An unrecordable cache
// hit must not become a failed call -- which is the opposite of the unwired
// RESOLVER, where refusing is the whole point.
func TestCacheHitWithoutARecorderStillServes(t *testing.T) {
	var hits []recordedCacheHit
	runtime, mock := cachingRuntime(t, &hits)
	runtime.recordCacheServed = nil

	invocation := &AIInvocation{TemplateId: "cachedPrompt"}
	data := map[string]any{"name": "Bea"}
	_, err := runtime.Invoke(context.Background(), invocation, data)
	require.NoError(t, err)
	result, err := runtime.Invoke(context.Background(), invocation, data)
	require.NoError(t, err)
	require.Equal(t, "hello Bea", result)
	require.Equal(t, 1, mock.calls)
}

// The seam refuses a cache kind outside the closed set rather than writing a
// row the concept's two-value enum would refuse.
func TestRecordCacheServedRefusesAnUnknownKind(t *testing.T) {
	require.True(t, airoute.ValidCacheKind(airoute.CacheKindExact))
	require.True(t, airoute.ValidCacheKind(airoute.CacheKindSemantic))
	for _, bad := range []string{"", " ", "guess", "Exact", "provider"} {
		require.False(t, airoute.ValidCacheKind(bad), "ValidCacheKind(%q)", bad)
	}
	// An empty kind is the absence of a cache, which is a provider call --
	// never a third state.
	require.Equal(t, []string{airoute.CacheKindExact, airoute.CacheKindSemantic}, airoute.CacheKinds())
}
