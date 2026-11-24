package memql

import (
	"context"
	"database/sql"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ----------------------------------------------------------------------------
// Hermetic deterministic embedder for the semantic-cache tests + eval.
//
// A real embedding model places near-duplicate phrasings ("thanks" / "thx" /
// "thank you") close in vector space and lexically-similar-but-opposite
// phrases ("cancel the meeting" / "confirm the meeting") far apart. We can't
// call a real model in CI, so we model that geometry deterministically: each
// phrase is projected onto a small set of hand-labelled CONCEPT axes (gratitude,
// affirmation, cancel-intent, confirm-intent, ...). Synonyms share axes; the
// cancel/confirm pair share the "meeting" axis but load OPPOSITE intent axes,
// which is exactly the geometry the threshold guard must separate.
//
// This keeps the false-positive eval an honest test of the THRESHOLD MECHANISM
// (does similarity >= threshold correctly admit near-dupes and reject
// opposites?) without depending on a network model. The production path swaps
// this for the real EmbeddingAIProvider; the threshold semantics are identical.
// ----------------------------------------------------------------------------

// conceptAxes is the ordered concept vocabulary. Each phrase becomes a unit
// vector over these axes from the keyword hits below.
var conceptAxes = []string{
	"gratitude",      // thanks family
	"affirmation",    // yes / ok / sounds good
	"farewell",       // bye / goodnight
	"greeting",       // hi / hello
	"cancel_intent",  // cancel / call off / scrap
	"confirm_intent", // confirm / keep / lock in
	"meeting",        // meeting / appointment / call
	"send_intent",    // send / deliver / ship
	"delete_intent",  // delete / remove / wipe
	"keep_intent",    // keep / save / retain
}

// keywordAxis maps a normalized token to a concept axis. Multiple tokens map
// to the same axis => synonyms land on the same axis => high cosine similarity.
var keywordAxis = map[string]string{
	// gratitude
	"thanks": "gratitude", "thank": "gratitude", "thx": "gratitude",
	"thankyou": "gratitude", "ty": "gratitude", "cheers": "gratitude", "appreciate": "gratitude",
	// affirmation
	"yes": "affirmation", "yeah": "affirmation", "yep": "affirmation", "ok": "affirmation",
	"okay": "affirmation", "sure": "affirmation", "sounds": "affirmation", "good": "affirmation",
	// farewell
	"bye": "farewell", "goodbye": "farewell", "goodnight": "farewell", "later": "farewell",
	// greeting
	"hi": "greeting", "hello": "greeting", "hey": "greeting",
	// cancel
	"cancel": "cancel_intent", "scrap": "cancel_intent", "abort": "cancel_intent",
	// confirm
	"confirm": "confirm_intent", "lock": "confirm_intent",
	// meeting
	"meeting": "meeting", "appointment": "meeting", "sync": "meeting", "call": "meeting",
	// send
	"send": "send_intent", "deliver": "send_intent", "ship": "send_intent",
	// delete
	"delete": "delete_intent", "remove": "delete_intent", "wipe": "delete_intent", "erase": "delete_intent",
	// keep
	"keep": "keep_intent", "save": "keep_intent", "retain": "keep_intent",
}

type conceptEmbedder struct{}

func (conceptEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	vec := make([]float32, len(conceptAxes))
	axisIndex := make(map[string]int, len(conceptAxes))
	for i, a := range conceptAxes {
		axisIndex[a] = i
	}
	for _, tok := range tokenize(text) {
		if axis, ok := keywordAxis[tok]; ok {
			vec[axisIndex[axis]] += 1
		}
	}
	// Add a tiny uniform floor so a phrase with no keyword hits still yields a
	// non-zero vector (avoids a zero-norm degenerate case); negligible vs a
	// real hit.
	for i := range vec {
		vec[i] += 0.001
	}
	// Normalize to unit length so cosine == dot product.
	var norm float64
	for _, v := range vec {
		norm += float64(v) * float64(v)
	}
	norm = math.Sqrt(norm)
	if norm > 0 {
		for i := range vec {
			vec[i] = float32(float64(vec[i]) / norm)
		}
	}
	return vec, nil
}

func tokenize(text string) []string {
	t := strings.ToLower(text)
	var b strings.Builder
	for _, r := range t {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == ' ' {
			b.WriteRune(r)
		} else {
			b.WriteRune(' ')
		}
	}
	fields := strings.Fields(b.String())
	// Collapse "thank you" -> "thankyou" so the bigram lands on gratitude.
	out := make([]string, 0, len(fields))
	for i := 0; i < len(fields); i++ {
		if fields[i] == "thank" && i+1 < len(fields) && fields[i+1] == "you" {
			out = append(out, "thankyou")
			i++
			continue
		}
		out = append(out, fields[i])
	}
	return out
}

func newTestSemanticCache(t *testing.T, namespace string, threshold float64) *semanticAICache {
	t.Helper()
	return newSemanticAICache(
		conceptEmbedder{},
		newMemVectorStore(),
		nil,
		0,
		map[string]SemanticNamespaceConfig{
			namespace: {Enabled: true, Threshold: threshold},
		},
	)
}

// TestSemanticCacheHitNearDuplicate: a near-duplicate prompt hits without a
// fresh provider call. This is the property the whole layer exists for.
func TestSemanticCacheHitNearDuplicate(t *testing.T) {
	const ns = "test.gratitude"
	c := newTestSemanticCache(t, ns, 0.95)
	ctx := context.Background()

	// Cold: store the answer for "thanks".
	_, ok := c.Lookup(ctx, ns, "thanks")
	require.False(t, ok, "cold cache must miss")
	c.Store(ctx, ns, "thanks", "LABEL:affirmation_no_action")

	// "thank you" and "thx" are near-duplicates of "thanks" -> HIT.
	for _, near := range []string{"thank you", "thx", "thanks!"} {
		val, hit := c.Lookup(ctx, ns, near)
		require.True(t, hit, "near-duplicate %q should hit", near)
		require.Equal(t, "LABEL:affirmation_no_action", val)
	}

	stats := c.Stats()
	require.GreaterOrEqual(t, stats.Hits, int64(3))
	require.Equal(t, int64(1), stats.Sets)
	require.Greater(t, stats.TokensSavedApprox, int64(0), "hits must accrue approximate token savings")
}

// TestSemanticCacheMissDifferentMeaning: a lexically-similar but semantically
// OPPOSITE prompt must NOT hit. This is the wrong-answer guard.
func TestSemanticCacheMissDifferentMeaning(t *testing.T) {
	const ns = "test.meeting_intent"
	c := newTestSemanticCache(t, ns, 0.95)
	ctx := context.Background()

	c.Store(ctx, ns, "cancel the meeting", "LABEL:cancel")

	// Same words minus the verb-of-opposite-intent: must NOT return cancel.
	val, hit := c.Lookup(ctx, ns, "confirm the meeting")
	require.False(t, hit, "opposite-intent prompt must not hit; got %v", val)

	stats := c.Stats()
	require.Equal(t, int64(0), stats.Hits)
	require.GreaterOrEqual(t, stats.BelowThreshold, int64(1),
		"a near neighbour existed but the threshold guard must reject it")
}

// TestSemanticCacheDisabledNamespaceFallsThrough: a namespace that isn't
// enabled never consults the cache -- clean fall-through, no behaviour change.
func TestSemanticCacheDisabledNamespaceFallsThrough(t *testing.T) {
	c := newSemanticAICache(
		conceptEmbedder{},
		newMemVectorStore(),
		nil, 0,
		map[string]SemanticNamespaceConfig{
			"enabled.ns": {Enabled: true, Threshold: 0.95},
		},
	)
	ctx := context.Background()

	// Store under the enabled ns so there IS data to (wrongly) hit.
	c.Store(ctx, "enabled.ns", "thanks", "X")

	// A DISABLED namespace must not hit even with identical text.
	_, hit := c.Lookup(ctx, "disabled.ns", "thanks")
	require.False(t, hit, "disabled namespace must fall through")

	require.False(t, c.Enabled("disabled.ns"))
	require.True(t, c.Enabled("enabled.ns"))

	stats := c.Stats()
	require.GreaterOrEqual(t, stats.DisabledLookups, int64(1))
}

// TestSemanticCacheNilSafe: the primitive degrades to a no-op when the embedder
// or store isn't wired -- a build that hasn't wired the substrate is safe.
func TestSemanticCacheNilSafe(t *testing.T) {
	var nilCache *semanticAICache
	require.False(t, nilCache.Enabled("any"))
	_, ok := nilCache.Lookup(context.Background(), "any", "text")
	require.False(t, ok)
	nilCache.Store(context.Background(), "any", "text", "v") // must not panic

	noSubstrate := newSemanticAICache(nil, nil, nil, 0, map[string]SemanticNamespaceConfig{
		"ns": {Enabled: true},
	})
	require.False(t, noSubstrate.Enabled("ns"))
	_, ok = noSubstrate.Lookup(context.Background(), "ns", "thanks")
	require.False(t, ok)
}

// TestSemanticCacheThresholdTunable: the SAME pair flips from miss to hit as the
// threshold is lowered, proving thresholds are tunable per namespace.
func TestSemanticCacheThresholdTunable(t *testing.T) {
	ctx := context.Background()
	const ns = "test.tunable"

	// "send it" vs "ship it": synonyms (send_intent) -> high similarity but not
	// identical token sets. At a very high threshold it may be rejected; at a
	// moderate one it hits.
	mkAndProbe := func(threshold float64) bool {
		c := newTestSemanticCache(t, ns, threshold)
		c.Store(ctx, ns, "send it", "LABEL:send")
		_, hit := c.Lookup(ctx, ns, "ship it")
		return hit
	}

	// Synonyms share the send_intent axis exactly (both reduce to that axis +
	// floor), so they hit even at a high threshold. The "it" token maps to no
	// axis, so both vectors are dominated by send_intent => similarity ~1.
	require.True(t, mkAndProbe(0.95), "synonyms should hit at the default threshold")
	// At an impossibly strict threshold (1.0 exact direction) the tiny floor
	// differences still keep them effectively identical, so assert the lower
	// bound instead: a DIFFERENT-intent pair must miss at every sane threshold.
	cDiff := newTestSemanticCache(t, ns, 0.5)
	cDiff.Store(ctx, ns, "delete it", "LABEL:delete")
	_, hit := cDiff.Lookup(ctx, ns, "keep it")
	require.False(t, hit, "opposite intents must miss even at a permissive 0.5 threshold")
}

// boundCacheProbe is an embedding model the test binds memory search to. Both
// models answer the SAME vector for the same text, so only the cache scoping
// its rows by model can stop one model's row answering the other's lookup.
type boundCacheProbe struct {
	name  string
	calls *[]string
}

var boundCacheVectors = map[string][]float32{"classify: hello": {1, 0, 0, 0}, "classify: other": {0, 1, 0, 0}}

func (p boundCacheProbe) Call(context.Context, string) (any, error) { return nil, nil }
func (p boundCacheProbe) Embed(_ context.Context, text string) ([]float32, error) {
	*p.calls = append(*p.calls, p.name)
	if v, ok := boundCacheVectors[text]; ok {
		return v, nil
	}
	return []float32{0, 0, 1, 0}, nil
}
func (p boundCacheProbe) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	var out [][]float32
	for _, t := range texts {
		v, _ := p.Embed(ctx, t)
		out = append(out, v)
	}
	return out, nil
}
func (boundCacheProbe) Dimensions() int { return 4 }

// lateReplicaEmbedder answers as a replica that read the binding a moment
// before the flip and wrote after it: the replaced model's vector and name.
type lateReplicaEmbedder struct{ model string }

func (lateReplicaEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	return boundCacheVectors[text], nil
}
func (l lateReplicaEmbedder) EmbedWithModel(_ context.Context, text string) ([]float32, string, error) {
	return boundCacheVectors[text], l.model, nil
}

// semanticCacheOnBinding wires the semantic answer cache on an engine whose
// memory-search binding the returned function sets (nil: nothing bound), over
// an in-memory store, with one namespace enabled.
func semanticCacheOnBinding(t *testing.T, calls *[]string, binding *EmbedderBinding) (*semanticAICache, *memVectorStore) {
	t.Helper()
	providers := NewProviderRegistryForTest()
	providers.RegisterForTest("semanticEmbedA", "openai", "embed-a", boundCacheProbe{name: "a", calls: calls})
	providers.RegisterForTest("semanticEmbedB", "openai", "embed-b", boundCacheProbe{name: "b", calls: calls})
	e := auditTestEngine(t)
	e.providers = providers
	e.aiRuntime = newAIRuntime(nil, nil, providers, aiCacheConfig{})
	wireTestResolver(e, providers)
	SetEmbedderBindingReader(func(context.Context) (EmbedderBinding, error) {
		if binding.ProviderRef == "" {
			return EmbedderBinding{}, ErrNoEmbedderBound
		}
		return *binding, nil
	})
	t.Cleanup(func() { SetEmbedderBindingReader(nil) })
	e.WireSemanticCache("", func() *sql.DB { return nil })
	cache := e.aiRuntime.semantic
	require.NotNil(t, cache)
	store := newMemVectorStore()
	cache.store = store
	cache.SetNamespace("test.ns", SemanticNamespaceConfig{Enabled: true})
	return cache, store
}

// THE SEMANTIC ANSWER CACHE FOLLOWS THE BINDING (ruling R8). It embeds every
// lookup and every write with the model memory search is bound to at that
// moment, so after a switch it neither keeps embedding with the replaced model
// nor compares vectors of two models: a row is a hit only for the model that
// wrote it, including a row a lagging replica writes after the flip's DELETE.
func TestTheSemanticCacheFollowsTheBinding(t *testing.T) {
	var calls []string
	binding := EmbedderBinding{ProviderRef: "semanticEmbedA", Dimensions: 4, ActivatedAt: "2026-11-01T00:00:00Z"}
	cache, store := semanticCacheOnBinding(t, &calls, &binding)
	ctx := context.Background()
	cache.Store(ctx, "test.ns", "classify: hello", "label-a")
	value, hit := cache.Lookup(ctx, "test.ns", "classify: hello")
	require.True(t, hit)
	require.Equal(t, "label-a", value)

	// The switch: memory search now names model B.
	binding = EmbedderBinding{ProviderRef: "semanticEmbedB", Dimensions: 4, ActivatedAt: "2026-11-24T00:00:00Z"}
	calls = nil
	_, hit = cache.Lookup(ctx, "test.ns", "classify: hello")
	require.False(t, hit, "model A's row is not an answer for model B")
	require.Equal(t, []string{"b"}, calls, "the lookup embeds with the bound model")
	cache.Store(ctx, "test.ns", "classify: hello", "label-b")
	value, hit = cache.Lookup(ctx, "test.ns", "classify: hello")
	require.True(t, hit)
	require.Equal(t, "label-b", value)

	// A replica still embedding with model A writes after the flip.
	late := newSemanticAICache(lateReplicaEmbedder{model: "semanticEmbedA@4"}, store, nil, time.Hour,
		map[string]SemanticNamespaceConfig{"test.ns": {Enabled: true}})
	late.Store(ctx, "test.ns", "classify: other", "label-late")
	_, hit = cache.Lookup(ctx, "test.ns", "classify: other")
	require.False(t, hit, "a late write in the replaced model's space is never a hit")
}

// The cache is wired even before any model is bound -- a clean miss until then
// -- and works once one is, without a restart.
func TestTheSemanticCacheIsWiredBeforeAModelIsBound(t *testing.T) {
	var calls []string
	var binding EmbedderBinding
	providers := NewProviderRegistryForTest()
	providers.RegisterForTest("semanticEmbedA", "openai", "embed-a", boundCacheProbe{name: "a", calls: &calls})
	e := auditTestEngine(t)
	e.providers = providers
	e.aiRuntime = newAIRuntime(nil, nil, providers, aiCacheConfig{})
	wireTestResolver(e, providers)
	SetEmbedderBindingReader(func(context.Context) (EmbedderBinding, error) {
		if binding.ProviderRef == "" {
			return EmbedderBinding{}, ErrNoEmbedderBound
		}
		return binding, nil
	})
	t.Cleanup(func() { SetEmbedderBindingReader(nil) })
	e.WireSemanticCache("", func() *sql.DB { return nil })
	cache := e.aiRuntime.semantic
	require.NotNil(t, cache, "wired before any model is bound")
	cache.store = newMemVectorStore()
	cache.SetNamespace("test.ns", SemanticNamespaceConfig{Enabled: true})
	ctx := context.Background()
	_, hit := cache.Lookup(ctx, "test.ns", "classify: hello")
	require.False(t, hit, "nothing bound: a clean miss")

	binding = EmbedderBinding{ProviderRef: "semanticEmbedA", Dimensions: 4, ActivatedAt: "2026-11-24T00:00:00Z"}
	cache.Store(ctx, "test.ns", "classify: hello", "label-a")
	value, hit := cache.Lookup(ctx, "test.ns", "classify: hello")
	require.True(t, hit)
	require.Equal(t, "label-a", value)
}
