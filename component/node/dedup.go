package node

import "sync"

// eventDedup is a fixed-size ring buffer that tracks recently seen event IDs
// to prevent re-forwarding events in the mesh. Thread-safe.
type eventDedup struct {
	mu    sync.Mutex
	ring  []string
	index int
	size  int
	seen  map[string]struct{}
}

// newEventDedup creates a dedup buffer with the given capacity.
func newEventDedup(capacity int) *eventDedup {
	if capacity <= 0 {
		capacity = 4096
	}
	return &eventDedup{
		ring: make([]string, capacity),
		size: capacity,
		seen: make(map[string]struct{}, capacity),
	}
}

// Check returns true if the event ID has already been seen.
// If not seen, it records the ID and returns false.
func (d *eventDedup) Check(eventId string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	if _, exists := d.seen[eventId]; exists {
		return true
	}

	// Evict the oldest entry if the ring is full
	if old := d.ring[d.index]; old != "" {
		delete(d.seen, old)
	}

	d.ring[d.index] = eventId
	d.seen[eventId] = struct{}{}
	d.index = (d.index + 1) % d.size

	return false
}

// Contains returns true if the event ID is in the dedup buffer.
func (d *eventDedup) Contains(eventId string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, exists := d.seen[eventId]
	return exists
}
