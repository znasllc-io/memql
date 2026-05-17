package automations

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/znasllc-io/memql/core/id"
)

// StepCache provides step-level memoization for automation steps.
// Caches step results keyed by (scope, stepDefinition, input) to skip
// redundant computation when the same step runs with the same input.
type StepCache struct {
	engine *id.Engine

	mu       sync.RWMutex
	entries  map[id.ID]*cacheEntry
	order    []id.ID // insertion order for LRU eviction
	curBytes int64
	maxBytes int64
}

// cacheEntry holds a cached step result with metadata.
type cacheEntry struct {
	result    *StepResult
	createdAt int64
	ttl       int64 // nanoseconds; 0 means no expiration
	size      int64 // estimated size in bytes
}

// NewStepCache creates a cache with the given memory limit.
// If maxBytes is 0, defaults to 256MB.
func NewStepCache(maxBytes int64) *StepCache {
	if maxBytes == 0 {
		maxBytes = 256 * 1024 * 1024 // 256MB default
	}
	return &StepCache{
		engine:   id.New(),
		entries:  make(map[id.ID]*cacheEntry),
		order:    make([]id.ID, 0, 1024),
		maxBytes: maxBytes,
	}
}

// Engine returns the id.Engine used for cache key computation.
func (c *StepCache) Engine() *id.Engine {
	return c.engine
}

// Key computes the cache key from scope, step definition, and input.
// The key is deterministic: same inputs always produce the same key.
func (c *StepCache) Key(scopeId, stepDefId, inputId id.ID) id.ID {
	return c.engine.Combine(c.engine.Combine(scopeId, stepDefId), inputId)
}

// Get retrieves a cached result if present and not expired.
// Returns a cloned result to prevent caller mutations from affecting the cache.
// Returns (result, true) on cache hit, (nil, false) on miss or expiration.
func (c *StepCache) Get(key id.ID) (*StepResult, bool) {
	c.mu.RLock()
	entry, ok := c.entries[key]
	c.mu.RUnlock()

	if !ok {
		return nil, false
	}

	// Check expiration
	if entry.ttl > 0 && time.Now().UnixNano() >= entry.createdAt+entry.ttl {
		c.mu.Lock()
		c.evictKey(key)
		c.mu.Unlock()
		return nil, false
	}

	// Clone the result to prevent caller mutations from affecting cached copy
	return cloneStepResult(entry.result), true
}

// Put stores a result with the given TTL.
// If ttl is 0, the result is cached indefinitely.
// Evicts oldest entries if the cache exceeds maxBytes.
// The result is cloned before storing to prevent caller mutations from affecting the cache.
func (c *StepCache) Put(key id.ID, result *StepResult, ttl time.Duration) {
	// Clone before storing to prevent caller mutations from affecting cached copy
	// (caller adds chain tracking fields after executeStep returns)
	cloned := cloneStepResult(result)
	size := estimateResultSize(cloned)

	c.mu.Lock()
	defer c.mu.Unlock()

	// If key already exists, subtract old size (old position in order slice
	// becomes stale and will be skipped during eviction)
	if old, exists := c.entries[key]; exists {
		c.curBytes -= old.size
	}

	// Always append key to order slice (moves to back if already present)
	// Stale positions are skipped in evictOldest since key won't be in entries
	c.order = append(c.order, key)

	// Evict oldest entries until we have space
	for c.curBytes+size > c.maxBytes && len(c.order) > 0 {
		c.evictOldest()
	}

	c.entries[key] = &cacheEntry{
		result:    cloned,
		createdAt: time.Now().UnixNano(),
		ttl:       int64(ttl),
		size:      size,
	}
	c.curBytes += size
}

// Clear removes all cached entries.
func (c *StepCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.entries = make(map[id.ID]*cacheEntry)
	c.order = make([]id.ID, 0, 1024)
	c.curBytes = 0
}

// Size returns the current estimated cache size in bytes.
func (c *StepCache) Size() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.curBytes
}

// Len returns the number of cached entries.
func (c *StepCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}

// evictOldest removes the oldest entry from the cache.
// Must be called with mu held.
func (c *StepCache) evictOldest() {
	if len(c.order) == 0 {
		return
	}

	// Find first valid key in order (skip already-evicted keys)
	for len(c.order) > 0 {
		key := c.order[0]
		c.order = c.order[1:]

		if entry, exists := c.entries[key]; exists {
			c.curBytes -= entry.size
			delete(c.entries, key)
			return
		}
	}
}

// evictKey removes a specific key from the cache and compacts order slice if needed.
// Must be called with mu held.
func (c *StepCache) evictKey(key id.ID) {
	if entry, exists := c.entries[key]; exists {
		c.curBytes -= entry.size
		delete(c.entries, key)
	}

	// Compact order slice if it has grown much larger than entries map
	// This prevents unbounded growth from TTL expirations
	if len(c.order) > len(c.entries)*2+100 {
		c.compactOrder()
	}
}

// compactOrder removes stale keys from the order slice.
// Must be called with mu held.
func (c *StepCache) compactOrder() {
	newOrder := make([]id.ID, 0, len(c.entries))
	for _, key := range c.order {
		if _, exists := c.entries[key]; exists {
			newOrder = append(newOrder, key)
		}
	}
	c.order = newOrder
}

// estimateResultSize returns an approximate size in bytes for a StepResult.
func estimateResultSize(result *StepResult) int64 {
	if result == nil {
		return 64 // base overhead
	}

	// Base struct size
	size := int64(256)

	// StepId and Status strings
	size += int64(len(result.StepId) + len(result.Status) + len(result.Error))

	// ContentId and chain fields
	size += int64(len(result.ContentId) + len(result.PreviousChainHead))

	// Estimate Result size via JSON marshaling (rough approximation)
	if result.Result != nil {
		if data, err := json.Marshal(result.Result); err == nil {
			size += int64(len(data))
		} else {
			size += 1024 // fallback estimate
		}
	}

	// Metadata
	if result.Metadata != nil {
		if data, err := json.Marshal(result.Metadata); err == nil {
			size += int64(len(data))
		}
	}

	// Children (recursive estimation would be expensive, use rough estimate)
	size += int64(len(result.Children) * 512)
	size += int64(len(result.ChildFingerprints) * 64)

	return size
}

// cloneStepResult creates a shallow copy of a StepResult.
// This prevents mutations to chain tracking fields (ContentId, PreviousChainHead)
// from affecting the cached copy.
func cloneStepResult(r *StepResult) *StepResult {
	if r == nil {
		return nil
	}

	clone := &StepResult{
		StepId:            r.StepId,
		Status:            r.Status,
		Result:            r.Result, // shallow copy is fine; Result isn't mutated
		Error:             r.Error,
		Duration:          r.Duration,
		StartedAt:         r.StartedAt,
		CompletedAt:       r.CompletedAt,
		ContentId:         r.ContentId,
		PreviousChainHead: r.PreviousChainHead,
	}

	// Clone slices to prevent mutation
	if r.Children != nil {
		clone.Children = make([]*StepResult, len(r.Children))
		copy(clone.Children, r.Children)
	}

	if r.ChildFingerprints != nil {
		clone.ChildFingerprints = make([]string, len(r.ChildFingerprints))
		copy(clone.ChildFingerprints, r.ChildFingerprints)
	}

	// Clone metadata map
	if r.Metadata != nil {
		clone.Metadata = make(map[string]any, len(r.Metadata))
		for k, v := range r.Metadata {
			clone.Metadata[k] = v
		}
	}

	return clone
}
