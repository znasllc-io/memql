package automations

import (
	"sync"
	"time"
)

// executionDedup tracks recent executions for duplicate detection.
// Keyed by automationName -> initialChainHead -> executionId.
type executionDedup struct {
	mu      sync.RWMutex
	seen    map[string]map[string]dedupEntry
	ttl     time.Duration
	cleanup *time.Ticker
	done    chan struct{}
}

type dedupEntry struct {
	identity  string
	inFlight  bool
	execId    string
	expiresAt time.Time
}

// newExecutionDedup creates a new deduplication tracker with the given TTL.
func newExecutionDedup(ttl time.Duration) *executionDedup {
	d := &executionDedup{
		seen:    make(map[string]map[string]dedupEntry),
		ttl:     ttl,
		cleanup: time.NewTicker(ttl / 2), // cleanup at half TTL interval
		done:    make(chan struct{}),
	}
	go d.cleanupLoop()
	return d
}

// isDuplicate checks if an execution with the given initial chain head
// has already been processed for this automation.
func (d *executionDedup) isDuplicate(automationName, initialHead string) bool {
	if d == nil {
		return false
	}

	d.mu.RLock()
	defer d.mu.RUnlock()

	byHead, ok := d.seen[automationName]
	if !ok {
		return false
	}

	entry, exists := byHead[initialHead]
	if !exists {
		return false
	}

	// Check if expired
	return time.Now().Before(entry.expiresAt)
}

// register records an execution for deduplication.
func (d *executionDedup) register(automationName, initialHead, execId string) {
	if d == nil {
		return
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if d.seen[automationName] == nil {
		d.seen[automationName] = make(map[string]dedupEntry)
	}
	identity := d.seen[automationName][initialHead].identity
	d.seen[automationName][initialHead] = dedupEntry{
		identity:  identity,
		execId:    execId,
		expiresAt: time.Now().Add(d.ttl),
	}
}

// claimOrDuplicate atomically registers a fire before any step executes.
func (d *executionDedup) claimOrDuplicate(name, key, identity, execId string) (bool, bool) {
	if d == nil {
		return false, false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.seen[name] == nil {
		d.seen[name] = map[string]dedupEntry{}
	}
	if v, ok := d.seen[name][key]; ok && (v.inFlight || time.Now().Before(v.expiresAt)) {
		return true, v.identity != identity
	}
	d.seen[name][key] = dedupEntry{identity: identity, execId: execId, inFlight: true, expiresAt: time.Now().Add(d.ttl)}
	return false, false
}
func (d *executionDedup) release(name, key, execId string) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if v, ok := d.seen[name][key]; ok && v.execId == execId {
		delete(d.seen[name], key)
		if len(d.seen[name]) == 0 {
			delete(d.seen, name)
		}
	}
}

// cleanupLoop periodically removes expired entries.
func (d *executionDedup) cleanupLoop() {
	for {
		select {
		case <-d.cleanup.C:
			d.cleanupExpired()
		case <-d.done:
			d.cleanup.Stop()
			return
		}
	}
}

// cleanupExpired removes all expired dedup entries.
func (d *executionDedup) cleanupExpired() {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := time.Now()
	for automationName, byHead := range d.seen {
		for head, entry := range byHead {
			if !entry.inFlight && now.After(entry.expiresAt) {
				delete(byHead, head)
			}
		}
		// Remove empty automation maps
		if len(byHead) == 0 {
			delete(d.seen, automationName)
		}
	}
}

// stop shuts down the cleanup goroutine.
func (d *executionDedup) stop() {
	if d != nil && d.done != nil {
		close(d.done)
	}
}
