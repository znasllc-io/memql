package memql

import (
	"context"
	"time"

	"github.com/znasllc-io/memql/component/events"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// StartCacheInvalidationSubscriber subscribes the result cache to the
// dedicated cache-invalidation channel (epic 5, issue 5.6 /
// memql#1970). Every graph write emits cache.invalidate.<concept> on
// this separate topic alongside its graph.node.* CDC event; the topic
// carries the written row's concept, and on such an event we evict
// exactly the cached query results whose plan reads that concept
// (index-keyed eviction via resultCache.depIndex, never a full cache
// scan). This is what makes default-on caching safe: a cached read can
// never outlive a write to a row it depends on.
//
// WHY A DEDICATED CHANNEL (5.6 refactor). 5.4/5.5 subscribed to
// graph.node.* and relied on a node.RegisterRoutingRule PER CACHED
// CONCEPT to forward those graph writes cross-node. Forwarding graph
// writes more broadly risked re-publishing lifecycle events onto peer
// buses and double-firing automations that assumed single-node delivery
// (e.g. reRouteNeedsAgentOnAgentCreate, memql#1396). 5.6 splits the
// concern: cache.invalidate.* is a channel ONLY this evictor consumes
// (no automations, no other subscribers), so a single broadcast routing
// rule can forward it to every node with ZERO side effects. The
// per-concept cache routing rules are retired.
//
// Cross-node correctness (CRITICAL). Each node runs its own Ristretto
// instance, so the eviction must fire on EVERY replica that holds a
// cache, not just the one that handled the write. The node EventBridge
// bridges the cache.invalidate.* event across the mesh -- a remote write
// is re-published onto this node's local bus by EventBridge.HandleInbound
// -- because the single broadcast rule (cache.invalidate.*) forwards it
// to all node types. Subscribing to the LOCAL bus here is therefore
// sufficient on every replica; the cross-node test in
// test/clustere2e + component/node proves the wired path end to end.
//
// The subscription is scoped to the engine lifecycle context: when ctx
// is cancelled (engine stop) the unsubscribe runs and the handler stops
// firing.
//
// Exported so the cross-node cluster harness can wire two engines onto
// two buses and prove eviction propagates across the mesh; the engine
// bootstrap (run) calls it once at startup.
func (e *MemQLEngine) StartCacheInvalidationSubscriber(ctx context.Context) {
	if e == nil || e.cache == nil || e.eventBus == nil {
		return
	}

	unsubscribe := e.eventBus.Subscribe(
		cacheInvalidationPattern,
		func(event events.Event) {
			concept, ok := events.ConceptFromCacheInvalidateTopic(event.Topic)
			if !ok {
				return
			}
			evicted := e.cache.evictConcept(concept)
			if evicted > 0 && e.Logger != nil {
				e.Logger.Debug("resultCache: evicted on cache.invalidate",
					"concept", concept,
					"topic", event.Topic,
					"keysEvicted", evicted,
					"originNode", event.OriginNodeId,
				)
			}
		},
		events.WithSubscriberName("resultCache:invalidation"),
	)

	if unsubscribe == nil {
		return
	}

	go func() {
		<-ctx.Done()
		unsubscribe()
	}()
}

// cacheInvalidationPattern matches every cache-invalidation topic
// (cache.invalidate.<concept>) across all concepts (5.6). The "#" tail
// matches the concept suffix regardless of how many dot-segments a
// concept id spans (concept ids carry colon segments, not dots, so the
// suffix is a single segment in practice).
const cacheInvalidationPattern = "cache.invalidate.#"

// SeedResultCacheForInvalidationTest inserts a synthetic cached result
// under key, recorded as depending on concept, without executing a
// query (no DB required). It exists so the cross-node cluster harness
// can prove that a graph write handled on one replica evicts a cached
// result on ANOTHER replica's independent cache -- the single-node
// caches are intentionally separate, so the test seeds a key on the
// remote node's engine, fires a write on the local node, and asserts
// the remote key is gone. Not used on any production path.
func (e *MemQLEngine) SeedResultCacheForInvalidationTest(key, concept string) {
	if e == nil || e.cache == nil || key == "" || concept == "" {
		return
	}
	res := newExecuteResult(&memqlv1.GraphBundle{
		Nodes: []*memqlv1.MemoryNode{{Id: key, Concept: concept}},
	})
	e.cache.set(key, res, time.Minute, []string{concept})
}

// ResultCacheContainsForInvalidationTest reports whether key is still
// present in the result cache. Companion to
// SeedResultCacheForInvalidationTest for the cross-node harness.
func (e *MemQLEngine) ResultCacheContainsForInvalidationTest(key string) bool {
	if e == nil || e.cache == nil {
		return false
	}
	_, ok := e.cache.get(key)
	return ok
}
