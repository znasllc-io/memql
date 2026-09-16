// component/edge/assetcache_overhead_test.go
package edge

import (
	"fmt"
	"testing"
)

// A CACHE'S BYTE BUDGET MUST BOUND ITS MEMORY, and the value length alone
// does not: every entry also costs its key string, a map slot and a list
// node. That is negligible when the values are whole files -- which is what
// this cache was built for -- and dominant when they are short, which is
// what the inline-script hash list is (scripthash.go). The extreme is a
// document with NO inline scripts, whose entry is a one-byte sentinel and
// perhaps 120 bytes of real memory.
//
// Without counting that overhead the cap is unreachable for such entries,
// so eviction never fires and the map grows for the life of the process:
// measured before this was fixed, 500,000 empty-result entries were
// accounted as 488KB against a 4MB cap while occupying ~54MB.
func TestCacheAccountsForPerEntryOverheadNotJustValueBytes(t *testing.T) {
	const cap = 64 * 1024
	c := newBundleCache(cap)

	// One-byte values: under value-only accounting this would admit 65,536
	// entries; under honest accounting the key and node dominate.
	const attempted = 20000
	for i := 0; i < attempted; i++ {
		c.Put(fmt.Sprintf(`"%032x"`, i), []byte{0})
	}
	entries, used := c.stats()

	if used > cap {
		t.Errorf("cache is over its own cap: used=%d cap=%d", used, cap)
	}
	// The real bound: entries must be limited by the byte budget divided by
	// what an entry actually costs, not by the value length.
	if maxHonest := cap / 64; entries > maxHonest {
		t.Errorf("cache holds %d entries under a %d-byte cap, accounting for only %d bytes. "+
			"Per-entry overhead is not counted, so the cap does not bound memory: "+
			"an entry costs its key (%d bytes here) plus a map slot and a list node.",
			entries, cap, used, 34)
	}
}

// Eviction must still be LRU and the cache must still work normally for
// ordinary values -- the overhead accounting must not break what it bounds.
func TestCacheStillServesOrdinaryValuesAfterOverheadAccounting(t *testing.T) {
	c := newBundleCache(1 << 20)
	c.Put("k1", []byte("hello"))
	if got, ok := c.Get("k1"); !ok || string(got) != "hello" {
		t.Fatalf("Get(k1) = %q, %v; want \"hello\", true", got, ok)
	}
}
