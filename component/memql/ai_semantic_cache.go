package memql

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ----------------------------------------------------------------------------
// Semantic (vector) AI-call cache primitive  --  Epic 5, issue 5.9
// (Phase 1 of docs/internal/planning/llm-driven-decisions.md, §1.3)
//
// WHERE THIS SITS
//
// The exact-hash cache (aiResponseCache, ai_cache.go) keys on
// sha256(templateId|provider|rendered-prompt), so "thanks!" and "thanks"
// miss each other. High-redundancy CLASSIFICATION inputs (message intent,
// affirmation, domain-shape) are exactly where the misses pile up: a
// near-duplicate prompt should resolve to the SAME label, but the exact
// cache re-pays the LLM every time.
//
// The semantic cache fills that gap. The lookup order on the AI path is:
//
//	1. exact-hash cache   (cheap, byte-exact)            -- ai_cache.go
//	2. semantic cache     (embedding nearest-neighbour)  -- THIS FILE, only
//	                                                        for ENABLED namespaces
//	3. fresh LLM call      -> store back into BOTH caches
//
// THE WRONG-ANSWER RISK (the whole concern)
//
// A near-duplicate prompt does NOT in general imply an identical correct
// answer. "cancel the meeting" and "confirm the meeting" are lexically and
// semantically close yet demand OPPOSITE answers. So the semantic cache is:
//
//   - opt-in PER NAMESPACE (a "classification namespace" = a stable
//     input->label call type). Disabled by default; a disabled namespace
//     falls straight through to exact-hash/fresh with zero behaviour change.
//   - gated by a conservative, per-namespace similarity threshold
//     (default 0.95) -- a hit only counts at or above it.
//   - NEVER appropriate for free-form generation (agent prose replies):
//     two prompts that "look similar" there can demand entirely different
//     correct outputs. Only register classification/structured-output
//     namespaces with a stable input->label mapping.
//
// HERMETIC BY CONSTRUCTION
//
// The primitive depends on two narrow interfaces -- a SemanticEmbedder and a
// SemanticVectorStore -- not on Postgres or a real embedding provider
// directly. Production wires the existing embedding provider + node_vectors
// behind them (see ai_semantic_cache_store.go); tests wire a deterministic
// in-memory embedder + store so the false-positive eval runs with no network
// and no DB. This keeps CI hermetic AND keeps the threshold an honest
// measurement of separation between near-duplicates and semantic opposites.
// ----------------------------------------------------------------------------

// defaultSemanticThreshold is the conservative per-namespace similarity floor.
// Cosine similarity in [0,1]; a hit requires similarity >= threshold. 0.95 is
// deliberately high -- the cost of a false positive (a WRONG cached answer) is
// far higher than the cost of a false negative (one extra LLM call).
const defaultSemanticThreshold = 0.95

// SemanticEmbedder turns prompt text into a vector. Production wraps the
// existing EmbeddingAIProvider; tests supply a deterministic stub.
type SemanticEmbedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// SemanticVectorStore is the nearest-neighbour substrate the semantic cache
// reads/writes, scoped by namespace. Production wraps node_vectors; tests use
// an in-memory map. Cached VALUES live alongside the vector so a hit returns
// the stored AIResult directly (no second round-trip).
type SemanticVectorStore interface {
	// Nearest returns the single closest stored entry to vec WITHIN the
	// namespace, with its cosine similarity in [0,1]. ok=false when the
	// namespace is empty or no entry exists.
	Nearest(ctx context.Context, namespace string, vec []float32) (value any, similarity float64, ok bool, err error)

	// Store upserts (vec -> value) under the namespace with the given TTL.
	Store(ctx context.Context, namespace string, vec []float32, value any, ttl time.Duration) error
}

// SemanticNamespaceConfig is the per-call-type enablement record. A namespace
// is the identity of a classification call (e.g. "cognition.message_intent").
type SemanticNamespaceConfig struct {
	// Enabled gates the whole namespace. Disabled (the zero value) =>
	// the AI path never consults the semantic cache for this namespace.
	Enabled bool
	// Threshold is the minimum cosine similarity for a hit. <=0 falls back
	// to defaultSemanticThreshold. >1 is clamped to 1 (only exact-direction
	// matches would hit, which is effectively off).
	Threshold float64
	// TTL bounds how long a stored classification stays valid. <=0 falls
	// back to the cache-wide default TTL.
	TTL time.Duration
}

// SemanticCacheStats is the telemetry snapshot, mirroring AICacheStats so the
// two cache layers report through the same shape.
type SemanticCacheStats struct {
	Lookups           int64   `json:"lookups"`           // enabled-namespace lookups attempted
	Hits              int64   `json:"hits"`              // similarity >= threshold
	Misses            int64   `json:"misses"`            // no entry, or below threshold
	BelowThreshold    int64   `json:"belowThreshold"`    // a neighbour existed but was rejected (the guard firing)
	Sets              int64   `json:"sets"`              // results stored back
	DisabledLookups   int64   `json:"disabledLookups"`   // calls for a disabled/unknown namespace (fell through)
	Errors            int64   `json:"errors"`            // embed/store failures (fail-open to fresh)
	TokensSavedApprox int64   `json:"tokensSavedApprox"` // sum of prompt-token estimates avoided by hits
	HitRatio          float64 `json:"hitRatio"`
}

// semanticAICache is the primitive. It is intentionally decoupled from the
// engine: construct it with an embedder + store + namespace registry and call
// Lookup / Store around an LLM call.
type semanticAICache struct {
	embedder   SemanticEmbedder
	store      SemanticVectorStore
	logger     *slog.Logger
	defaultTTL time.Duration

	mu         sync.RWMutex
	namespaces map[string]SemanticNamespaceConfig

	lookups         atomic.Int64
	hits            atomic.Int64
	misses          atomic.Int64
	belowThreshold  atomic.Int64
	sets            atomic.Int64
	disabledLookups atomic.Int64
	errors          atomic.Int64
	tokensSaved     atomic.Int64
}

// newSemanticAICache builds the primitive. namespaces is the initial
// enablement registry (may be nil => everything disabled). A nil embedder or
// store makes every Lookup a clean miss (the primitive degrades to a no-op),
// so a build that hasn't wired the substrate is safe.
func newSemanticAICache(
	embedder SemanticEmbedder,
	store SemanticVectorStore,
	logger *slog.Logger,
	defaultTTL time.Duration,
	namespaces map[string]SemanticNamespaceConfig,
) *semanticAICache {
	cp := make(map[string]SemanticNamespaceConfig, len(namespaces))
	for k, v := range namespaces {
		cp[strings.TrimSpace(k)] = v
	}
	if defaultTTL <= 0 {
		defaultTTL = 24 * time.Hour
	}
	return &semanticAICache{
		embedder:   embedder,
		store:      store,
		logger:     logger,
		defaultTTL: defaultTTL,
		namespaces: cp,
	}
}

// SetNamespace registers/updates a namespace's enablement at runtime. Used to
// flip a namespace on (with a tuned threshold) without a rebuild.
func (c *semanticAICache) SetNamespace(namespace string, cfg SemanticNamespaceConfig) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.namespaces[strings.TrimSpace(namespace)] = cfg
	c.mu.Unlock()
}

// configFor returns the resolved config for a namespace and whether it is
// enabled. An unknown namespace is treated as disabled (conservative default).
func (c *semanticAICache) configFor(namespace string) (SemanticNamespaceConfig, bool) {
	c.mu.RLock()
	cfg, ok := c.namespaces[strings.TrimSpace(namespace)]
	c.mu.RUnlock()
	if !ok || !cfg.Enabled {
		return cfg, false
	}
	if cfg.Threshold <= 0 {
		cfg.Threshold = defaultSemanticThreshold
	}
	if cfg.Threshold > 1 {
		cfg.Threshold = 1
	}
	if cfg.TTL <= 0 {
		cfg.TTL = c.defaultTTL
	}
	return cfg, true
}

// Enabled reports whether a namespace is opted in. Cheap; used by the AI path
// to skip the embed call entirely for disabled namespaces.
func (c *semanticAICache) Enabled(namespace string) bool {
	if c == nil || c.embedder == nil || c.store == nil {
		return false
	}
	_, ok := c.configFor(namespace)
	return ok
}

// Lookup attempts a semantic hit for promptText within namespace. Returns the
// cached value only when an enabled namespace has a stored neighbour at or
// above its threshold. On ANY error it fails open (ok=false, no error
// surfaced) so the caller proceeds to a fresh LLM call -- a cache must never
// break the request path.
func (c *semanticAICache) Lookup(ctx context.Context, namespace, promptText string) (value any, ok bool) {
	if c == nil || c.embedder == nil || c.store == nil {
		return nil, false
	}
	cfg, enabled := c.configFor(namespace)
	if !enabled {
		c.disabledLookups.Add(1)
		return nil, false
	}
	c.lookups.Add(1)

	vec, err := c.embedder.Embed(ctx, promptText)
	if err != nil {
		c.errors.Add(1)
		c.logSoft("semanticCache: embed failed on lookup; falling through to fresh", namespace, err)
		return nil, false
	}

	cached, sim, found, err := c.store.Nearest(ctx, namespace, vec)
	if err != nil {
		c.errors.Add(1)
		c.logSoft("semanticCache: nearest lookup failed; falling through to fresh", namespace, err)
		return nil, false
	}
	if !found {
		c.misses.Add(1)
		return nil, false
	}
	if sim < cfg.Threshold {
		// The guard firing: a lexically/semantically near neighbour that is
		// NOT close enough to be the same answer. This is the line that keeps
		// "cancel the meeting" from returning "confirm the meeting"'s label.
		c.belowThreshold.Add(1)
		c.misses.Add(1)
		return nil, false
	}

	c.hits.Add(1)
	c.tokensSaved.Add(int64(estimatePromptTokens(promptText)))
	return cloneInterface(cached), true
}

// Store embeds promptText and writes (vector -> value) under the namespace, so
// the next near-duplicate hits. No-op for disabled namespaces. Best-effort:
// a store failure is counted and logged but never returned (it must not fail
// the request that produced a perfectly good fresh answer).
func (c *semanticAICache) Store(ctx context.Context, namespace, promptText string, value any) {
	if c == nil || c.embedder == nil || c.store == nil {
		return
	}
	cfg, enabled := c.configFor(namespace)
	if !enabled {
		return
	}
	vec, err := c.embedder.Embed(ctx, promptText)
	if err != nil {
		c.errors.Add(1)
		c.logSoft("semanticCache: embed failed on store; result not cached", namespace, err)
		return
	}
	if err := c.store.Store(ctx, namespace, vec, cloneInterface(value), cfg.TTL); err != nil {
		c.errors.Add(1)
		c.logSoft("semanticCache: store failed; result not cached", namespace, err)
		return
	}
	c.sets.Add(1)
}

// Stats snapshots the counters.
func (c *semanticAICache) Stats() SemanticCacheStats {
	if c == nil {
		return SemanticCacheStats{}
	}
	hits := c.hits.Load()
	misses := c.misses.Load()
	total := hits + misses
	ratio := 0.0
	if total > 0 {
		ratio = float64(hits) / float64(total)
	}
	return SemanticCacheStats{
		Lookups:           c.lookups.Load(),
		Hits:              hits,
		Misses:            misses,
		BelowThreshold:    c.belowThreshold.Load(),
		Sets:              c.sets.Load(),
		DisabledLookups:   c.disabledLookups.Load(),
		Errors:            c.errors.Load(),
		TokensSavedApprox: c.tokensSaved.Load(),
		HitRatio:          ratio,
	}
}

func (c *semanticAICache) logSoft(msg, namespace string, err error) {
	if c.logger == nil {
		return
	}
	c.logger.Warn(msg, "namespace", namespace, "err", err)
}

// estimatePromptTokens is a coarse token estimate (~4 chars/token) used only
// to attribute approximate token savings to a hit for the 5.9 token-saving
// metric. Not billing-grade; a demonstrable proxy.
func estimatePromptTokens(text string) int {
	t := strings.TrimSpace(text)
	if t == "" {
		return 0
	}
	return (len(t) + 3) / 4
}

// cosineSimilarity returns cosine similarity in [-1,1] mapped to [0,1] by the
// store implementations that need it. Exposed at package scope so both the
// in-memory test store and any pure-Go store reuse one definition.
func cosineSimilarity(a, b []float32) (float64, error) {
	if len(a) != len(b) {
		return 0, fmt.Errorf("cosineSimilarity: dimension mismatch %d != %d", len(a), len(b))
	}
	var dot, na, nb float64
	for i := range a {
		av := float64(a[i])
		bv := float64(b[i])
		dot += av * bv
		na += av * av
		nb += bv * bv
	}
	if na == 0 || nb == 0 {
		return 0, nil
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb)), nil
}

// memVectorStore is an in-memory SemanticVectorStore used by tests and the
// false-positive eval. It is the canonical reference implementation of the
// nearest-neighbour contract: brute-force cosine scan within a namespace,
// TTL-expiry on read. The Postgres node_vectors store
// (ai_semantic_cache_store.go) implements the same contract over pgvector.
type memVectorStore struct {
	mu      sync.RWMutex
	entries map[string][]memVectorEntry
}

type memVectorEntry struct {
	vec       []float32
	value     any
	expiresAt time.Time
}

func newMemVectorStore() *memVectorStore {
	return &memVectorStore{entries: make(map[string][]memVectorEntry)}
}

func (s *memVectorStore) Nearest(_ context.Context, namespace string, vec []float32) (any, float64, bool, error) {
	now := time.Now()
	s.mu.RLock()
	list := s.entries[namespace]
	s.mu.RUnlock()

	bestSim := -2.0
	var bestVal any
	found := false
	for _, e := range list {
		if now.After(e.expiresAt) {
			continue
		}
		sim, err := cosineSimilarity(vec, e.vec)
		if err != nil {
			return nil, 0, false, err
		}
		if sim > bestSim {
			bestSim = sim
			bestVal = e.value
			found = true
		}
	}
	if !found {
		return nil, 0, false, nil
	}
	// Clamp negative cosine to 0 so the [0,1] threshold contract holds.
	if bestSim < 0 {
		bestSim = 0
	}
	return bestVal, bestSim, true, nil
}

func (s *memVectorStore) Store(_ context.Context, namespace string, vec []float32, value any, ttl time.Duration) error {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	cp := make([]float32, len(vec))
	copy(cp, vec)
	s.mu.Lock()
	s.entries[namespace] = append(s.entries[namespace], memVectorEntry{
		vec:       cp,
		value:     value,
		expiresAt: time.Now().Add(ttl),
	})
	s.mu.Unlock()
	return nil
}
