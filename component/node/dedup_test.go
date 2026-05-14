package node

import "testing"

func TestEventDedup_BasicCheck(t *testing.T) {
	d := newEventDedup(4)

	if d.Check("evt-1") {
		t.Error("first check of evt-1 should return false (not seen)")
	}
	if !d.Check("evt-1") {
		t.Error("second check of evt-1 should return true (already seen)")
	}
}

func TestEventDedup_Contains(t *testing.T) {
	d := newEventDedup(4)

	d.Check("evt-1")

	if !d.Contains("evt-1") {
		t.Error("Contains should return true for recorded event")
	}
	if d.Contains("evt-2") {
		t.Error("Contains should return false for unknown event")
	}
}

func TestEventDedup_Eviction(t *testing.T) {
	d := newEventDedup(3) // capacity 3

	d.Check("evt-1")
	d.Check("evt-2")
	d.Check("evt-3")

	// All three should be present
	if !d.Contains("evt-1") || !d.Contains("evt-2") || !d.Contains("evt-3") {
		t.Error("all three events should be in buffer")
	}

	// Adding a 4th should evict evt-1
	d.Check("evt-4")

	if d.Contains("evt-1") {
		t.Error("evt-1 should be evicted after capacity exceeded")
	}
	if !d.Contains("evt-4") {
		t.Error("evt-4 should be in buffer")
	}
}

func TestEventDedup_WrapAround(t *testing.T) {
	d := newEventDedup(2)

	d.Check("a")
	d.Check("b")
	d.Check("c") // evicts "a"
	d.Check("d") // evicts "b"

	if d.Contains("a") || d.Contains("b") {
		t.Error("a and b should be evicted")
	}
	if !d.Contains("c") || !d.Contains("d") {
		t.Error("c and d should be present")
	}
}

func TestEventDedup_DefaultCapacity(t *testing.T) {
	d := newEventDedup(0) // should default to 4096
	if d.size != 4096 {
		t.Errorf("expected default capacity 4096, got %d", d.size)
	}
}
