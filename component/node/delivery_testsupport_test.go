package node

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
)

// busSink collects the events a delivery re-publishes onto the local bus.
//
// It lived in chat_reply_delivery_test.go, which went with the chat-reply
// substrate (epic memql#4988); plan_delivery_test.go is now its only user.
type busSink struct {
	mu     sync.Mutex
	events []events.Event
}

func (s *busSink) publish(e events.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
}

func (s *busSink) snapshot() []events.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]events.Event(nil), s.events...)
}

// waitForEvents blocks until the sink holds at least n events, or fails.
func waitForEvents(t *testing.T, sink *busSink, n int) []events.Event {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got := sink.snapshot(); len(got) >= n {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d events; got %d", n, len(sink.snapshot()))
	return nil
}

// waitForCursor blocks until a consumer's durable cursor reaches want.
func waitForCursor(t *testing.T, store *fakeOutboxStore, key RoutingKey, consumer string, want int64) error {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cur, _ := store.LoadCursor(context.Background(), key, consumer); cur >= want {
			return nil
		}
		time.Sleep(5 * time.Millisecond)
	}
	cur, _ := store.LoadCursor(context.Background(), key, consumer)
	return fmt.Errorf("cursor=%d want>=%d", cur, want)
}

// testDeliveryConcept is a concept id the delivery tests build topics from. It
// was v1:library:artifact until that concept was removed (epic memql#4988);
// nothing about these tests depends on WHICH concept it is.
const testDeliveryConcept = "v1:library:artifact"
