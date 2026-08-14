package edge

import (
	"context"
	"sync"
	"testing"

	"github.com/znasllc-io/memql/component/events"
)

// stubInvalidator records every hostname Invalidate was called with, guarded
// by a mutex since the bus delivers handlers from its own goroutine even
// under PublishSync (the handler runs synchronously, but on a goroutine the
// bus spawns for delivery bookkeeping in some code paths -- guarding is
// cheap and removes any doubt).
type stubInvalidator struct {
	mu    sync.Mutex
	calls []string
}

func (s *stubInvalidator) Invalidate(hostname string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, hostname)
}

func (s *stubInvalidator) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (s *stubInvalidator) last() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.calls) == 0 {
		return ""
	}
	return s.calls[len(s.calls)-1]
}

// siteEvent builds a graph.node.<verb>.v1:platform:site event carrying the
// full merged row under "payload", matching the shape
// component/memql/executor_mutation.go actually publishes (flattened fields
// at top level too, but the subscriber only reads the nested "payload").
func siteEvent(verb, hostname string) events.Event {
	return events.Event{
		Topic: "graph.node." + verb + ".v1:platform:site",
		Kind:  events.KindNodeCreated,
		Payload: map[string]any{
			"id":      "v1:platform:site:abc123",
			"concept": "v1:platform:site",
			"payload": map[string]any{
				"hostname": hostname,
				"status":   "live",
			},
		},
	}
}

func TestSiteInvalidationSubscriber_CreatedInvalidates(t *testing.T) {
	bus := events.NewBus()
	inv := &stubInvalidator{}
	sub := NewSiteInvalidationSubscriber(nil, bus, inv)
	sub.Start(context.Background())
	defer sub.Stop(context.Background())

	bus.PublishSync(siteEvent("created", "shop.example.com"))

	if got := inv.callCount(); got != 1 {
		t.Fatalf("Invalidate called %d times, want 1", got)
	}
	if got := inv.last(); got != "shop.example.com" {
		t.Errorf("Invalidate called with %q, want shop.example.com", got)
	}
}

// The status flip (live -> disabled) and the bundle rollback
// (updateSiteBundle) both go through update(), not insert() -- so the
// subscriber MUST also fire on graph.node.updated, not only .created.
func TestSiteInvalidationSubscriber_UpdatedInvalidates(t *testing.T) {
	bus := events.NewBus()
	inv := &stubInvalidator{}
	sub := NewSiteInvalidationSubscriber(nil, bus, inv)
	sub.Start(context.Background())
	defer sub.Stop(context.Background())

	bus.PublishSync(siteEvent("updated", "shop.example.com"))

	if got := inv.callCount(); got != 1 {
		t.Fatalf("Invalidate called %d times on .updated, want 1", got)
	}
}

// A sibling concept's event must not trigger an invalidation -- the
// subscription is scoped to v1:platform:site only.
func TestSiteInvalidationSubscriber_IgnoresOtherConcepts(t *testing.T) {
	bus := events.NewBus()
	inv := &stubInvalidator{}
	sub := NewSiteInvalidationSubscriber(nil, bus, inv)
	sub.Start(context.Background())
	defer sub.Stop(context.Background())

	bus.PublishSync(events.Event{
		Topic: "graph.node.created.v1:platform:globalVariable",
		Kind:  events.KindNodeCreated,
		Payload: map[string]any{
			"payload": map[string]any{"name": "FOO", "value": "bar"},
		},
	})

	if got := inv.callCount(); got != 0 {
		t.Fatalf("Invalidate called %d times for an unrelated concept, want 0", got)
	}
}

// A malformed event (no nested payload object, or no hostname within it)
// must be dropped, not panic and not evict an empty-string cache key.
func TestSiteInvalidationSubscriber_ToleratesMalformedEvent(t *testing.T) {
	bus := events.NewBus()
	inv := &stubInvalidator{}
	sub := NewSiteInvalidationSubscriber(nil, bus, inv)
	sub.Start(context.Background())
	defer sub.Stop(context.Background())

	bus.PublishSync(events.Event{Topic: "graph.node.created.v1:platform:site", Kind: events.KindNodeCreated})
	bus.PublishSync(events.Event{
		Topic:   "graph.node.updated.v1:platform:site",
		Kind:    events.KindNodeUpdated,
		Payload: map[string]any{"payload": map[string]any{"status": "disabled"}}, // no hostname
	})

	if got := inv.callCount(); got != 0 {
		t.Fatalf("Invalidate called %d times for malformed events, want 0", got)
	}
}

// After Stop, the subscription must be torn down: an event published later
// must not still trigger the (potentially freed) resolver.
func TestSiteInvalidationSubscriber_StopUnsubscribes(t *testing.T) {
	bus := events.NewBus()
	inv := &stubInvalidator{}
	sub := NewSiteInvalidationSubscriber(nil, bus, inv)
	sub.Start(context.Background())

	bus.PublishSync(siteEvent("created", "shop.example.com"))
	if got := inv.callCount(); got != 1 {
		t.Fatalf("Invalidate called %d times before Stop, want 1", got)
	}

	sub.Stop(context.Background())
	if sub.IsRunning() {
		t.Error("IsRunning() true after Stop")
	}

	bus.PublishSync(siteEvent("updated", "shop.example.com"))
	if got := inv.callCount(); got != 1 {
		t.Fatalf("Invalidate called %d times after Stop, want still 1 (unsubscribed)", got)
	}
}

// A nil bus or nil resolver must not panic Start/Stop -- mirrors
// observe.CodeProfileSubscriber's own defensive nil-bus handling.
func TestSiteInvalidationSubscriber_NilBusDoesNotPanic(t *testing.T) {
	sub := NewSiteInvalidationSubscriber(nil, nil, &stubInvalidator{})
	sub.Start(context.Background())
	sub.Stop(context.Background())
	if sub.IsRunning() {
		t.Error("IsRunning() true with a nil bus")
	}
}
