package node

import (
	"context"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
)

// TestResultCacheInvalidation_CrossNode proves the 5.6 cross-node
// guarantee: each replica runs its OWN result cache, so an eviction
// triggered by a write handled on replica A must ALSO evict the
// dependent cached result on replica B. It wires the real path -- two
// engines on two buses, the real EventBridge forward (A) + inbound
// republish (B), gated by the real routing rules -- not a single shared
// cache.
//
// 5.6 architecture (memql#1970). Eviction rides a DEDICATED broadcast
// channel: every graph write emits cache.invalidate.<concept>, ONLY the
// result-cache evictor subscribes to it, and the single
// cache.invalidate.* broadcast rule (in defaultRoutingRules) forwards it
// to every node. This test publishes that event -- NOT a graph.node.*
// write -- and registers NO per-concept routing rule: the broadcast rule
// already in the default set is the only forwarding the cache needs.
//
// WHY THIS FAILS AGAINST SINGLE-NODE-ASSUMING CODE. The eviction
// subscriber listens on each engine's LOCAL bus. A write on A only
// reaches B's local bus if a routing rule forwards the event across the
// mesh. Drop the cache.invalidate.* broadcast rule (or the inbound
// republish) and B keeps serving the stale cached result -- exactly the
// false-green single-node trap.
func TestResultCacheInvalidation_CrossNode(t *testing.T) {
	const concept = "v1:crossnodecachetest:widget"
	const cacheKey = "cross-node-key"

	// No per-concept routing rule is registered: 5.6 evicts cross-node
	// purely via the cache.invalidate.* broadcast rule baked into
	// defaultRoutingRules. This is the proof the per-concept rules are
	// unnecessary.

	// --- Replica A (where the write is handled) ---
	busA := events.NewBus(events.WithLogger(testLogger()))
	defer busA.Close()
	engineA, err := memqlengine.New(nil)
	if err != nil {
		t.Fatalf("engineA: %v", err)
	}
	engineA.SetEventBus(busA)

	idA := &Identity{ID: "replica-a", Type: NodeTypeBFF, Address: "a:50052"}
	pmA := NewPeerManager(idA, testLogger())
	ebA := NewEventBridge(idA, busA, pmA, testLogger())

	// --- Replica B (holds an independent cache) ---
	busB := events.NewBus(events.WithLogger(testLogger()))
	defer busB.Close()
	engineB, err := memqlengine.New(nil)
	if err != nil {
		t.Fatalf("engineB: %v", err)
	}
	engineB.SetEventBus(busB)

	idB := &Identity{ID: "replica-b", Type: NodeTypeBFF, Address: "b:50052"}
	pmB := NewPeerManager(idB, testLogger())
	ebB := NewEventBridge(idB, busB, pmB, testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engineA.StartCacheInvalidationSubscriber(ctx)
	engineB.StartCacheInvalidationSubscriber(ctx)

	// Mesh link A -> B: register B as a peer of A with an outbound
	// connection whose sendCh we drain, then feed each forwarded event
	// into B's inbound handler (the same path NodeServer.Stream drives in
	// production).
	pmA.Register(&nodev1.PeerInfo{
		NodeId:   idB.ID,
		NodeType: string(NodeTypeBFF),
		Address:  idB.Address,
		Health:   nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY,
	})
	conn := newPeerConnection(idA, idB.ID, idB.Address, testLogger())
	pmA.AttachConnection(idB.ID, conn)

	// Pump: forwards A sends to B land on B's inbound handler.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case msg := <-conn.sendCh:
				if fwd := msg.GetEventForward(); fwd != nil {
					ebB.HandleInbound(fwd)
				}
			}
		}
	}()

	// Both replicas have cached a result that depends on the concept.
	engineA.SeedResultCacheForInvalidationTest(cacheKey, concept)
	engineB.SeedResultCacheForInvalidationTest(cacheKey, concept)

	waitFor(t, "A cache populated", func() bool {
		return engineA.ResultCacheContainsForInvalidationTest(cacheKey)
	})
	waitFor(t, "B cache populated", func() bool {
		return engineB.ResultCacheContainsForInvalidationTest(cacheKey)
	})

	// A write to the concept is handled on replica A: it emits the
	// dedicated cache-invalidation event onto A's local bus (engine
	// subscriber evicts A) and the EventBridge forwards it to peers via the
	// cache.invalidate.* broadcast rule (-> B's inbound -> B's local bus ->
	// B's engine subscriber evicts B).
	writeEvent := events.NewEvent(
		events.TopicCacheInvalidateForConcept(concept),
		events.KindCacheInvalidate,
		map[string]any{"concept": concept},
	)
	busA.PublishSync(writeEvent) // local-bus eviction on A
	ebA.onLocalEvent(writeEvent) // mesh forward to B

	// Local replica evicts.
	waitFor(t, "A cache evicted", func() bool {
		return !engineA.ResultCacheContainsForInvalidationTest(cacheKey)
	})
	// CROSS-NODE: the remote replica's independent cache must evict too.
	if !waitForBool(func() bool {
		return !engineB.ResultCacheContainsForInvalidationTest(cacheKey)
	}) {
		t.Fatal("cross-node eviction FAILED: replica B still serves the stale cached result after a write handled on replica A")
	}
}

func waitForBool(cond func() bool) bool {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return false
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	if !waitForBool(cond) {
		t.Fatalf("timed out waiting for: %s", what)
	}
}

// TestResultCacheInvalidation_CrossNodeThroughTheWriteSeam is the companion
// to the test above, and the difference is the ENTRY POINT.
//
// That test hand-publishes the invalidation event and then hand-calls
// onLocalEvent, which proves the routing rule carries the channel. It cannot
// prove that a WRITE reaches that channel, because no write appears in it --
// if executeWrite stopped invalidating tomorrow, it would stay green. This
// one drives MemQLEngine.InvalidateCacheForConcept, the single seam
// executeWrite itself calls (memql#4531), so the two halves of the guarantee
// are gated together:
//
//   - LOCAL, SYNCHRONOUS: replica A's own entry is gone the moment the call
//     returns, with no polling. That is the read-your-writes property; a
//     poll here would pass against an eviction that rides the async bus,
//     which is exactly what must not happen (events.Bus.Publish gives each
//     subscriber its own goroutine).
//   - REMOTE, BROADCAST: replica B's independent cache evicts via the
//     cache.invalidate.* rule, and B's next read re-caches.
//
// It also pins the thing a broadcast makes easy to get wrong in the other
// direction: an unrelated concept cached on B must SURVIVE. A mesh-wide
// invalidation that over-evicts is not a correctness bug, so nothing else
// would ever catch it -- it just quietly makes every replica's cache useless
// on every write, which is the same defect memql#4531 removed locally.
func TestResultCacheInvalidation_CrossNodeThroughTheWriteSeam(t *testing.T) {
	const written = "v1:crossnodecachetest:written"
	const untouched = "v1:crossnodecachetest:untouched"
	const writtenKey = "written-key"
	const untouchedKey = "untouched-key"

	busA := events.NewBus(events.WithLogger(testLogger()))
	defer busA.Close()
	engineA, err := memqlengine.New(nil)
	if err != nil {
		t.Fatalf("engineA: %v", err)
	}
	engineA.SetEventBus(busA)

	idA := &Identity{ID: "seam-replica-a", Type: NodeTypeBFF, Address: "a:50052"}
	pmA := NewPeerManager(idA, testLogger())
	ebA := NewEventBridge(idA, busA, pmA, testLogger())

	busB := events.NewBus(events.WithLogger(testLogger()))
	defer busB.Close()
	engineB, err := memqlengine.New(nil)
	if err != nil {
		t.Fatalf("engineB: %v", err)
	}
	engineB.SetEventBus(busB)

	idB := &Identity{ID: "seam-replica-b", Type: NodeTypeBFF, Address: "b:50052"}
	pmB := NewPeerManager(idB, testLogger())
	ebB := NewEventBridge(idB, busB, pmB, testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engineA.StartCacheInvalidationSubscriber(ctx)
	engineB.StartCacheInvalidationSubscriber(ctx)

	pmA.Register(&nodev1.PeerInfo{
		NodeId:   idB.ID,
		NodeType: string(NodeTypeBFF),
		Address:  idB.Address,
		Health:   nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY,
	})
	conn := newPeerConnection(idA, idB.ID, idB.Address, testLogger())
	pmA.AttachConnection(idB.ID, conn)

	// A's local bus is the production forward trigger: EventBridge.run
	// subscribes "#" and calls onLocalEvent for everything. Subscribing it
	// here is what lets InvalidateCacheForConcept's own publish reach the
	// mesh, instead of the test hand-calling the forward.
	unsubA := busA.Subscribe("#", ebA.onLocalEvent, events.WithSubscriberName("test:bridgeA"))
	if unsubA != nil {
		defer unsubA()
	}

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case msg := <-conn.sendCh:
				if fwd := msg.GetEventForward(); fwd != nil {
					ebB.HandleInbound(fwd)
				}
			}
		}
	}()

	engineA.SeedResultCacheForInvalidationTest(writtenKey, written)
	engineB.SeedResultCacheForInvalidationTest(writtenKey, written)
	engineB.SeedResultCacheForInvalidationTest(untouchedKey, untouched)

	waitFor(t, "A warm", func() bool { return engineA.ResultCacheContainsForInvalidationTest(writtenKey) })
	waitFor(t, "B warm", func() bool { return engineB.ResultCacheContainsForInvalidationTest(writtenKey) })
	waitFor(t, "B unrelated warm", func() bool { return engineB.ResultCacheContainsForInvalidationTest(untouchedKey) })

	// The real seam a write goes through.
	engineA.InvalidateCacheForConcept(written)

	// Local half: already gone, no polling. See the doc comment.
	if engineA.ResultCacheContainsForInvalidationTest(writtenKey) {
		t.Error("the writing replica did NOT evict synchronously -- a client re-reading immediately after its own write can be served the pre-write result")
	}

	// Remote half: evicted across the mesh.
	if !waitForBool(func() bool { return !engineB.ResultCacheContainsForInvalidationTest(writtenKey) }) {
		t.Fatal("cross-node eviction FAILED through the write seam: replica B still serves the stale cached result")
	}

	// No over-eviction: the broadcast is per-concept on the far side too.
	if !engineB.ResultCacheContainsForInvalidationTest(untouchedKey) {
		t.Error("the mesh invalidation evicted an UNRELATED concept on replica B -- the broadcast is over-evicting, which makes every replica's cache useless on every write")
	}

	// ...and B re-caches: the entry was dropped, not poisoned.
	engineB.SeedResultCacheForInvalidationTest(writtenKey, written)
	waitFor(t, "B re-cached after invalidation", func() bool {
		return engineB.ResultCacheContainsForInvalidationTest(writtenKey)
	})
}
