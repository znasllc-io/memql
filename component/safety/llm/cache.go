// Package llm is the semantic-classifier layer (#230) of the
// command-classifier epic (#226). It wraps a ChatStructuredProvider
// in a Classifier so the safety chain can fall back to an LLM
// verdict for actions the deterministic rule layer (#228) didn't
// decide.
//
// Architectural seam: this package depends on `core/common` (for
// the ChatStructuredProvider interface + StructuredSchema) and
// `component/safety` (for the descriptor + Classifier types). It
// does NOT import `component/memql` -- the provider is injected by
// app boot, mirroring how #229 designed the gate to be swapped via
// `safety.SetDefaultGate`. That keeps the safety stack engine-
// independent and avoids a cycle with `component/memql/tool_execution.go`,
// which imports `component/safety`.
package llm

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/safety"
)

// cacheEntry is one cached classification + its expiry.
type cacheEntry struct {
	cls       safety.Classification
	expiresAt time.Time
}

// verdictCache is a TTL-bounded in-memory cache of classification
// verdicts keyed by a stable hash of (surface, action, normalised
// payload). It exists to keep the LLM out of the hot path on
// repeated identical actions -- agent loops re-issue the same
// `ls -la /tmp` dozens of times in a turn, and the verdict for one
// `ls -la /tmp` is the verdict for all of them.
//
// Bounded by `maxEntries`: when adding a new entry would overflow,
// the oldest-expiring entries get swept in a single pass. No LRU
// recency tracking -- the LLM verdict for a command is cheap to
// recompute and the dominant evictor is the TTL.
type verdictCache struct {
	mu         sync.RWMutex
	entries    map[string]cacheEntry
	ttl        time.Duration
	maxEntries int
	now        func() time.Time // injectable for tests
}

// newVerdictCache returns a cache with the given TTL and a soft
// upper bound on entry count. ttl<=0 disables caching (Get always
// misses, Put is a no-op). maxEntries<=0 means "unbounded" -- not
// recommended in production; #235's rollout config sets a real cap.
func newVerdictCache(ttl time.Duration, maxEntries int) *verdictCache {
	return &verdictCache{
		entries:    make(map[string]cacheEntry),
		ttl:        ttl,
		maxEntries: maxEntries,
		now:        time.Now,
	}
}

// Get returns the cached verdict if one exists and hasn't expired.
// A cache hit stamps Source=SourceCache + LatencyMs=0 on the
// returned Classification before handing it back, so the recorder
// can tell rule / model / cache verdicts apart.
func (c *verdictCache) Get(key string) (safety.Classification, bool) {
	if c.ttl <= 0 {
		return safety.Classification{}, false
	}
	c.mu.RLock()
	entry, ok := c.entries[key]
	c.mu.RUnlock()
	if !ok {
		return safety.Classification{}, false
	}
	if c.now().After(entry.expiresAt) {
		// Expired -- delete and miss. A racing Put may have
		// refreshed it; check again under the write lock.
		c.mu.Lock()
		if entry, ok := c.entries[key]; ok && c.now().After(entry.expiresAt) {
			delete(c.entries, key)
		}
		c.mu.Unlock()
		return safety.Classification{}, false
	}
	cls := entry.cls
	cls.Source = safety.SourceCache
	cls.LatencyMs = 0
	return cls, true
}

// Put records a verdict under `key` with the cache's TTL. Best-
// effort eviction: when adding would push the entry count past
// maxEntries, sweep every expired entry once; if still over,
// drop the new entry rather than evict a live one (the LLM will
// re-classify on the next miss -- correct, just a little slower).
func (c *verdictCache) Put(key string, cls safety.Classification) {
	if c.ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.maxEntries > 0 && len(c.entries) >= c.maxEntries {
		c.sweepExpiredLocked()
		if len(c.entries) >= c.maxEntries {
			return
		}
	}
	c.entries[key] = cacheEntry{cls: cls, expiresAt: c.now().Add(c.ttl)}
}

// Len returns the current entry count. Test + observability hook.
func (c *verdictCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}

// sweepExpiredLocked removes every entry whose expiresAt is in the
// past. Caller holds the write lock.
func (c *verdictCache) sweepExpiredLocked() {
	now := c.now()
	for k, e := range c.entries {
		if now.After(e.expiresAt) {
			delete(c.entries, k)
		}
	}
}

// descriptorCacheKey returns a stable hash of the fields the LLM
// verdict actually depends on -- surface, action, normalised
// payload. Caller / scope / capability are deliberately EXCLUDED:
// the verdict for "run `rm -rf /`" doesn't change based on which
// agent is asking. Including them would tank the hit rate without
// changing the answer.
//
// Normalisation:
//   - shell command: trimmed exact string (whitespace is significant
//     in shell, so don't collapse it).
//   - URLs / methods / tool names: lower-cased.
//   - paths: sorted (an FSAction over [/a, /b] == [/b, /a]).
//   - body: SHA-256'd into the key so even a multi-MiB body doesn't
//     bloat the key.
//
// Returns 32 hex chars (truncated SHA-256). Long enough for
// collision-free use at typical cache sizes (~millions).
func descriptorCacheKey(desc safety.ActionDescriptor) string {
	h := sha256.New()
	h.Write([]byte("v1\n"))
	h.Write([]byte(desc.Surface))
	h.Write([]byte{0})
	h.Write([]byte(desc.Action))
	h.Write([]byte{0})

	// Per-action normalisation.
	switch desc.Action {
	case safety.ActionExec:
		h.Write([]byte(strings.TrimSpace(desc.Payload.Command)))
	case safety.ActionHTTPFetch, safety.ActionWebhook:
		h.Write([]byte(strings.ToLower(strings.TrimSpace(desc.Payload.Method))))
		h.Write([]byte{0})
		h.Write([]byte(strings.ToLower(strings.TrimSpace(desc.Payload.URL))))
		h.Write([]byte{0})
		bodySum := sha256.Sum256([]byte(desc.Payload.Body))
		h.Write(bodySum[:])
	case safety.ActionFSRead, safety.ActionFSWrite, safety.ActionFSList, safety.ActionFSStat:
		sorted := append([]string(nil), desc.Payload.Paths...)
		sort.Strings(sorted)
		for _, p := range sorted {
			h.Write([]byte(p))
			h.Write([]byte{0})
		}
	case safety.ActionToolCall:
		h.Write([]byte(desc.Payload.ToolName))
		h.Write([]byte{0})
		// Args are intentionally NOT hashed -- the rule layer
		// already handles tool-specific cases, and arg-shaped LLM
		// verdicts cache poorly (sessionId, timestamps drift).
	default:
		// Unknown action: include the raw command + URL as best-
		// effort so we get SOME cache benefit if the same odd shape
		// repeats.
		h.Write([]byte(desc.Payload.Command))
		h.Write([]byte(desc.Payload.URL))
	}

	return hex.EncodeToString(h.Sum(nil)[:16])
}
