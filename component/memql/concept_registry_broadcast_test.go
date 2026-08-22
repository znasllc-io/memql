package memql

// concept_registry_broadcast_test.go -- the in-process registry-change
// broadcaster (memql#4238): the fan-out mechanics, the promote/demote emit
// points, and the load-bearing cross-node property.

import (
	"context"
	"testing"
	"time"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
)

// recvDelta reads one delta or fails after a short timeout, so a missing
// broadcast fails loud rather than hanging the suite.
func recvDelta(t *testing.T, ch <-chan ConceptRegistryDelta) ConceptRegistryDelta {
	t.Helper()
	select {
	case d := <-ch:
		return d
	case <-time.After(2 * time.Second):
		t.Fatal("expected a registry delta, got none within 2s")
		return ConceptRegistryDelta{}
	}
}

// noDelta asserts nothing arrives within a short window.
func noDelta(t *testing.T, ch <-chan ConceptRegistryDelta) {
	t.Helper()
	select {
	case d := <-ch:
		t.Fatalf("expected no delta, got %+v", d)
	case <-time.After(150 * time.Millisecond):
	}
}

// TestConceptRegistryBroadcaster_FanOutAndGeneration: every live subscriber
// gets every delta, and the generation increments by exactly one per emit.
func TestConceptRegistryBroadcaster_FanOutAndGeneration(t *testing.T) {
	e := &MemQLEngine{}

	a := e.SubscribeConceptRegistry()
	b := e.SubscribeConceptRegistry()
	defer a.Unsubscribe()
	defer b.Unsubscribe()

	if a.Generation != 0 || b.Generation != 0 {
		t.Fatalf("fresh broadcaster should start at generation 0, got a=%d b=%d", a.Generation, b.Generation)
	}

	e.broadcastConceptAdded(&memoryNodes.Concept{Name: "v1:trainingns:one"})
	da := recvDelta(t, a.Deltas)
	db := recvDelta(t, b.Deltas)
	if da.Generation != 1 || db.Generation != 1 {
		t.Fatalf("first delta should be generation 1, got a=%d b=%d", da.Generation, db.Generation)
	}
	if len(da.Added) != 1 || da.Added[0].Name != "v1:trainingns:one" {
		t.Fatalf("added delta lost its concept: %+v", da.Added)
	}

	e.broadcastConceptRemoved("v1:trainingns:one")
	da = recvDelta(t, a.Deltas)
	if da.Generation != 2 {
		t.Fatalf("second delta should be generation 2, got %d", da.Generation)
	}
	if len(da.Removed) != 1 || da.Removed[0] != "v1:trainingns:one" {
		t.Fatalf("removed delta lost its id: %+v", da.Removed)
	}
}

// TestConceptRegistryBroadcaster_SnapshotIsAtomicWithGeneration: a subscriber
// taken after N emits sees generation N and does NOT replay the earlier deltas.
func TestConceptRegistryBroadcaster_SnapshotIsAtomicWithGeneration(t *testing.T) {
	e := &MemQLEngine{}
	e.broadcastConceptAdded(&memoryNodes.Concept{Name: "v1:trainingns:pre"})
	e.broadcastConceptAdded(&memoryNodes.Concept{Name: "v1:trainingns:pre2"})

	sub := e.SubscribeConceptRegistry()
	defer sub.Unsubscribe()
	if sub.Generation != 2 {
		t.Fatalf("a subscriber taken after 2 emits should report generation 2, got %d", sub.Generation)
	}
	// No backlog replay: the channel is empty until the NEXT emit.
	noDelta(t, sub.Deltas)

	e.broadcastConceptAdded(&memoryNodes.Concept{Name: "v1:trainingns:post"})
	d := recvDelta(t, sub.Deltas)
	if d.Generation != 3 {
		t.Fatalf("next delta after generation 2 should be 3, got %d", d.Generation)
	}
}

// TestConceptRegistryBroadcaster_UnsubscribeStops: after Unsubscribe the channel
// is closed and no further delta is delivered.
func TestConceptRegistryBroadcaster_UnsubscribeStops(t *testing.T) {
	e := &MemQLEngine{}
	sub := e.SubscribeConceptRegistry()

	sub.Unsubscribe()
	// Unsubscribe closes the channel; a receive returns the zero value + false.
	select {
	case _, ok := <-sub.Deltas:
		if ok {
			t.Fatal("channel should be closed after Unsubscribe")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Unsubscribe should close the delta channel")
	}

	// Emitting after Unsubscribe must not panic (send on closed channel) -- the
	// subscriber is gone from the map under the same lock as emit.
	e.broadcastConceptAdded(&memoryNodes.Concept{Name: "v1:trainingns:after"})

	// Idempotent: a second Unsubscribe is a no-op, not a double-close panic.
	sub.Unsubscribe()
}

// TestConceptRegistryBroadcaster_DropOnFullIsGenerationDetectable: a subscriber
// that never drains has deltas dropped once its buffer fills, and the drop shows
// up as a generation gap the client uses to re-snapshot. This is the recovery
// contract, asserted at the mechanism.
func TestConceptRegistryBroadcaster_DropOnFullIsGenerationDetectable(t *testing.T) {
	e := &MemQLEngine{}
	sub := e.SubscribeConceptRegistry()
	defer sub.Unsubscribe()

	// Overrun the buffer without draining.
	total := conceptRegistryDeltaBuffer + 20
	for i := 0; i < total; i++ {
		e.broadcastConceptAdded(&memoryNodes.Concept{Name: "v1:trainingns:x"})
	}

	// Drain what is buffered. Deltas must be strictly increasing, and FEWER than
	// were emitted -- the overflow was dropped rather than blocking the emitter.
	var got []uint64
	for {
		select {
		case d := <-sub.Deltas:
			got = append(got, d.Generation)
			continue
		default:
		}
		break
	}
	if len(got) == 0 {
		t.Fatal("expected some buffered deltas")
	}
	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] {
			t.Fatalf("generations must be strictly increasing, got %v", got)
		}
	}
	if uint64(len(got)) >= uint64(total) {
		t.Fatalf("expected drops (delivered %d of %d emitted)", len(got), total)
	}

	// The recovery signal: the authoritative generation (what a fresh snapshot
	// reports) has run PAST the last delta this subscriber received. A client
	// comparing the two sees the gap and re-snapshots.
	fresh := e.SubscribeConceptRegistry()
	defer fresh.Unsubscribe()
	lastDelivered := got[len(got)-1]
	if fresh.Generation != uint64(total) {
		t.Fatalf("a fresh snapshot should report the authoritative generation %d, got %d", total, fresh.Generation)
	}
	if fresh.Generation <= lastDelivered {
		t.Fatalf("the authoritative generation %d must exceed the last delivered %d for the gap to be detectable", fresh.Generation, lastDelivered)
	}
}

// --- promote / demote emit points, over a real engine + registry -------------

// TestConceptRegistry_PromoteEmitsAdded: a concept promote through the real
// engine path fires an Added delta naming the promoted id.
func TestConceptRegistry_PromoteEmitsAdded(t *testing.T) {
	e := promoteConceptEngine(t)
	sub := e.SubscribeConceptRegistry()
	defer sub.Unsubscribe()

	if err := promoteConceptSource(t, e, trainedWidgetSrc, "trainedWidget"); err != nil {
		t.Fatalf("promote concept: %v", err)
	}
	d := recvDelta(t, sub.Deltas)
	if len(d.Added) != 1 || d.Added[0].Name != trainedWidgetId {
		t.Fatalf("promote should emit an Added delta for %q, got %+v", trainedWidgetId, d)
	}
	if d.Generation == 0 {
		t.Fatal("a delta must carry a non-zero generation")
	}
}

// TestConceptRegistry_DemoteRemoveEmitsRemoved: a zero-row demote removes the
// concept and fires a Removed delta; a subscriber taken AFTER the promote sees
// only the remove.
func TestConceptRegistry_DemoteRemoveEmitsRemoved(t *testing.T) {
	e := promoteConceptEngine(t)
	e.conceptRowCount = countingRows(0) // zero rows -> demote REMOVES

	if err := promoteConceptSource(t, e, trainedWidgetSrc, "trainedWidget"); err != nil {
		t.Fatalf("promote concept: %v", err)
	}

	sub := e.SubscribeConceptRegistry()
	defer sub.Unsubscribe()

	if _, err := e.demoteConceptFromLiveRegistry(context.Background(), "trainedWidget"); err != nil {
		t.Fatalf("demote concept: %v", err)
	}
	d := recvDelta(t, sub.Deltas)
	if len(d.Removed) != 1 || d.Removed[0] != trainedWidgetId {
		t.Fatalf("zero-row demote should emit a Removed delta for %q, got %+v", trainedWidgetId, d)
	}
}

// TestConceptRegistry_RetireDoesNotEmit: a demote with rows RETIRES the concept
// (still registered, readable) so the registry set is unchanged and nothing is
// broadcast.
func TestConceptRegistry_RetireDoesNotEmit(t *testing.T) {
	e := promoteConceptEngine(t)
	e.conceptRowCount = countingRows(5) // rows exist -> demote RETIRES

	if err := promoteConceptSource(t, e, trainedWidgetSrc, "trainedWidget"); err != nil {
		t.Fatalf("promote concept: %v", err)
	}

	sub := e.SubscribeConceptRegistry()
	defer sub.Unsubscribe()

	if _, err := e.demoteConceptFromLiveRegistry(context.Background(), "trainedWidget"); err != nil {
		t.Fatalf("demote concept: %v", err)
	}
	noDelta(t, sub.Deltas)
}

// TestConceptRegistryDelta_PropagatesCrossNode is the load-bearing cross-node
// test. Two SEPARATE engines are bridged by forwarding the authoring.promote.*
// broadcast from engine A's bus to engine B's bus -- exactly what the mesh
// EventBridge + the single broadcast routing rule do. A registry-delta
// subscriber on engine B (a stand-in for a client whose stream landed on
// replica B) must receive an Added delta when the concept is promoted on engine
// A, WITHOUT a restart, purely because B's promote-propagation subscriber
// re-registers the concept locally -- which fires B's own broadcaster.
//
// FAILS against single-node-assuming code: if the delta were emitted only on the
// node that HANDLED the promote, engine B's subscriber would re-register the
// concept (so it is callable) yet emit no delta, and a client on B would still
// need a reconnect. PASSES because the emit sits in promoteConceptIntoLiveRegistry,
// which the re-hydration path funnels through on B.
func TestConceptRegistryDelta_PropagatesCrossNode(t *testing.T) {
	busA := events.NewBus()
	busB := events.NewBus()
	t.Cleanup(func() { busA.Close(); busB.Close() })

	// Bridge A -> B, stamping an OriginNodeId so it reads as peer-bridged.
	busA.Subscribe("authoring.promote.#", func(ev events.Event) {
		ev.OriginNodeId = "node-A"
		busB.Publish(ev)
	})

	engA := promoteConceptEngine(t)
	engA.SetEventBus(busA)

	engB := promoteConceptEngine(t)
	engB.SetEventBus(busB)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// B's registry-delta subscriber -- the client on replica B.
	subB := engB.SubscribeConceptRegistry()
	defer subB.Unsubscribe()

	// B's promote-propagation subscriber -- the existing cross-node re-hydration
	// path (authoring_promote_propagate.go), driven by a store that serves the
	// promoted concept's row for whatever bundle id the broadcast carries.
	bStore := &crossNodeRehydrateStore{row: AuthoringConstructRow{
		OwnerUserId: "owner-1", Kind: "concept", Name: "trainedWidget", Source: trainedWidgetSrc,
	}}
	engB.startAuthoringPromoteSubscriberWithStore(ctx, bStore)

	// Promote on engine A (fake persist store, real bus broadcast).
	if err := engA.promoteConstructDurableWithStore(ctx, &fakePromoteStore{}, nil, "owner-1",
		mustAuthorConcept(t, "owner-1", trainedWidgetSrc, "trainedWidget")); err != nil {
		t.Fatalf("promote on engine A: %v", err)
	}

	// A's own client sees the delta (local path).
	// B's client sees it too, purely via the broadcast + re-hydration.
	d := recvDelta(t, subB.Deltas)
	if len(d.Added) != 1 || d.Added[0].Name != trainedWidgetId {
		t.Fatalf("engine B's registry-delta subscriber should have received an Added delta for %q, got %+v", trainedWidgetId, d)
	}
	if !engB.conceptIsRegistered(trainedWidgetId) {
		t.Fatal("engine B should have the concept registered after the broadcast")
	}
}

// mustAuthorConcept authors + compiles a single concept construct and returns
// it ready for promoteConstructDurableWithStore.
func mustAuthorConcept(t *testing.T, owner, source, name string) *AuthoredConstruct {
	t.Helper()
	reg := NewAuthoredRuntimeRegistry()
	if _, err := AuthorSessionBundle(reg, owner, source, ""); err != nil {
		t.Fatalf("author concept %q: %v", name, err)
	}
	c, ok := reg.Lookup(owner, "concept", name)
	if !ok {
		t.Fatalf("session define did not register concept %q", name)
	}
	return c
}

// conceptIsRegistered reports whether the engine's registry currently holds the
// id -- a small helper so the cross-node test can assert registration alongside
// the delta.
func (e *MemQLEngine) conceptIsRegistered(id string) bool {
	if e.concepts == nil {
		return false
	}
	c, err := e.concepts.Get(id)
	return err == nil && c != nil
}
