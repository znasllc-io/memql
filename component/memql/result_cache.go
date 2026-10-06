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
	// concept. depMu also guards invalidation epochs. Code taking both locks
	// takes mu first; snapshot needs only depMu and never touches Ristretto.
	depMu    sync.Mutex
	depIndex map[string]map[string]struct{}

	// A query captures epoch before reading storage. An invalidation advances
	// it even when no dependent key has been admitted yet. This fences late
	// fills from in-flight reads and buffered Ristretto writes alike.
	epoch         uint64
	invalidatedAt map[string]uint64
}

type resultCacheEntry struct {
	result   *ExecuteResult
	epoch    uint64
	concepts []string
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
		Metrics: true,
	}

	rc, err := ristretto.NewCache(cfg)
	if err != nil {
		return nil, err
	}

	return &resultCache{
		cache:         rc,
		depIndex:      make(map[string]map[string]struct{}),
		invalidatedAt: make(map[string]uint64),
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

	entry, ok := value.(*resultCacheEntry)
	if !ok || entry == nil {
		return nil, false
	}
	c.depMu.Lock()
	valid := c.validAt(entry.epoch, entry.concepts)
	c.depMu.Unlock()
	if !valid {
		return nil, false
	}
	return cloneExecuteResult(entry.result), true
}

// snapshot is taken BEFORE querying storage, never when its result arrives.
func (c *resultCache) snapshot() uint64 {
	c.depMu.Lock()
	defer c.depMu.Unlock()
	return c.epoch
}

// set is for already-current synthetic values (including test seeds). Engine
// queries must use setAt with the snapshot captured before their storage read.
func (c *resultCache) set(key string, tree *ExecuteResult, ttl time.Duration, concepts []string) {
	if c == nil {
		return
	}
	c.setAt(key, tree, ttl, concepts, c.snapshot())
}

// validAt requires depMu. Only writes to the result's dependencies disqualify
// a fill; unrelated writes preserve both admitted entries and in-flight reads.
func (c *resultCache) validAt(epoch uint64, concepts []string) bool {
	for _, concept := range concepts {
		if c.invalidatedAt[concept] > epoch {
			return false
		}
	}
	return true
}

func (c *resultCache) setAt(key string, tree *ExecuteResult, ttl time.Duration, concepts []string, epoch uint64) {
	if c == nil || c.cache == nil || tree == nil || ttl <= 0 || len(concepts) == 0 {
		return
	}
	copy := cloneExecuteResult(tree)
	if copy == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.depMu.Lock()
	defer c.depMu.Unlock()
	if !c.validAt(epoch, concepts) {
		return
	}
	entry := &resultCacheEntry{result: copy, epoch: epoch, concepts: append([]string(nil), concepts...)}
	if !c.cache.SetWithTTL(key, entry, 1, ttl) {
		return
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
// cached plan reads the concept (but still fences in-flight reads). Called from the engine's graph-write
// event subscriber, which fires on every replica (local writes and
// mesh-forwarded remote writes both republish onto the local bus).
func (c *resultCache) evictConcept(concept string) int {
	if c == nil || c.cache == nil || concept == "" {
		return 0
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.depMu.Lock()
	c.epoch++
	c.invalidatedAt[concept] = c.epoch
	keys := c.depIndex[concept]
	delete(c.depIndex, concept)
	c.depMu.Unlock()

	if len(keys) == 0 {
		return 0
	}

	for key := range keys {
		c.cache.Del(key)
	}

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
