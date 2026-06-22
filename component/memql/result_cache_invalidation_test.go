package memql

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// bundleForConcepts builds a minimal ExecuteResult whose bundle carries
// one node per concept, so dependencyConceptsForResult / the cache can
// key off real concept ids.
func bundleForConcepts(concepts ...string) *ExecuteResult {
	nodes := make([]*memqlv1.MemoryNode, 0, len(concepts))
	for _, c := range concepts {
		nodes = append(nodes, &memqlv1.MemoryNode{Id: c + ":row", Concept: c})
	}
	return newExecuteResult(&memqlv1.GraphBundle{Nodes: nodes})
}

func TestResultCache_EvictConcept_DropsDependentKeys(t *testing.T) {
	c, err := newResultCache(1024)
	if err != nil {
		t.Fatalf("newResultCache: %v", err)
	}
	t.Cleanup(c.close)

	const concept = "v1:test:widget"
	res := bundleForConcepts(concept)

	c.set("key-A", res, time.Minute, []string{concept})
	c.set("key-B", res, time.Minute, []string{concept})
	c.set("key-other", res, time.Minute, []string{"v1:test:gadget"})

	// Ristretto Set is async; wait for the writes to land.
	waitForCacheKey(t, c, "key-A")
	waitForCacheKey(t, c, "key-B")
	waitForCacheKey(t, c, "key-other")

	if n := c.evictConcept(concept); n != 2 {
		t.Fatalf("evictConcept(%q) evicted %d keys, want 2", concept, n)
	}

	if _, ok := c.get("key-A"); ok {
		t.Error("key-A still cached after eviction of its concept")
	}
	if _, ok := c.get("key-B"); ok {
		t.Error("key-B still cached after eviction of its concept")
	}
	// A key that depends on a DIFFERENT concept must survive.
	if _, ok := c.get("key-other"); !ok {
		t.Error("key-other (different concept) was wrongly evicted")
	}
}

// TestResultCache_InvalidationSubscriber_GraphWriteEvictsResult is the
// acceptance test: a query result is cached, a cache.invalidate for that
// concept fires on the engine's event bus (the dedicated 5.6 channel
// every graph write emits), and the next read is a cache MISS (no stale
// read). It exercises the real engine subscriber wiring
// (StartCacheInvalidationSubscriber) against a real events.Bus, scoped
// to the engine lifecycle context.
func TestResultCache_InvalidationSubscriber_GraphWriteEvictsResult(t *testing.T) {
	cache, err := newResultCache(1024)
	if err != nil {
		t.Fatalf("newResultCache: %v", err)
	}
	t.Cleanup(cache.close)

	bus := events.NewBus(events.WithLogger(slog.Default()))
	t.Cleanup(bus.Close)

	e := &MemQLEngine{cache: cache, eventBus: bus}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	e.StartCacheInvalidationSubscriber(ctx)

	const concept = "v1:test:widget"
	cache.set("plan-key", bundleForConcepts(concept), time.Minute, []string{concept})
	waitForCacheKey(t, cache, "plan-key")

	if _, ok := cache.get("plan-key"); !ok {
		t.Fatal("precondition: result should be cached before the write")
	}

	// A cache.invalidate for that concept must evict the cached result.
	bus.PublishSync(events.Event{
		Topic: events.TopicCacheInvalidateForConcept(concept), // cache.invalidate.v1:test:widget
		Kind:  events.KindCacheInvalidate,
	})

	// The subscriber handler runs in a delivery goroutine; poll for the miss.
	if waitForCacheMiss(t, cache, "plan-key") == false {
		t.Fatal("cached result was NOT evicted by a graph write to its concept (stale read risk)")
	}
}

// TestResultCache_InvalidationSubscriber_UnrelatedWriteKeepsResult
// guards against over-eviction: a write to a concept the cached result
// does not depend on must leave the result cached.
func TestResultCache_InvalidationSubscriber_UnrelatedWriteKeepsResult(t *testing.T) {
	cache, err := newResultCache(1024)
	if err != nil {
		t.Fatalf("newResultCache: %v", err)
	}
	t.Cleanup(cache.close)

	bus := events.NewBus(events.WithLogger(slog.Default()))
	t.Cleanup(bus.Close)

	e := &MemQLEngine{cache: cache, eventBus: bus}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	e.StartCacheInvalidationSubscriber(ctx)

	const concept = "v1:test:widget"
	cache.set("plan-key", bundleForConcepts(concept), time.Minute, []string{concept})
	waitForCacheKey(t, cache, "plan-key")

	bus.PublishSync(events.Event{
		Topic: events.TopicCacheInvalidateForConcept("v1:test:gadget"), // unrelated concept
		Kind:  events.KindCacheInvalidate,
	})

	// Give any (wrong) eviction a chance to run, then assert still cached.
	time.Sleep(50 * time.Millisecond)
	if _, ok := cache.get("plan-key"); !ok {
		t.Fatal("cached result was wrongly evicted by a write to an unrelated concept")
	}
}

func waitForCacheKey(t *testing.T, c *resultCache, key string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := c.get(key); ok {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("cache key %q never became visible", key)
}

func waitForCacheMiss(t *testing.T, c *resultCache, key string) bool {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := c.get(key); !ok {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return false
}
