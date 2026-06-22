package memql

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/dgraph-io/ristretto"
)

// resultCache memoises full ExecuteResult rows for query plans
// annotated with @cache(ttl=...). Backed by Ristretto (LFU + TTL +
// max-cost). Phase-0 instrumentation: Ristretto metrics on so we
// can baseline hit / miss / eviction-cost via Stats(); the engine
// bootstrap launches a background log emitter (every 5 minutes)
// when the cache is non-empty.
//
// Invalidation (5.4): caching a result is unsafe without a way to
// drop it the moment a row it depends on is written. resultCache
// maintains a concept -> cached-plan-keys dependency index keyed by
// the concept(s) a cached plan reads. On a graph write to a concept
// the engine's event-bus subscriber calls evictConcept, which drops
// exactly the dependent keys (index-keyed eviction, never a full
// cache scan). The index is the source of truth for "which keys does
// concept C feed"; Ristretto's own eviction (LFU / TTL) can drop a
// key from under us, so the index is treated as best-effort (a stale
// index entry whose key is already gone is harmless) and pruned on
// the eviction sweep.
type resultCache struct {
	cache *ristretto.Cache
	mu    sync.RWMutex

	// depIndex maps a concept id (e.g. "v1:cognition:utterance") to
	// the set of cache keys whose cached result depends on that
	// concept. Guarded by depMu, separate from mu so an eviction
	// sweep doesn't hold the Ristretto get/set hot-path lock.
	depMu    sync.Mutex
	depIndex map[string]map[string]struct{}
}

// ResultCacheStats exposes a snapshot of Ristretto's internal
// metrics for the query result cache. Counters are since process
// start; subtract two snapshots to get a rate over an interval.
type ResultCacheStats struct {
	Hits        uint64  `json:"hits"`
	Misses      uint64  `json:"misses"`
	HitRatio    float64 `json:"hitRatio"`
	KeysAdded   uint64  `json:"keysAdded"`
	KeysEvicted uint64  `json:"keysEvicted"`
	CostAdded   uint64  `json:"costAdded"`
	CostEvicted uint64  `json:"costEvicted"`
}

func newResultCache(size int64) (*resultCache, error) {
	if size <= 0 {
		return nil, nil
	}

	cfg := &ristretto.Config{
		NumCounters: size * 10,
		MaxCost:     size,
		BufferItems: 64,
		// Phase-0 plan: enable so hit/miss/eviction is observable.
		// See docs/internal/planning/cache-audit-phase-0.md.
		Metrics: true,
	}

	rc, err := ristretto.NewCache(cfg)
	if err != nil {
		return nil, err
	}

	return &resultCache{
		cache:    rc,
		depIndex: make(map[string]map[string]struct{}),
	}, nil
}

func (c *resultCache) get(key string) (*ExecuteResult, bool) {
	if c == nil || c.cache == nil {
		return nil, false
	}

	c.mu.RLock()
	defer c.mu.RUnlock()

	value, ok := c.cache.Get(key)
	if !ok {
		return nil, false
	}

	tree, ok := value.(*ExecuteResult)
	if !ok || tree == nil {
		return nil, false
	}

	return cloneExecuteResult(tree), true
}

// set stores the result under key for ttl and records the concepts
// the cached plan reads so a later write to any of them can evict
// this key. concepts is the dependency set the engine resolves from
// the plan (its bound/filter concept) and the returned bundle's rows;
// an empty concepts slice still caches the row but leaves it
// invalidation-blind, so the engine only caches when it can name at
// least one dependency concept.
func (c *resultCache) set(key string, tree *ExecuteResult, ttl time.Duration, concepts []string) {
	if c == nil || c.cache == nil || tree == nil || ttl <= 0 {
		return
	}

	copy := cloneExecuteResult(tree)
	if copy == nil {
		return
	}

	c.mu.Lock()
	c.cache.SetWithTTL(key, copy, 1, ttl)
	c.mu.Unlock()

	c.recordDependencies(key, concepts)
}

// recordDependencies adds key to the dependency set of every concept
// it reads. Cheap map inserts under depMu; no Ristretto contact.
func (c *resultCache) recordDependencies(key string, concepts []string) {
	if c == nil || len(concepts) == 0 {
		return
	}

	c.depMu.Lock()
	defer c.depMu.Unlock()

	if c.depIndex == nil {
		c.depIndex = make(map[string]map[string]struct{})
	}
	for _, concept := range concepts {
		if concept == "" {
			continue
		}
		keys := c.depIndex[concept]
		if keys == nil {
			keys = make(map[string]struct{})
			c.depIndex[concept] = keys
		}
		keys[key] = struct{}{}
	}
}

// evictConcept drops every cached result that depends on the given
// concept and returns the number of keys evicted. Index-keyed: it
// touches only the keys recorded for that concept, never the whole
// cache. Safe to call on every node holding a cache; a no-op when no
// cached plan reads the concept. Called from the engine's graph-write
// event subscriber, which fires on every replica (local writes and
// mesh-forwarded remote writes both republish onto the local bus).
func (c *resultCache) evictConcept(concept string) int {
	if c == nil || c.cache == nil || concept == "" {
		return 0
	}

	c.depMu.Lock()
	keys := c.depIndex[concept]
	delete(c.depIndex, concept)
	c.depMu.Unlock()

	if len(keys) == 0 {
		return 0
	}

	c.mu.Lock()
	for key := range keys {
		c.cache.Del(key)
	}
	c.mu.Unlock()

	return len(keys)
}

func (c *resultCache) close() {
	if c == nil || c.cache == nil {
		return
	}
	c.cache.Close()

	c.depMu.Lock()
	c.depIndex = make(map[string]map[string]struct{})
	c.depMu.Unlock()
}

// Stats returns a point-in-time snapshot of Ristretto's metrics.
// Cheap (counter loads). Returns the zero value if Metrics wasn't
// enabled or the cache isn't initialised.
func (c *resultCache) Stats() ResultCacheStats {
	if c == nil || c.cache == nil {
		return ResultCacheStats{}
	}
	m := c.cache.Metrics
	if m == nil {
		return ResultCacheStats{}
	}
	hits := m.Hits()
	misses := m.Misses()
	total := hits + misses
	ratio := 0.0
	if total > 0 {
		ratio = float64(hits) / float64(total)
	}
	return ResultCacheStats{
		Hits:        hits,
		Misses:      misses,
		HitRatio:    ratio,
		KeysAdded:   m.KeysAdded(),
		KeysEvicted: m.KeysEvicted(),
		CostAdded:   m.CostAdded(),
		CostEvicted: m.CostEvicted(),
	}
}

// startStatsEmitter logs Stats() every interval, only when the
// cache has been touched (any counter non-zero). Cancellable via
// context. Spawned by the engine bootstrap.
func (c *resultCache) startStatsEmitter(ctx context.Context, logger *slog.Logger, interval time.Duration) {
	if c == nil || logger == nil {
		return
	}
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				stats := c.Stats()
				if stats.Hits == 0 && stats.Misses == 0 {
					continue
				}
				logger.Info("resultCache: stats",
					"hits", stats.Hits,
					"misses", stats.Misses,
					"hitRatio", stats.HitRatio,
					"keysAdded", stats.KeysAdded,
					"keysEvicted", stats.KeysEvicted,
					"costAdded", stats.CostAdded,
					"costEvicted", stats.CostEvicted,
				)
			}
		}
	}()
}
