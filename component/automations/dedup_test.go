package automations

import (
	"testing"
	"time"
)

func TestExecutionDedup_IsDuplicate(t *testing.T) {
	d := newExecutionDedup(1 * time.Minute)
	defer d.stop()

	automationName := "testAutomation"
	initialHead := "initial-head-123"
	execId := "exec-123"

	// Initially not a duplicate
	if d.isDuplicate(automationName, initialHead) {
		t.Error("should not be duplicate before registration")
	}

	// Register
	d.register(automationName, initialHead, execId)

	// Now it's a duplicate
	if !d.isDuplicate(automationName, initialHead) {
		t.Error("should be duplicate after registration")
	}

	// Different automation is not a duplicate
	if d.isDuplicate("otherAutomation", initialHead) {
		t.Error("different automation should not be duplicate")
	}

	// Different head is not a duplicate
	if d.isDuplicate(automationName, "different-head") {
		t.Error("different head should not be duplicate")
	}
}

func TestExecutionDedup_Expiration(t *testing.T) {
	// Very short TTL for testing
	d := newExecutionDedup(50 * time.Millisecond)
	defer d.stop()

	automationName := "testAutomation"
	initialHead := "initial-head-123"
	execId := "exec-123"

	d.register(automationName, initialHead, execId)

	if !d.isDuplicate(automationName, initialHead) {
		t.Error("should be duplicate immediately after registration")
	}

	// Wait for expiration
	time.Sleep(100 * time.Millisecond)

	if d.isDuplicate(automationName, initialHead) {
		t.Error("should not be duplicate after expiration")
	}
}

func TestExecutionDedup_NilSafe(t *testing.T) {
	var d *executionDedup

	// Should not panic
	if d.isDuplicate("test", "head") {
		t.Error("nil dedup should return false for isDuplicate")
	}

	// Should not panic
	d.register("test", "head", "exec")
}

func TestExecutionDedup_MultipleAutomations(t *testing.T) {
	d := newExecutionDedup(1 * time.Minute)
	defer d.stop()

	// Register multiple automations
	d.register("auto1", "head1", "exec1")
	d.register("auto2", "head2", "exec2")
	d.register("auto1", "head3", "exec3")

	if !d.isDuplicate("auto1", "head1") {
		t.Error("auto1/head1 should be duplicate")
	}
	if !d.isDuplicate("auto2", "head2") {
		t.Error("auto2/head2 should be duplicate")
	}
	if !d.isDuplicate("auto1", "head3") {
		t.Error("auto1/head3 should be duplicate")
	}

	// Cross-check: auto1 head doesn't affect auto2
	if d.isDuplicate("auto1", "head2") {
		t.Error("auto1/head2 should not be duplicate")
	}
}

func TestExecutionDedup_CleanupExpired(t *testing.T) {
	d := newExecutionDedup(100 * time.Millisecond)
	defer d.stop()

	// Register some entries
	d.register("auto1", "head1", "exec1")
	d.register("auto2", "head2", "exec2")

	// Verify they exist
	if !d.isDuplicate("auto1", "head1") {
		t.Error("auto1/head1 should exist")
	}

	// Wait for expiration and cleanup
	time.Sleep(200 * time.Millisecond)

	// Manually trigger cleanup
	d.cleanupExpired()

	// Verify they're gone
	d.mu.RLock()
	_, auto1Exists := d.seen["auto1"]
	_, auto2Exists := d.seen["auto2"]
	d.mu.RUnlock()

	if auto1Exists || auto2Exists {
		t.Error("expired entries should be cleaned up")
	}
}
