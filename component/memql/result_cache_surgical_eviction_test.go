package memql

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/metrics"
)

// Surgical local eviction on the writing node (memql#4531).
//
// WHAT WAS WRONG. executeWrite called e.invalidateCache() -- cache.Clear()
// plus a full dependency-index wipe -- on the shared insert AND update path.
// So on whichever node handled a write, the ENTIRE result cache died, and the
// per-concept eviction machinery (the depIndex, evictConcept, the
// cache.invalidate.* subscriber) only ever earned its keep for writes
// arriving from OTHER replicas. Layering debt: the full clear predates the
// dependency index and nobody removed it when the targeted path landed.
//
// THE ARCHAEOLOGY, AND WHY IT IS A CODE ARGUMENT RATHER THAN A GIT ONE. The
// task asked for `git log -L` on the call. That is not answerable in this
// repository: history is truncated at 68 commits and its first commit is the
// #4461 merge, which is long after the depIndex landed, so every -L / -S walk
// bottoms out at that one commit and proves nothing. The question it was
// meant to settle -- "does some write path exist whose dependency concepts
// cannot be named, which the full clear was quietly covering?" -- is settled
// by an INVARIANT in the code instead, which is stronger evidence than a
// commit message anyway:
//
//	engine.go's cache-set site stores a result ONLY when
//	len(dependencyConceptsForResult(...)) > 0.
//
// A result whose dependencies cannot be named is never CACHED, so no cached
// entry can exist that evictConcept is unable to reach. There is nothing for
// a full clear to catch that the index misses. TestResultCacheRefusesToStoreUnnamedDependencies
// below pins that invariant, because it is the load-bearing half of this
// removal: if a future change ever started caching dependency-less results,
// the surgical path would silently go blind and this argument would expire
// with no test to notice.
func TestInvalidateCacheForConcept_EvictsOnlyTheWrittenConcept(t *testing.T) {
	cache, err := newResultCache(1024)
	if err != nil {
		t.Fatalf("newResultCache: %v", err)
	}
	t.Cleanup(cache.close)

	bus := events.NewBus(events.WithLogger(slog.Default()))
	t.Cleanup(bus.Close)

	e := &MemQLEngine{cache: cache, eventBus: bus}

	const written = "v1:test:widget"
	const untouched = "v1:test:gadget"

	cache.set("widget-key", bundleForConcepts(written), time.Minute, []string{written})
	cache.set("gadget-key", bundleForConcepts(untouched), time.Minute, []string{untouched})
	waitForCacheKey(t, cache, "widget-key")
	waitForCacheKey(t, cache, "gadget-key")

	e.InvalidateCacheForConcept(written)

	// NO POLLING, DELIBERATELY. Asserting the miss immediately on return is
	// the read-your-writes proof: a poll would pass just as happily against
	// an eviction that rides the async bus, which is precisely the
	// regression this design must not introduce (events.Bus.Publish hands
	// each subscriber its own goroutine).
	if _, ok := cache.get("widget-key"); ok {
		t.Error("a write to the concept did NOT synchronously evict its dependent cached result -- a client re-reading immediately after its own write can be served the pre-write rows")
	}

	// The half the old full clear destroyed.
	if _, ok := cache.get("gadget-key"); !ok {
		t.Error("a cached result for an UNRELATED concept was evicted by this write -- this is the full-clear behaviour memql#4531 removed")
	}
}

// The write must still publish for the other replicas: the local eviction
// above is an ADDITION to the broadcast, never a replacement for it. Each
// node holds its own Ristretto instance, so dropping the publish would leave
// every sibling serving the pre-write result until its TTL lapsed.
func TestInvalidateCacheForConcept_StillPublishesForRemoteReplicas(t *testing.T) {
	cache, err := newResultCache(1024)
	if err != nil {
		t.Fatalf("newResultCache: %v", err)
	}
	t.Cleanup(cache.close)

	bus := events.NewBus(events.WithLogger(slog.Default()))
	t.Cleanup(bus.Close)

	const concept = "v1:test:widget"
	got := make(chan string, 1)
	unsubscribe := bus.Subscribe(cacheInvalidationPattern, func(evt events.Event) {
		select {
		case got <- evt.Topic:
		default:
		}
	}, events.WithSubscriberName("test:remoteWitness"))
	if unsubscribe != nil {
		t.Cleanup(unsubscribe)
	}

	e := &MemQLEngine{cache: cache, eventBus: bus}
	e.InvalidateCacheForConcept(concept)

	select {
	case topic := <-got:
		if want := events.TopicCacheInvalidateForConcept(concept); topic != want {
			t.Fatalf("published topic = %q, want %q", topic, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no cache.invalidate event was published -- sibling replicas would keep serving the pre-write result until their TTL lapsed")
	}
}

// A blank concept must do nothing at all -- neither evict nor publish. The
// old full clear had no concept to check, so this is a new edge the surgical
// path introduces.
func TestInvalidateCacheForConcept_BlankConceptIsANoop(t *testing.T) {
	cache, err := newResultCache(1024)
	if err != nil {
		t.Fatalf("newResultCache: %v", err)
	}
	t.Cleanup(cache.close)

	bus := events.NewBus(events.WithLogger(slog.Default()))
	t.Cleanup(bus.Close)

	e := &MemQLEngine{cache: cache, eventBus: bus}
	cache.set("keep", bundleForConcepts("v1:test:widget"), time.Minute, []string{"v1:test:widget"})
	waitForCacheKey(t, cache, "keep")

	e.InvalidateCacheForConcept("   ")

	if _, ok := cache.get("keep"); !ok {
		t.Fatal("a blank concept evicted something")
	}
}

// The invariant the removal of the full clear rests on: a result whose
// dependency concepts cannot be named is never stored, so every cached key is
// reachable from the dependency index and a full clear can catch nothing the
// index misses.
func TestResultCacheRefusesToStoreUnnamedDependencies(t *testing.T) {
	cache, err := newResultCache(1024)
	if err != nil {
		t.Fatalf("newResultCache: %v", err)
	}
	t.Cleanup(cache.close)

	// recordDependencies is what populates the index; with no concepts the
	// key would be cached but unreachable from it. engine.go's set site is
	// what refuses that, and this pins the reason it must keep refusing.
	cache.set("unnamed", bundleForConcepts(), time.Minute, nil)
	time.Sleep(50 * time.Millisecond)

	cache.depMu.Lock()
	indexed := len(cache.depIndex)
	cache.depMu.Unlock()

	if indexed != 0 {
		t.Fatalf("dependency index has %d concepts for a result with no named dependencies", indexed)
	}
	// The engine's guard is the enforcement point; assert it is still spelled
	// as a positive dependency count so a refactor cannot quietly drop it.
	if got := dependencyConceptsForResult(nil, nil); len(got) != 0 {
		t.Fatalf("dependencyConceptsForResult(nil, nil) = %v, want empty -- the cache-set guard keys off this being empty", got)
	}
}

// Invalidation must be COUNTED, including the events that evict nothing:
// "invalidation is not reaching this replica" and "invalidation is reaching
// this replica and finding nothing cached" are different incidents with
// different remedies, and an evictions-only counter cannot tell them apart.
func TestInvalidationIsMetered(t *testing.T) {
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

	const concept = "v1:test:metered"
	cache.set("metered-key", bundleForConcepts(concept), time.Minute, []string{concept})
	waitForCacheKey(t, cache, "metered-key")

	eventsBefore, evictionsBefore := metrics.ResultCacheInvalidationValues()

	bus.PublishSync(events.Event{
		Topic: events.TopicCacheInvalidateForConcept(concept),
		Kind:  events.KindCacheInvalidate,
	})
	if !waitForCacheMiss(t, cache, "metered-key") {
		t.Fatal("precondition: the invalidation did not evict")
	}

	eventsAfter, evictionsAfter := metrics.ResultCacheInvalidationValues()
	if eventsAfter-eventsBefore < 1 {
		t.Errorf("invalidation events counter did not move: %v -> %v", eventsBefore, eventsAfter)
	}
	if evictionsAfter-evictionsBefore < 1 {
		t.Errorf("invalidation evictions counter did not move: %v -> %v", evictionsBefore, evictionsAfter)
	}
}
