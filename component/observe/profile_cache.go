package observe

import (
	"sync"
	"time"
)

// profileEntry holds the live override applied to one fully-qualified
// code reference. expiresAt == zero means "no expiry"; the
// codeProfileExpiry sweep clears expired entries lazily so the cache
// stays small.
type profileEntry struct {
	level     Level
	expiresAt time.Time
}

// profileCache is the per-FQN override registry consulted by Method()
// / Func() before falling back to DefaultLevel. Populated by the
// codeProfile CDC subscriber (component/automations) when
// v1:observability:codeProfile rows arrive.
//
// The cache is process-local; the source of truth is the concept
// row in memQL. On restart the cache is empty until the CDC
// subscriber catches up.
type profileCache struct {
	mu      sync.RWMutex
	entries map[string]profileEntry
}

var globalProfiles = &profileCache{
	entries: make(map[string]profileEntry),
}

// SetProfile installs (or updates) a per-FQN level override. Called
// by the codeProfile CDC subscriber when a row is created or
// updated; safe to call concurrently. Passing LevelOff is the
// canonical way to disable an override -- the helper falls through
// to the default level, which is normally LevelOff too unless the
// process-wide default has been raised.
//
// expiresAt is honored at lookup time; the helper still consults
// SweepExpiredProfiles periodically to keep the entries map small.
// Passing the zero time disables expiry.
func SetProfile(fqn string, level Level, expiresAt time.Time) {
	if fqn == "" {
		return
	}
	globalProfiles.mu.Lock()
	globalProfiles.entries[fqn] = profileEntry{level: level, expiresAt: expiresAt}
	globalProfiles.mu.Unlock()
}

// ClearProfile removes the override for fqn. Equivalent to setting
// the level to off for callers that want to evict the row rather
// than leave a tombstone entry.
func ClearProfile(fqn string) {
	globalProfiles.mu.Lock()
	delete(globalProfiles.entries, fqn)
	globalProfiles.mu.Unlock()
}

// SweepExpiredProfiles removes entries whose expiresAt is in the
// past. The cron-style automation calls this on a fixed cadence
// (typically every minute). Returns the number of entries removed.
func SweepExpiredProfiles(now time.Time) int {
	globalProfiles.mu.Lock()
	defer globalProfiles.mu.Unlock()
	var removed int
	for k, v := range globalProfiles.entries {
		if !v.expiresAt.IsZero() && !now.Before(v.expiresAt) {
			delete(globalProfiles.entries, k)
			removed++
		}
	}
	return removed
}

// lookupProfile returns (level, ok) for fqn. ok is false when no
// override exists or the override has expired (in which case the
// helper falls through to DefaultLevel). The expiry check is done
// at lookup so an expired entry is invisible even before the next
// sweep runs.
func lookupProfile(fqn string) (Level, bool) {
	globalProfiles.mu.RLock()
	defer globalProfiles.mu.RUnlock()
	e, ok := globalProfiles.entries[fqn]
	if !ok {
		return LevelOff, false
	}
	if !e.expiresAt.IsZero() && nowFn().After(e.expiresAt) {
		return LevelOff, false
	}
	return e.level, true
}

// ProfileSize is intended for tests / metrics. Returns the current
// number of cached entries (including not-yet-swept expired ones).
func ProfileSize() int {
	globalProfiles.mu.RLock()
	defer globalProfiles.mu.RUnlock()
	return len(globalProfiles.entries)
}
