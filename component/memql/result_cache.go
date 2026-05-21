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
type resultCache struct {
	cache *ristretto.Cache
	mu    sync.RWMutex
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
		// See docs/planning/cache-audit-phase-0.md.
		Metrics: true,
	}

	rc, err := ristretto.NewCache(cfg)
	if err != nil {
		return nil, err
	}

	return &resultCache{
		cache: rc,
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

func (c *resultCache) set(key string, tree *ExecuteResult, ttl time.Duration) {
	if c == nil || c.cache == nil || tree == nil || ttl <= 0 {
		return
	}

	copy := cloneExecuteResult(tree)
	if copy == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.cache.SetWithTTL(key, copy, 1, ttl)
}

func (c *resultCache) close() {
	if c == nil || c.cache == nil {
		return
	}
	c.cache.Close()
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
