package memql

import (
	"context"
	"time"

	"github.com/znasllc-io/memql/component/events"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// startCacheInvalidationSubscriber subscribes the result cache to the
// graph write event bus (5.4). Every graph.node.created / updated /
// deleted event carries the written row's concept in its topic; on
// such an event we evict exactly the cached query results whose plan
// reads that concept (index-keyed eviction via resultCache.depIndex,
// never a full cache scan). This is what makes turning @cache on safe:
// a cached read can never outlive a write to a row it depends on.
//
// Cross-node correctness (CRITICAL). Each node runs its own Ristretto
// instance, so the eviction must fire on EVERY replica that holds a
// cache, not just the one that handled the write. The node EventBridge
// already bridges graph writes across the mesh -- a remote write is
// re-published onto this node's local bus by EventBridge.HandleInbound
// -- so subscribing to the LOCAL bus here is sufficient PROVIDED a
// routing rule forwards the write to peers. The default routing rules
// only forward a fixed set of concept namespaces (cluster / cognition
// / planner); a cached concept outside those would be evicted on the
// writing node but go stale on its siblings. Adoption (5.5) must pair
// each newly-@cache'd concept with a node.RegisterRoutingRule forward
// rule (or rely on an existing one); the cross-node test in
// test/clustere2e proves the wired path end to end.
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
			concept, ok := events.ConceptFromGraphNodeTopic(event.Topic)
			if !ok {
				return
			}
			evicted := e.cache.evictConcept(concept)
			if evicted > 0 && e.Logger != nil {
				e.Logger.Debug("resultCache: evicted on graph write",
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

// cacheInvalidationPattern matches every graph node CDC topic
// (graph.node.created.<concept>, .updated.<concept>, .deleted.<concept>)
// across all concepts. The "#" tail matches the concept suffix
// regardless of how many dot-segments a concept id spans.
const cacheInvalidationPattern = "graph.node.*.#"

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
