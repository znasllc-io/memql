package workbench

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func waitCapacityQueue(t *testing.T, gate *capacityGate, count int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		gate.mu.Lock()
		got := len(gate.waiting)
		gate.mu.Unlock()
		if got == count {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("queue did not reach expected size")
}

func TestCapacityQueuesBeforeStartingAndCancellationCannotLeakASlot(t *testing.T) {
	g := newCapacityGate(1, 1, func() (bool, error) { return false, nil })
	release, err := g.acquire(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	answer := make(chan error, 1)
	go func() {
		done, err := g.acquire(ctx, false)
		if done != nil {
			done()
		}
		answer <- err
	}()
	waitCapacityQueue(t, g, 1)
	if _, err := g.acquire(context.Background(), false); !errors.Is(err, errWorkbenchQueueFull) {
		t.Fatalf("full queue = %v", err)
	}
	cancel()
	if err := <-answer; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel = %v", err)
	}
	release()
	release() // Cleanup is idempotent, not another free slot.
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done, err := g.acquire(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	done()
	if g.active != 0 || len(g.waiting) != 0 {
		t.Fatal("admission leaked")
	}
}

func TestCapacityBuildWaitsForAllCommandsAndKeepsItsFIFOPlace(t *testing.T) {
	g := newCapacityGate(2, 8, func() (bool, error) { return false, nil })
	first, _ := g.acquire(context.Background(), false)
	second, _ := g.acquire(context.Background(), false)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	build := make(chan func(), 1)
	go func() { release, _ := g.acquire(ctx, true); build <- release }()
	waitCapacityQueue(t, g, 1)
	first()
	select {
	case <-build:
		t.Fatal("build overlapped a running command")
	default:
	}
	later := make(chan func(), 1)
	go func() { release, _ := g.acquire(ctx, false); later <- release }()
	waitCapacityQueue(t, g, 2)
	second()
	release := <-build
	if release == nil {
		t.Fatal("build was never admitted")
	}
	select {
	case <-later:
		t.Fatal("later work bypassed the build")
	default:
	}
	release()
	release = <-later
	if release == nil {
		t.Fatal("command was never admitted")
	}
	release()
}

func TestCapacityPreservesMemoryCushionAndRefusesUnreadableUsage(t *testing.T) {
	var pressured atomic.Bool
	pressured.Store(true)
	g := newCapacityGate(2, 2, func() (bool, error) { return pressured.Load(), nil })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	answer := make(chan func(), 1)
	go func() { release, _ := g.acquire(ctx, false); answer <- release }()
	waitCapacityQueue(t, g, 1)
	select {
	case <-answer:
		t.Fatal("work started above the memory threshold")
	default:
	}
	pressured.Store(false)
	g.mu.Lock()
	g.signal()
	g.mu.Unlock()
	release := <-answer
	if release == nil {
		t.Fatal("work did not start after pressure cleared")
	}
	release()
	g.pressure = func() (bool, error) { return false, errors.New("unreadable") }
	if _, err := g.acquire(ctx, false); err == nil {
		t.Fatal("unreadable usage admitted work")
	}
}
