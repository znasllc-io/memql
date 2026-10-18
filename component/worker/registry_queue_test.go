package worker

import (
	"context"
	"errors"
	"testing"
	"time"
)

func queueWorker() (*Registry, *Worker) {
	r := NewRegistry(nil, nil)
	w := &Worker{RegistrationId: "queue-test", Concurrency: map[string]uint32{ModelCapability: 1, CapabilityHeadless: 1}}
	r.Add(w)
	return r, w
}
func awaitQueued(t *testing.T, w *Worker, count int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		w.mu.Lock()
		n := len(w.queue)
		w.mu.Unlock()
		if n == count {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("call did not reach queue")
}
func TestWorkerQueueDoesNotGrantAnotherCapabilitySlot(t *testing.T) {
	_, w := queueWorker()
	if err := w.Acquire(context.Background(), ModelCapability); err != nil {
		t.Fatal(err)
	}
	if err := w.Acquire(context.Background(), CapabilityHeadless); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- w.Acquire(ctx, ModelCapability) }()
	awaitQueued(t, w, 1)
	w.Release(CapabilityHeadless)
	select {
	case err := <-result:
		t.Fatalf("model admitted while model cap full: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	w.Release(ModelCapability)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	w.Release(ModelCapability)
}
func TestWorkerQueueDisconnectRefusesWaitingCalls(t *testing.T) {
	r, w := queueWorker()
	if err := w.Acquire(context.Background(), ModelCapability); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- w.Acquire(context.Background(), ModelCapability) }()
	awaitQueued(t, w, 1)
	r.Remove(w.RegistrationId)
	if err := <-result; !errors.Is(err, ErrWorkerDisconnected) {
		t.Fatalf("disconnected worker admitted call: %v", err)
	}
}

func TestWorkerModelQueueHonorsBothCapsAndReservesBeforeWake(t *testing.T) {
	_, w := queueWorker()
	w.Concurrency[ModelCapability] = 2
	if err := w.AcquireModel(context.Background(), "chat", 1); err != nil {
		t.Fatal(err)
	}
	second := make(chan error, 1)
	go func() { second <- w.AcquireModel(context.Background(), "chat", 1) }()
	awaitQueued(t, w, 1)
	// Another model may use the other machine slot while chat waits.
	if err := w.AcquireModel(context.Background(), "embedding", 1); err != nil {
		t.Fatal(err)
	}
	w.ReleaseModel("embedding")
	awaitQueued(t, w, 1)
	w.ReleaseModel("chat")
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if got := w.ActiveCount(); got != 1 {
		t.Fatalf("reserved slots=%d", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	third := make(chan error, 1)
	go func() { third <- w.AcquireModel(ctx, "chat", 1) }()
	awaitQueued(t, w, 1)
	cancel()
	if err := <-third; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel=%v", err)
	}
	w.ReleaseModel("chat")
	if w.ActiveCount() != 0 {
		t.Fatal("cancelled waiter leaked a slot")
	}
}

func TestWorkerQueueCancellationRacingGrantDoesNotLeak(t *testing.T) {
	for n := 0; n < 100; n++ {
		_, w := queueWorker()
		if err := w.AcquireModel(context.Background(), "chat", 1); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- w.AcquireModel(ctx, "chat", 1) }()
		awaitQueued(t, w, 1)
		go cancel()
		w.ReleaseModel("chat")
		if err := <-done; err == nil {
			w.ReleaseModel("chat")
		} else if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if w.ActiveCount() != 0 {
			t.Fatal("grant/cancel race leaked capacity")
		}
	}
}
