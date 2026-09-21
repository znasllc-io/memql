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
	// flushes counts InvalidateAll, which a v1:shopify:store event triggers
	// (epic memql#5530). Counted SEPARATELY from calls: a flush is not an
	// eviction by name, and a subscriber that answered a store event by
	// evicting some hostname would be doing something else entirely.
	flushes int
}

func (s *stubInvalidator) Invalidate(hostname string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, hostname)
}

func (s *stubInvalidator) InvalidateAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushes++
}

func (s *stubInvalidator) flushCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flushes
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

// ---------------------------------------------------------------------------
// The bound store (epic memql#5530, issue memql#5538)
// ---------------------------------------------------------------------------

// storeEvent builds a graph.node.<verb>.v1:shopify:store event. The payload
// is a real store row's shape; the handler reads NONE of it, which is what
// the assertions below are about.
func storeEvent(verb string) events.Event {
	return events.Event{
		Topic: "graph.node." + verb + ".v1:shopify:store",
		Kind:  events.KindNodeUpdated,
		Payload: map[string]any{
			"id":      "v1:shopify:store:acme",
			"concept": "v1:shopify:store",
			"payload": map[string]any{
				"domain":             "acme.myshopify.com",
				"storefrontTokenRef": "acme_storefront_token",
			},
		},
	}
}

// A STORE WRITE FLUSHES THE WHOLE CACHE, on both verbs.
//
// The domain the policy admits and the token reference the runtime document
// resolves live on this row, and no site write accompanies an edit to it --
// so without this the operator's change reaches one replica at a time as each
// TTL expires.
func TestSiteInvalidationSubscriber_AStoreWriteFlushesEveryResolution(t *testing.T) {
	for _, verb := range []string{"created", "updated"} {
		t.Run(verb, func(t *testing.T) {
			bus := events.NewBus()
			inv := &stubInvalidator{}
			sub := NewSiteInvalidationSubscriber(nil, bus, inv)
			sub.Start(context.Background())
			defer sub.Stop(context.Background())

			bus.PublishSync(storeEvent(verb))

			if got := inv.flushCount(); got != 1 {
				t.Errorf("InvalidateAll called %d times on .%s, want 1", got, verb)
			}
			// NOT an eviction by name. A store row carries no hostname, so a
			// subscriber that reached for one would be evicting the empty key
			// -- which looks like it is working and evicts nothing.
			if got := inv.callCount(); got != 0 {
				t.Errorf("Invalidate(hostname) called %d times for a store write, want 0", got)
			}
		})
	}
}

// A store event carrying nothing readable still flushes -- the handler reads
// no field, so there is no malformed case for it to drop.
func TestSiteInvalidationSubscriber_AStoreEventNeedsNoPayload(t *testing.T) {
	bus := events.NewBus()
	inv := &stubInvalidator{}
	sub := NewSiteInvalidationSubscriber(nil, bus, inv)
	sub.Start(context.Background())
	defer sub.Stop(context.Background())

	bus.PublishSync(events.Event{Topic: "graph.node.updated.v1:shopify:store", Kind: events.KindNodeUpdated})

	if got := inv.flushCount(); got != 1 {
		t.Errorf("InvalidateAll called %d times for a payload-less store event, want 1", got)
	}
}

// A SIBLING SHOPIFY CONCEPT MUST NOT FLUSH. The mirrored catalog is written
// by the connector in bulk -- one flush per product row would empty the
// resolver cache on every edge replica for the length of a sync.
func TestSiteInvalidationSubscriber_IgnoresOtherShopifyConcepts(t *testing.T) {
	bus := events.NewBus()
	inv := &stubInvalidator{}
	sub := NewSiteInvalidationSubscriber(nil, bus, inv)
	sub.Start(context.Background())
	defer sub.Stop(context.Background())

	bus.PublishSync(events.Event{
		Topic:   "graph.node.updated.v1:shopify:product",
		Kind:    events.KindNodeUpdated,
		Payload: map[string]any{"payload": map[string]any{"title": "A hat"}},
	})

	if got := inv.flushCount(); got != 0 {
		t.Errorf("InvalidateAll called %d times for a mirrored catalog row, want 0", got)
	}
	// The reachable positive, so the zero above is evidence about the pattern
	// rather than about a subscriber that never flushes at all.
	bus.PublishSync(storeEvent("updated"))
	if got := inv.flushCount(); got != 1 {
		t.Fatalf("control failed: a real store event flushed %d times, so the check above proves nothing", got)
	}
}

// A SITE EVENT MUST NOT FLUSH EVERYTHING. It names a hostname, and evicting
// that one entry is the whole reason the cache is keyed by hostname.
func TestSiteInvalidationSubscriber_ASiteWriteEvictsByNameNotEverything(t *testing.T) {
	bus := events.NewBus()
	inv := &stubInvalidator{}
	sub := NewSiteInvalidationSubscriber(nil, bus, inv)
	sub.Start(context.Background())
	defer sub.Stop(context.Background())

	bus.PublishSync(siteEvent("updated", "shop.example.com"))

	if got := inv.flushCount(); got != 0 {
		t.Errorf("InvalidateAll called %d times for a site write, want 0 -- a site names its own hostname", got)
	}
	if got := inv.callCount(); got != 1 {
		t.Errorf("Invalidate called %d times for a site write, want 1", got)
	}
}
