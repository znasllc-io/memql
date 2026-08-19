package memql

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// aiResponseCache memoises LLM call results so repeated invocations
// of the same template + provider + fully-rendered prompt return
// instantly. Hash-keyed (so "thanks!" vs "thanks" miss each other --
// the new vector-classification cache primitive in Phase 1 of the
// llm-driven-decisions plan addresses that for high-redundancy
// classification calls).
//
// Phase-0 instrumentation: atomic counters + a Stats() snapshot + a
// background log emitter so we can baseline hit rates over a week
// of dev usage.
//
// BOUNDED since memql#4124. It was an unbounded map whose only eviction
// was lazy: an expired entry was deleted when a read happened to land on
// it, so an entry nobody ever read again was never reclaimed. That is the
// common case here rather than the rare one -- the key is a hash of the
// FULLY RENDERED prompt (buildAICacheKey), which folds in conversation
// history, so most keys are written once and never looked up again. The
// map therefore grew with total LLM call volume and shrank only by
// coincidence. See evictLocked for the policy and why it is not LRU.
type aiResponseCache struct {
	mu      sync.RWMutex
	entries map[string]aiCacheEntry

	// maxEntries bounds the live entry count (memql#4124). <=0 is
	// unbounded -- the pre-#4124 behaviour, reachable only as an explicit
	// operator override via MEMQL_AI_CACHE_MAX_ENTRIES.
	maxEntries int

	// Telemetry counters. Atomic so the Stats() snapshot doesn't
	// need to grab the entries mutex (it just reads counters + a
	// short read-locked size).
	hits            atomic.Int64
	misses          atomic.Int64
	expiredOnRead   atomic.Int64 // entries deleted because the read found them past TTL
	sets            atomic.Int64
	skippedSetsZero atomic.Int64 // set() called with ttl<=0
	sweptExpired    atomic.Int64 // entries reclaimed by the at-capacity sweep
	evictedAtCap    atomic.Int64 // live entries dropped because the cap still bound after a sweep
}

type aiCacheEntry struct {
	value     any
	expiresAt time.Time
}

// AICacheStats is the Phase-0 telemetry snapshot. Fields are
// counters since process start; subtract two snapshots to get a
// rate over an interval. `Size` is the live entry count at the
// moment Stats() ran.
//
// Stats are expected to be written periodically to logs (every 5
// minutes when the cache is non-empty) AND callable on-demand.
// A future debug HTTP endpoint surfaces them for live inspection.
type AICacheStats struct {
	Hits            int64   `json:"hits"`
	Misses          int64   `json:"misses"`
	ExpiredOnRead   int64   `json:"expiredOnRead"`
	Sets            int64   `json:"sets"`
	SkippedSetsZero int64   `json:"skippedSetsZero"`
	Size            int     `json:"size"`
	HitRatio        float64 `json:"hitRatio"`
	// SweptExpired and EvictedAtCap are the memql#4124 bound's telemetry.
	// A non-zero EvictedAtCap means live entries are being shed -- the cap
	// is binding, and either the workload or the cap wants a look.
	SweptExpired int64 `json:"sweptExpired"`
	EvictedAtCap int64 `json:"evictedAtCap"`
	MaxEntries   int   `json:"maxEntries"`
}

// newAIResponseCache builds the cache with an entry cap. maxEntries <=0
// disables the cap (unbounded), which only the explicit operator override
// selects.
func newAIResponseCache(maxEntries int) *aiResponseCache {
	return &aiResponseCache{
		entries:    make(map[string]aiCacheEntry),
		maxEntries: maxEntries,
	}
}

func (c *aiResponseCache) get(key string) (any, bool) {
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

func (c *aiResponseCache) set(key string, value any, ttl time.Duration) {
	if c == nil || ttl <= 0 {
		if c != nil {
			c.skippedSetsZero.Add(1)
		}
		return
	}
	c.mu.Lock()
	c.evictLocked(key)
	c.entries[key] = aiCacheEntry{
		value:     cloneInterface(value),
		expiresAt: time.Now().Add(ttl),
	}
	c.mu.Unlock()
	c.sets.Add(1)
}

// evictLocked makes room for one insertion. Caller holds c.mu for writing.
//
// Two stages, cheapest first:
//
//  1. Sweep every entry already past its TTL. These are dead by the
//     cache's own contract -- get() would refuse them -- so reclaiming
//     them costs nothing in hit rate. Under any steady workload this
//     stage alone keeps the map bounded, because the TTL is short
//     (60s by default) relative to how long it takes to write 5000 keys.
//  2. If the map is STILL at capacity, drop the live entry closest to
//     expiry. That entry has the least remaining value: it is the one
//     that would have been reclaimed first anyway.
//
// NOT LRU, deliberately. LRU needs a per-read write to recency
// bookkeeping, which would force get() from an RLock to a full Lock --
// turning the hot path (a cache HIT) from a shared read into a serialised
// write in order to improve an eviction decision that only matters on the
// cold path. Nearest-expiry ordering is already carried by the entries.
//
// incomingKey is exempt from the count: overwriting an existing key does
// not grow the map, so a repeated set on a full cache must not evict.
func (c *aiResponseCache) evictLocked(incomingKey string) {
	if c.maxEntries <= 0 {
		return // explicitly unbounded
	}
	if _, replacing := c.entries[incomingKey]; replacing {
		return
	}
	if len(c.entries) < c.maxEntries {
		return
	}

	now := time.Now()
	swept := 0
	for k, entry := range c.entries {
		if now.After(entry.expiresAt) {
			delete(c.entries, k)
			swept++
		}
	}
	if swept > 0 {
		c.sweptExpired.Add(int64(swept))
	}

	if len(c.entries) < c.maxEntries {
		return // the sweep was enough, which is the steady-state case
	}

	// Stage 2: shed live entries nearest to expiry, in a BATCH down to a
	// low-water mark rather than one per insertion.
	//
	// Batching is the difference between O(n) and O(log n) amortised per
	// set. Evicting exactly one victim would mean the next set at capacity
	// scans the whole map again -- an O(n) pass under the WRITE lock, on
	// every insertion, for as long as the cap keeps binding. Shedding 10%
	// at once amortises one O(n log n) sort over maxEntries/10 insertions,
	// and the entries given up are the ones nearest to expiry, so the cost
	// in hit rate is the lowest available.
	target := c.maxEntries - c.maxEntries/10
	if target < 1 {
		target = 1
	}

	type victim struct {
		key       string
		expiresAt time.Time
	}
	victims := make([]victim, 0, len(c.entries))
	for k, entry := range c.entries {
		victims = append(victims, victim{key: k, expiresAt: entry.expiresAt})
	}
	sort.Slice(victims, func(i, j int) bool {
		if victims[i].expiresAt.Equal(victims[j].expiresAt) {
			// Map iteration order is random, so ties must break on
			// something stable or the same workload evicts different
			// entries run to run -- which makes a hit-rate regression
			// impossible to reproduce.
			return victims[i].key < victims[j].key
		}
		return victims[i].expiresAt.Before(victims[j].expiresAt)
	})

	evicted := 0
	for _, v := range victims {
		if len(c.entries) <= target {
			break
		}
		delete(c.entries, v.key)
		evicted++
	}
	if evicted > 0 {
		c.evictedAtCap.Add(int64(evicted))
	}
}

// Stats returns a point-in-time snapshot of the cache's counters
// and live size. Cheap (counter loads + one short RLock for size).
func (c *aiResponseCache) Stats() AICacheStats {
	if c == nil {
		return AICacheStats{}
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

	return AICacheStats{
		Hits:            hits,
		Misses:          misses,
		ExpiredOnRead:   c.expiredOnRead.Load(),
		Sets:            c.sets.Load(),
		SkippedSetsZero: c.skippedSetsZero.Load(),
		Size:            size,
		HitRatio:        ratio,
		SweptExpired:    c.sweptExpired.Load(),
		EvictedAtCap:    c.evictedAtCap.Load(),
		MaxEntries:      c.maxEntries,
	}
}

// startStatsEmitter logs the cache's Stats() every interval, but
// only if the cache has been touched (entries > 0 OR any counter
// non-zero) -- silent caches stay silent in the log. Cancellable
// via the supplied context. Spawned by the engine bootstrap; one
// goroutine per process.
func (c *aiResponseCache) startStatsEmitter(ctx context.Context, logger *slog.Logger, interval time.Duration) {
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
				logger.Info("aiCache: stats",
					"hits", stats.Hits,
					"misses", stats.Misses,
					"hitRatio", stats.HitRatio,
					"expiredOnRead", stats.ExpiredOnRead,
					"sets", stats.Sets,
					"skippedSetsZero", stats.SkippedSetsZero,
					"size", stats.Size,
					"maxEntries", stats.MaxEntries,
					"sweptExpired", stats.SweptExpired,
					"evictedAtCap", stats.EvictedAtCap,
				)
			}
		}
	}()
}

func buildAICacheKey(templateId, provider, prompt string) string {
	input := strings.TrimSpace(templateId) + "|" + strings.TrimSpace(provider) + "|" + prompt
	return string(cacheIdEngine.FromString(input))
}
