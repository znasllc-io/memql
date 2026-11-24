package app

// embedding_indexer_test.go -- memory search embeds AS ITS INDEXER, whoever
// asks (design record 2026-11-23, section 1), through the one resolver every
// plug-in embeds with (PluginContext.ResolveEmbeddingProvider) and the real
// router behind it.
//
// Each of the first two fails against the resolver this replaced, which
// resolved every embedding for whoever was asking: the writer below has no
// machine, so the bound model resolved for them is simply not there, and an
// item they write is never indexed. The third pins what must not change.

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/router"
	"github.com/znasllc-io/memql/core/airoute"
)

const (
	boundEmbedder    = "fleet:qwen3-embedding:0.6b"
	boundEmbedderId  = "qwen3-embedding:0.6b"
	embeddingIndexer = "owner-1"
	writerNoMachine  = "writer-1"
)

// servedEmbedding is what one embedding call carried to the fleet.
type servedEmbedding struct {
	// planned is whose machines the call was placed on.
	planned string
	// asserted is the subject the call's assertion names: what the replica
	// holding the machine's stream checks the machine against.
	asserted string
	// actor is who the call's own context still runs as.
	actor string
}

// indexerFleet is the fleet as the catalog projects it: each person sees
// their own machines and the ones lent to them, and nobody else's.
type indexerFleet struct {
	mu       sync.Mutex
	byPerson map[string][]memql.FleetModel
	served   []servedEmbedding
}

func (f *indexerFleet) Catalog(_ context.Context, actingUserId string) ([]memql.FleetModel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.byPerson[actingUserId], nil
}

func (*indexerFleet) ModelPreference(context.Context, string) ([]string, error) { return nil, nil }

func (f *indexerFleet) Call(ctx context.Context, req memql.FleetCallRequest) (memql.FleetCallResult, error) {
	served := servedEmbedding{planned: req.ActingUserId}
	if a, ok := auth.ForwardedAuthorityFromContext(ctx); ok {
		served.asserted = a.Subject
	}
	if ac, ok := auth.AccessFromContext(ctx); ok && ac != nil {
		served.actor = ac.UserId
	}
	f.mu.Lock()
	f.served = append(f.served, served)
	f.mu.Unlock()
	vectors := make([][]float32, len(req.EmbeddingInput))
	for i := range vectors {
		vectors[i] = make([]float32, 1024)
		vectors[i][0] = 1
	}
	return memql.FleetCallResult{Embeddings: vectors, ExecutionSurface: "fleet:studio"}, nil
}

func (f *indexerFleet) calls() []servedEmbedding {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]servedEmbedding(nil), f.served...)
}

// anEmbeddingModelOnline is one online machine offering an embedding model.
func anEmbeddingModelOnline(modelId string) memql.FleetModel {
	return memql.FleetModelAdvertisedOnEveryMachineForTest(memql.FleetModel{
		ModelId:       modelId,
		ContextWindow: 32768,
		Embeddings:    true,
		Machines:      []memql.FleetMachine{{RegistrationId: "studio", Name: "studio", Online: true}},
	})
}

// recordingResolver stands in front of the real router and keeps every
// request it is handed, as the engine finished it (attribution applied).
type recordingResolver struct {
	inner memql.AIResolver
	mu    sync.Mutex
	got   []airoute.ResolveRequest
}

func (r *recordingResolver) ResolveFor(ctx context.Context, req airoute.ResolveRequest) (memql.ResolvedProvider, error) {
	r.mu.Lock()
	r.got = append(r.got, req)
	r.mu.Unlock()
	return r.inner.ResolveFor(ctx, req)
}

func (r *recordingResolver) last(t *testing.T) airoute.ResolveRequest {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	require.NotEmpty(t, r.got, "the router was never asked")
	return r.got[len(r.got)-1]
}

// appEmbeddingThroughTheRouter builds the app's plug-in resolver over the real
// router, a provider registry holding the fleet and any vendor records, and a
// cluster binding read the way production reads it: freshly, on every call,
// counted.
func appEmbeddingThroughTheRouter(t *testing.T, fleet *indexerFleet, binding memql.EmbedderBinding, vendor map[string]*vendorEmbedder) (*App, *recordingResolver, *int) {
	t.Helper()
	providers := memql.NewProviderRegistryForTest()
	providers.SetFleetInference(fleet)
	for name, client := range vendor {
		// A vendor record declares its window; the router checks a call's
		// context floor against it.
		providers.RegisterWithParamsForTest(name, "OpenAI", "text-embedding-3-small", map[string]any{"contextWindow": 8191}, client)
	}
	rules := memql.NewRuleRegistry()
	require.NoError(t, rules.Register(&memql.RuleConfig{
		Name: memql.DefaultRuleName, When: memql.RuleWhen{Present: map[string]bool{}}, Policy: "embeddingsBinding",
		OnUnavailable: memql.OnUnavailablePark, Locked: true, SourceFile: "dsl/rules/rules.memql",
	}))
	require.NoError(t, rules.Finalize())
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	resolver := &recordingResolver{inner: router.New(providers, memql.NewPolicyRegistryForTest(map[string][]string{"embeddingsBinding": {"embedder:active"}}), rules, nil, logger)}
	engine := &memql.MemQLEngine{}
	engine.SetAIResolver(resolver)

	reads := 0
	memql.SetEmbedderBindingReader(func(context.Context) (memql.EmbedderBinding, error) {
		reads++
		return binding, nil
	})
	t.Cleanup(func() { memql.SetEmbedderBindingReader(nil) })
	return &App{engine: engine, Logger: logger}, resolver, &reads
}

// THE WHOLE OF DECISION 7. The writer has no machine; the owner who chose the
// model has one offering it. The writer's item is embedded on the owner's
// machine, and the decision names the owner as whom the call was resolved for.
func TestAnEmbeddingRunsOnTheIndexersMachinesWhoeverAsks(t *testing.T) {
	fleet := &indexerFleet{byPerson: map[string][]memql.FleetModel{
		embeddingIndexer: {anEmbeddingModelOnline(boundEmbedderId)},
	}}
	a, resolver, _ := appEmbeddingThroughTheRouter(t, fleet, memql.EmbedderBinding{
		ProviderRef: boundEmbedder, Dimensions: 1024, ActivatedAt: "2026-11-24T10:00:00Z", IndexedBy: embeddingIndexer,
	}, nil)
	writer := asPerson(writerNoMachine)

	provider, err := a.pluginContext().ResolveEmbeddingProvider(writer, boundEmbedder)
	require.NoError(t, err, "the bound model must resolve for a writer with no machine: it runs on the indexer's")
	require.Equal(t, embeddingIndexer, resolver.last(t).UserId, "the decision names the indexer")

	vector, err := provider.Embed(memql.ContextWithBackgroundLane(writer), "a note the writer wrote: "+t.Name())
	require.NoError(t, err)
	require.Len(t, vector, 1024)
	require.Equal(t, []servedEmbedding{{planned: embeddingIndexer, asserted: embeddingIndexer, actor: writerNoMachine}}, fleet.calls(),
		"placed on the indexer's machines and asserted as the indexer for the hop, while the call itself still runs as the writer")

	// A fleet model that is not the bound one is nobody's index: it resolves
	// for its caller, as every other fleet call does, and the writer has none.
	_, err = a.pluginContext().ResolveEmbeddingProvider(writer, "fleet:nomic-embed-text")
	require.Error(t, err)
	require.Equal(t, writerNoMachine, resolver.last(t).UserId)
}

// The index job names the indexer of the model it is filling, and that wins
// over the active binding's -- a switch fills a model that is not active yet,
// and an owner taking over indexes with their own machines from the next
// batch. The binding is not read at all: the job already holds it.
func TestTheJobsIndexerOverridesTheActiveOne(t *testing.T) {
	fleet := &indexerFleet{byPerson: map[string][]memql.FleetModel{
		"owner-2": {anEmbeddingModelOnline(boundEmbedderId)},
	}}
	a, resolver, reads := appEmbeddingThroughTheRouter(t, fleet, memql.EmbedderBinding{
		ProviderRef: boundEmbedder, Dimensions: 1024, ActivatedAt: "2026-11-24T10:00:00Z", IndexedBy: embeddingIndexer,
	}, nil)
	job := memql.ContextWithEmbeddingIndexer(asPerson(writerNoMachine), "owner-2")

	provider, err := a.pluginContext().ResolveEmbeddingProvider(job, boundEmbedder)
	require.NoError(t, err)
	require.Equal(t, "owner-2", resolver.last(t).UserId)
	require.Zero(t, *reads, "a context that names the indexer must not read the binding a second time")

	_, err = provider.Embed(memql.ContextWithBackgroundLane(job), "a lesson: "+t.Name())
	require.NoError(t, err)
	require.Equal(t, "owner-2", fleet.calls()[0].planned)
	require.Equal(t, "owner-2", fleet.calls()[0].asserted)
}

// vendorEmbedder is a vendor record's client: it has no machines and nothing
// to assert for a hop.
type vendorEmbedder struct{ asserted []bool }

func (*vendorEmbedder) Call(context.Context, string) (any, error) { return nil, nil }

func (v *vendorEmbedder) Embed(ctx context.Context, _ string) ([]float32, error) {
	_, asserted := auth.ForwardedAuthorityFromContext(ctx)
	v.asserted = append(v.asserted, asserted)
	return []float32{1, 0}, nil
}

func (v *vendorEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, text := range texts {
		out[i], _ = v.Embed(ctx, text)
	}
	return out, nil
}

func (*vendorEmbedder) Dimensions() int { return 1536 }

// A VENDOR MODEL IS UNCHANGED: it runs on no one's machines, and the
// cluster's account already pays for embeddings (component/router
// accounts.go), so the call is still resolved for whoever asked and nothing
// is borrowed -- not even when the context names an indexer.
func TestAVendorBindingIsResolvedAsBefore(t *testing.T) {
	vendor := &vendorEmbedder{}
	a, resolver, reads := appEmbeddingThroughTheRouter(t, &indexerFleet{}, memql.EmbedderBinding{
		ProviderRef: "embedding3Small", Dimensions: 1536, ActivatedAt: "2026-11-24T10:00:00Z",
	}, map[string]*vendorEmbedder{"embedding3Small": vendor})

	for _, ctx := range []context.Context{asPerson(writerNoMachine), memql.ContextWithEmbeddingIndexer(asPerson(writerNoMachine), "owner-2")} {
		provider, err := a.pluginContext().ResolveEmbeddingProvider(ctx, "embedding3Small")
		require.NoError(t, err)
		require.Equal(t, writerNoMachine, resolver.last(t).UserId, "a vendor call is resolved for its caller, as before")
		require.Same(t, vendor, provider, "a vendor's client is handed back as it is")
		_, err = provider.Embed(ctx, "text")
		require.NoError(t, err)
	}
	require.Equal(t, []bool{false, false}, vendor.asserted, "nothing is asserted for a vendor call")
	require.Zero(t, *reads, "a vendor ref must not read the binding")
}
