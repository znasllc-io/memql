package memql

import (
	"fmt"
	"os"
	"testing"
	"time"
)

// TestAIResponseCacheIsBounded is the regression for memql#4124: the cache
// was an unbounded map whose only eviction was lazy (an expired entry was
// deleted when a read happened to land on it). Writes that were never read
// again -- the common case, since the key hashes the fully rendered prompt --
// accumulated forever.
//
// Writes 50x the cap with a TTL long enough that NOTHING expires during the
// run, so the expired-sweep stage cannot be what holds the line. Only a real
// cap does.
func TestAIResponseCacheIsBounded(t *testing.T) {
	const cap = 100
	c := newAIResponseCache(cap)

	for i := 0; i < cap*50; i++ {
		c.set(fmt.Sprintf("key-%d", i), fmt.Sprintf("value-%d", i), time.Hour)
	}

	stats := c.Stats()
	if stats.Size > cap {
		t.Fatalf("cache grew past its cap: size=%d cap=%d", stats.Size, cap)
	}
	if stats.EvictedAtCap == 0 {
		t.Fatalf("cap never bound (evictedAtCap=0) -- the test wrote %d entries "+
			"into a cap of %d, so it must have; the bound is not being applied",
			cap*50, cap)
	}
	if stats.Sets != int64(cap*50) {
		t.Errorf("sets=%d, want %d -- every set should still be counted", stats.Sets, cap*50)
	}
	t.Logf("size=%d cap=%d sets=%d evictedAtCap=%d sweptExpired=%d",
		stats.Size, stats.MaxEntries, stats.Sets, stats.EvictedAtCap, stats.SweptExpired)
}

// TestAIResponseCacheSweepsExpiredBeforeEvictingLive pins the two-stage
// order: an entry already past its TTL is dead by the cache's own contract,
// so reclaiming it must be preferred over shedding a live one. If the order
// inverted, a full cache would start throwing away usable entries while
// holding dead ones.
func TestAIResponseCacheSweepsExpiredBeforeEvictingLive(t *testing.T) {
	const cap = 10
	c := newAIResponseCache(cap)

	// Fill to capacity with entries that are already dead.
	for i := 0; i < cap; i++ {
		c.set(fmt.Sprintf("dead-%d", i), i, time.Nanosecond)
	}
	time.Sleep(2 * time.Millisecond)

	c.set("live", "value", time.Hour)

	stats := c.Stats()
	if stats.SweptExpired == 0 {
		t.Errorf("expired entries were not swept (sweptExpired=0)")
	}
	if stats.EvictedAtCap != 0 {
		t.Errorf("live entries were evicted (evictedAtCap=%d) even though the map "+
			"was full of expired ones -- the sweep must come first", stats.EvictedAtCap)
	}
	if _, ok := c.get("live"); !ok {
		t.Errorf("the freshly-set live entry is missing")
	}
}

// TestAIResponseCacheOverwriteDoesNotEvict guards the exemption in
// evictLocked: replacing an existing key does not grow the map, so a repeated
// set against a full cache must not shed anything.
func TestAIResponseCacheOverwriteDoesNotEvict(t *testing.T) {
	const cap = 5
	c := newAIResponseCache(cap)
	for i := 0; i < cap; i++ {
		c.set(fmt.Sprintf("key-%d", i), i, time.Hour)
	}

	for i := 0; i < 100; i++ {
		c.set("key-0", i, time.Hour)
	}

	stats := c.Stats()
	if stats.EvictedAtCap != 0 {
		t.Errorf("overwriting an existing key evicted %d live entries", stats.EvictedAtCap)
	}
	if stats.Size != cap {
		t.Errorf("size=%d, want %d -- overwrite must not change the entry count", stats.Size, cap)
	}
	if v, ok := c.get("key-4"); !ok || v != 4 {
		t.Errorf("an untouched entry was lost to an overwrite: got %v ok=%v", v, ok)
	}
}

// TestAIResponseCacheUnboundedIsOptIn keeps the escape hatch honest: the
// pre-#4124 behaviour is still reachable, but only by explicitly configuring
// no cap.
func TestAIResponseCacheUnboundedIsOptIn(t *testing.T) {
	c := newAIResponseCache(0)
	for i := 0; i < 1000; i++ {
		c.set(fmt.Sprintf("key-%d", i), i, time.Hour)
	}
	if got := c.Stats().Size; got != 1000 {
		t.Errorf("maxEntries<=0 should be unbounded: size=%d, want 1000", got)
	}
}

// TestAICacheConfigDefaultsToBounded pins that a node built from env gets the
// cap by default. The bug was that no cap existed at all; a cap nothing
// selects by default would leave that unchanged.
func TestAICacheConfigDefaultsToBounded(t *testing.T) {
	// t.Setenv registers the restore; Unsetenv then gives the test a
	// genuinely-absent var, which is the state a default must be read from.
	t.Setenv(envAICacheMaxEntries, "")
	if err := os.Unsetenv(envAICacheMaxEntries); err != nil {
		t.Fatalf("unset %s: %v", envAICacheMaxEntries, err)
	}
	cfg := loadAICacheConfigFromEnv()
	if cfg.MaxEntries != defaultAICacheMaxEntries {
		t.Errorf("default MaxEntries=%d, want %d", cfg.MaxEntries, defaultAICacheMaxEntries)
	}
	if cfg.MaxEntries <= 0 {
		t.Errorf("the default must be a real cap, got %d", cfg.MaxEntries)
	}
}

// TestAICacheConfigHonoursOverride covers both directions of the env knob.
func TestAICacheConfigHonoursOverride(t *testing.T) {
	t.Setenv(envAICacheMaxEntries, "42")
	if got := loadAICacheConfigFromEnv().MaxEntries; got != 42 {
		t.Errorf("MaxEntries=%d, want 42", got)
	}

	t.Setenv(envAICacheMaxEntries, "0")
	if got := loadAICacheConfigFromEnv().MaxEntries; got != 0 {
		t.Errorf("explicit unbounded not honoured: MaxEntries=%d, want 0", got)
	}
}
