package memql

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// siResponseCache memoises LLM call results so repeated invocations
// of the same template + provider + fully-rendered prompt return
// instantly. Hash-keyed (so "thanks!" vs "thanks" miss each other --
// the new vector-classification cache primitive in Phase 1 of the
// llm-driven-decisions plan addresses that for high-redundancy
// classification calls).
//
// Phase-0 instrumentation: atomic counters + a Stats() snapshot + a
// background log emitter so we can baseline hit rates over a week
// of dev usage. See docs/internal/planning/cache-audit-phase-0.md.
type siResponseCache struct {
	mu      sync.RWMutex
	entries map[string]siCacheEntry

	// Telemetry counters. Atomic so the Stats() snapshot doesn't
	// need to grab the entries mutex (it just reads counters + a
	// short read-locked size).
	hits            atomic.Int64
	misses          atomic.Int64
	expiredOnRead   atomic.Int64 // entries deleted because the read found them past TTL
	sets            atomic.Int64
	skippedSetsZero atomic.Int64 // set() called with ttl<=0
}

type siCacheEntry struct {
	value     any
	expiresAt time.Time
}

// SICacheStats is the Phase-0 telemetry snapshot. Fields are
// counters since process start; subtract two snapshots to get a
// rate over an interval. `Size` is the live entry count at the
// moment Stats() ran.
//
// Stats are expected to be written periodically to logs (every 5
// minutes when the cache is non-empty) AND callable on-demand.
// A future debug HTTP endpoint surfaces them for live inspection.
type SICacheStats struct {
	Hits            int64   `json:"hits"`
	Misses          int64   `json:"misses"`
	ExpiredOnRead   int64   `json:"expiredOnRead"`
	Sets            int64   `json:"sets"`
	SkippedSetsZero int64   `json:"skippedSetsZero"`
	Size            int     `json:"size"`
	HitRatio        float64 `json:"hitRatio"`
}

func newSIResponseCache() *siResponseCache {
	return &siResponseCache{
		entries: make(map[string]siCacheEntry),
	}
}

func (c *siResponseCache) get(key string) (any, bool) {
	if c == nil {
		return nil, false
	}
	now := time.Now()

	c.mu.RLock()
	entry, ok := c.entries[key]
	c.mu.RUnlock()
	if !ok {
		c.misses.Add(1)
		return nil, false
	}
	if now.After(entry.expiresAt) {
		c.mu.Lock()
		delete(c.entries, key)
		c.mu.Unlock()
		c.expiredOnRead.Add(1)
		c.misses.Add(1)
		return nil, false
	}
	c.hits.Add(1)
	return cloneInterface(entry.value), true
}

func (c *siResponseCache) set(key string, value any, ttl time.Duration) {
	if c == nil || ttl <= 0 {
		if c != nil {
			c.skippedSetsZero.Add(1)
		}
		return
	}
	c.mu.Lock()
	c.entries[key] = siCacheEntry{
		value:     cloneInterface(value),
		expiresAt: time.Now().Add(ttl),
	}
	c.mu.Unlock()
	c.sets.Add(1)
}

// Stats returns a point-in-time snapshot of the cache's counters
// and live size. Cheap (counter loads + one short RLock for size).
func (c *siResponseCache) Stats() SICacheStats {
	if c == nil {
		return SICacheStats{}
	}
	c.mu.RLock()
	size := len(c.entries)
	c.mu.RUnlock()

	hits := c.hits.Load()
	misses := c.misses.Load()
	total := hits + misses
	ratio := 0.0
	if total > 0 {
		ratio = float64(hits) / float64(total)
	}

	return SICacheStats{
		Hits:            hits,
		Misses:          misses,
		ExpiredOnRead:   c.expiredOnRead.Load(),
		Sets:            c.sets.Load(),
		SkippedSetsZero: c.skippedSetsZero.Load(),
		Size:            size,
		HitRatio:        ratio,
	}
}

// startStatsEmitter logs the cache's Stats() every interval, but
// only if the cache has been touched (entries > 0 OR any counter
// non-zero) -- silent caches stay silent in the log. Cancellable
// via the supplied context. Spawned by the engine bootstrap; one
// goroutine per process.
func (c *siResponseCache) startStatsEmitter(ctx context.Context, logger *slog.Logger, interval time.Duration) {
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
				if stats.Size == 0 && stats.Hits == 0 && stats.Misses == 0 {
					continue
				}
				logger.Info("siCache: stats",
					"hits", stats.Hits,
					"misses", stats.Misses,
					"hitRatio", stats.HitRatio,
					"expiredOnRead", stats.ExpiredOnRead,
					"sets", stats.Sets,
					"skippedSetsZero", stats.SkippedSetsZero,
					"size", stats.Size,
				)
			}
		}
	}()
}

func buildSICacheKey(templateId, provider, prompt string) string {
	input := strings.TrimSpace(templateId) + "|" + strings.TrimSpace(provider) + "|" + prompt
	return string(cacheIdEngine.FromString(input))
}
