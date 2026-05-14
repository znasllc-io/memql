package events

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestBus_SubscribePublish(t *testing.T) {
	bus := NewBus()
	defer bus.Close()

	var received atomic.Int32
	var receivedEvent Event

	unsubscribe := bus.Subscribe("graph.node.created", func(e Event) {
		received.Add(1)
		receivedEvent = e
	})
	defer unsubscribe()

	event := NewEvent("graph.node.created", KindNodeCreated, map[string]any{
		"nodeId":  "test-123",
		"concept": "Skills",
	})

	bus.PublishSync(event)

	if got := received.Load(); got != 1 {
		t.Errorf("expected 1 event, got %d", got)
	}

	if receivedEvent.Topic != "graph.node.created" {
		t.Errorf("expected topic 'graph.node.created', got %q", receivedEvent.Topic)
	}

	if receivedEvent.Kind != KindNodeCreated {
		t.Errorf("expected kind KindNodeCreated, got %v", receivedEvent.Kind)
	}

	if receivedEvent.Payload["nodeId"] != "test-123" {
		t.Errorf("expected nodeId 'test-123', got %v", receivedEvent.Payload["nodeId"])
	}
}

func TestBus_PatternMatching(t *testing.T) {
	bus := NewBus()
	defer bus.Close()

	var starCount, hashCount, exactCount atomic.Int32

	bus.Subscribe("graph.node.*", func(e Event) {
		starCount.Add(1)
	})

	bus.Subscribe("graph.#", func(e Event) {
		hashCount.Add(1)
	})

	bus.Subscribe("graph.node.created", func(e Event) {
		exactCount.Add(1)
	})

	// This should match all three
	bus.PublishSync(NewEvent("graph.node.created", KindNodeCreated, nil))

	if got := starCount.Load(); got != 1 {
		t.Errorf("star pattern: expected 1, got %d", got)
	}

	if got := hashCount.Load(); got != 1 {
		t.Errorf("hash pattern: expected 1, got %d", got)
	}

	if got := exactCount.Load(); got != 1 {
		t.Errorf("exact pattern: expected 1, got %d", got)
	}

	// This should match star and hash, but not exact
	bus.PublishSync(NewEvent("graph.node.deleted", KindNodeDeleted, nil))

	if got := starCount.Load(); got != 2 {
		t.Errorf("star pattern after second: expected 2, got %d", got)
	}

	if got := hashCount.Load(); got != 2 {
		t.Errorf("hash pattern after second: expected 2, got %d", got)
	}

	if got := exactCount.Load(); got != 1 {
		t.Errorf("exact pattern after second: expected 1, got %d", got)
	}
}

func TestBus_Unsubscribe(t *testing.T) {
	bus := NewBus()
	defer bus.Close()

	var count atomic.Int32

	unsubscribe := bus.Subscribe("test.event", func(e Event) {
		count.Add(1)
	})

	bus.PublishSync(NewEvent("test.event", KindMessage, nil))

	if got := count.Load(); got != 1 {
		t.Errorf("before unsubscribe: expected 1, got %d", got)
	}

	unsubscribe()

	bus.PublishSync(NewEvent("test.event", KindMessage, nil))

	if got := count.Load(); got != 1 {
		t.Errorf("after unsubscribe: expected 1 (unchanged), got %d", got)
	}
}

func TestBus_Close(t *testing.T) {
	bus := NewBus()

	var count atomic.Int32

	bus.Subscribe("test.event", func(e Event) {
		count.Add(1)
	})

	bus.PublishSync(NewEvent("test.event", KindMessage, nil))

	if got := count.Load(); got != 1 {
		t.Errorf("before close: expected 1, got %d", got)
	}

	bus.Close()

	// After close, publishing should be a no-op
	bus.PublishSync(NewEvent("test.event", KindMessage, nil))

	if got := count.Load(); got != 1 {
		t.Errorf("after close: expected 1 (unchanged), got %d", got)
	}

	// Subscribing after close should return a no-op unsubscribe
	unsub := bus.Subscribe("test.event", func(e Event) {
		count.Add(1)
	})
	unsub() // Should not panic
}

func TestBus_ConcurrentPublish(t *testing.T) {
	bus := NewBus()
	defer bus.Close()

	var count atomic.Int32

	bus.Subscribe("#", func(e Event) {
		count.Add(1)
	})

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			bus.Publish(NewEvent("test.concurrent", KindMessage, map[string]any{"n": n}))
		}(i)
	}

	wg.Wait()

	// Give async handlers time to complete
	time.Sleep(100 * time.Millisecond)

	if got := count.Load(); got != 100 {
		t.Errorf("concurrent publish: expected 100, got %d", got)
	}
}

func TestBus_SubscriberCount(t *testing.T) {
	bus := NewBus()
	defer bus.Close()

	if got := bus.SubscriberCount(); got != 0 {
		t.Errorf("initial count: expected 0, got %d", got)
	}

	unsub1 := bus.Subscribe("test.a", func(e Event) {})
	unsub2 := bus.Subscribe("test.b", func(e Event) {})

	if got := bus.SubscriberCount(); got != 2 {
		t.Errorf("after 2 subscribes: expected 2, got %d", got)
	}

	unsub1()

	if got := bus.SubscriberCount(); got != 1 {
		t.Errorf("after 1 unsubscribe: expected 1, got %d", got)
	}

	unsub2()

	if got := bus.SubscriberCount(); got != 0 {
		t.Errorf("after all unsubscribed: expected 0, got %d", got)
	}
}

func TestBus_HandlerPanicRecovery(t *testing.T) {
	bus := NewBus()
	defer bus.Close()

	var goodHandlerCalled atomic.Bool

	// Handler that panics
	bus.Subscribe("test.panic", func(e Event) {
		panic("test panic")
	})

	// Handler that should still receive the event
	bus.Subscribe("test.panic", func(e Event) {
		goodHandlerCalled.Store(true)
	})

	// Should not panic the caller
	bus.PublishSync(NewEvent("test.panic", KindMessage, nil))

	if !goodHandlerCalled.Load() {
		t.Error("good handler should have been called despite other handler panicking")
	}
}

func TestEvent_Clone(t *testing.T) {
	original := NewEvent("test.event", KindMessage, map[string]any{
		"key": "value",
	}).WithMetadata("actor", "test-user")

	clone := original.Clone()

	// Modify original
	original.Payload["key"] = "modified"
	original.Metadata["actor"] = "modified-user"

	// Clone should be unaffected
	if clone.Payload["key"] != "value" {
		t.Errorf("clone payload was modified: got %v", clone.Payload["key"])
	}

	if clone.Metadata["actor"] != "test-user" {
		t.Errorf("clone metadata was modified: got %v", clone.Metadata["actor"])
	}
}

func TestEvent_WithMetadata(t *testing.T) {
	event := NewEvent("test.event", KindMessage, nil)
	event = event.WithMetadata("key1", "value1")
	event = event.WithMetadata("key2", "value2")

	if event.Metadata["key1"] != "value1" {
		t.Errorf("expected key1=value1, got %v", event.Metadata["key1"])
	}

	if event.Metadata["key2"] != "value2" {
		t.Errorf("expected key2=value2, got %v", event.Metadata["key2"])
	}
}

func TestKind_String(t *testing.T) {
	tests := []struct {
		kind Kind
		want string
	}{
		{KindUnspecified, "unspecified"},
		{KindNodeCreated, "node_created"},
		{KindNodeDeleted, "node_deleted"},
		{KindQueryExecuted, "query_executed"},
		{KindSessionOpened, "session_opened"},
		{Kind(999), "unspecified"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := tt.kind.String(); got != tt.want {
				t.Errorf("Kind(%d).String() = %q, want %q", tt.kind, got, tt.want)
			}
		})
	}
}
