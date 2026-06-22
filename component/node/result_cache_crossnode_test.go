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
