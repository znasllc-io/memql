package memql

import (
	"testing"
	"time"
)

func TestResultCacheInvalidationFencesAnInflightRead(t *testing.T) {
	c, err := newResultCache(1024)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.close)
	const written = "v1:test:written"
	const other = "v1:test:other"
	beforeRead := c.snapshot()
	// No key exists yet: eviction must still remember this write.
	c.evictConcept(written)
	c.setAt("stale", bundleForConcepts(written), time.Minute, []string{written}, beforeRead)
	c.setAt("unrelated", bundleForConcepts(other), time.Minute, []string{other}, beforeRead)
	c.cache.Wait()
	if _, ok := c.get("stale"); ok {
		t.Fatal("a query filled the cache after its dependency changed")
	}
	if _, ok := c.get("unrelated"); !ok {
		t.Fatal("an unrelated write prevented a valid fill")
	}
	c.setAt("fresh", bundleForConcepts(written), time.Minute, []string{written}, c.snapshot())
	waitForCacheKey(t, c, "fresh")
}

func TestResultCacheRejectsAnInvalidatedBufferedEntry(t *testing.T) {
	c, err := newResultCache(1024)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.close)
	const concept = "v1:test:written"
	entry := &resultCacheEntry{result: bundleForConcepts(concept), epoch: c.snapshot(), concepts: []string{concept}}
	c.evictConcept(concept)
	// Model an asynchronous admission completing after the synchronous Del.
	// Cache correctness must not depend on Ristretto's worker scheduling.
	c.cache.SetWithTTL("late-admission", entry, 1, time.Minute)
	c.cache.Wait()
	if _, ok := c.cache.Get("late-admission"); !ok {
		t.Fatal("positive control: entry was not admitted")
	}
	if _, ok := c.get("late-admission"); ok {
		t.Fatal("an invalidated buffered result was returned")
	}
}
