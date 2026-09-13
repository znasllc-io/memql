package node

import (
	"testing"
	"time"
)

// A burst of heartbeat events must cost ONE reconcile pass, issued once the
// coalescing window closes -- not one pass per event. Before this window every
// v1:cluster:node heartbeat on the mesh queued a full node-list read on every
// pod (memql.znas.io, 2026-09-13).
func TestTriggerReconcileCoalescesABurstIntoOnePass(t *testing.T) {
	wd := &WorkerDialer{triggerDelay: 20 * time.Millisecond}
	ch := make(chan struct{}, 1)
	wd.mu.Lock()
	wd.trigger = ch
	wd.mu.Unlock()

	for i := 0; i < 50; i++ {
		wd.triggerReconcile()
	}

	select {
	case <-ch:
		t.Fatalf("a trigger was queued before the coalescing window closed")
	case <-time.After(5 * time.Millisecond):
	}

	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatalf("no reconcile was queued after the window closed")
	}
	select {
	case <-ch:
		t.Fatalf("the burst queued a second pass; want exactly one")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestTriggerReconcileWithoutAWindowQueuesImmediately(t *testing.T) {
	wd := &WorkerDialer{}
	ch := make(chan struct{}, 1)
	wd.mu.Lock()
	wd.trigger = ch
	wd.mu.Unlock()
	wd.triggerReconcile()
	select {
	case <-ch:
	default:
		t.Fatalf("a zero window must queue the reconcile at once")
	}
}
