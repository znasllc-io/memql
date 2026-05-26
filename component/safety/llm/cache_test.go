package llm

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/safety"
)

func TestVerdictCacheGetPutTTL(t *testing.T) {
	var now atomic.Int64
	now.Store(time.Now().Unix())
	c := newVerdictCache(time.Hour, 0)
	c.now = func() time.Time { return time.Unix(now.Load(), 0) }

	cls := safety.Classification{Tier: safety.TierLow, Reason: "ok", Confidence: 0.9}
	c.Put("k", cls)

	got, ok := c.Get("k")
	if !ok {
		t.Fatal("Get after Put should hit")
	}
	if got.Tier != safety.TierLow || got.Reason != "ok" {
		t.Errorf("verdict round-trip mismatch: %+v", got)
	}
	if got.Source != safety.SourceCache {
		t.Errorf("cache hit should stamp Source=SourceCache, got %v", got.Source)
	}

	// Advance past TTL -- next Get must miss.
	now.Store(time.Now().Add(2 * time.Hour).Unix())
	if _, ok := c.Get("k"); ok {
		t.Error("Get past TTL should miss")
	}
	if c.Len() != 0 {
		t.Errorf("expired entry should be evicted on Get-miss, len=%d", c.Len())
	}
}

func TestVerdictCacheZeroTTLDisabled(t *testing.T) {
	c := newVerdictCache(0, 100)
	c.Put("k", safety.Classification{Tier: safety.TierHigh})
	if _, ok := c.Get("k"); ok {
		t.Error("ttl=0 should disable caching (Get always misses)")
	}
	if c.Len() != 0 {
		t.Errorf("Put with ttl=0 should be a no-op, len=%d", c.Len())
	}
}

func TestVerdictCacheBoundedSweepsExpired(t *testing.T) {
	var now atomic.Int64
	now.Store(time.Now().Unix())
	c := newVerdictCache(time.Hour, 2)
	c.now = func() time.Time { return time.Unix(now.Load(), 0) }

	c.Put("a", safety.Classification{Tier: safety.TierLow})
	c.Put("b", safety.Classification{Tier: safety.TierLow})
	if c.Len() != 2 {
		t.Fatalf("expected 2, got %d", c.Len())
	}
	// Advance past TTL so a + b are expired.
	now.Store(time.Now().Add(2 * time.Hour).Unix())
	// Put should sweep expired and then accept the new entry.
	c.Put("c", safety.Classification{Tier: safety.TierLow})
	if c.Len() != 1 {
		t.Errorf("expected 1 after sweep + add, got %d", c.Len())
	}
	if _, ok := c.Get("c"); !ok {
		t.Error("new entry should be present after sweep")
	}
}

func TestVerdictCacheBoundedDropsWhenFullAndLive(t *testing.T) {
	c := newVerdictCache(time.Hour, 1)
	c.Put("a", safety.Classification{Tier: safety.TierLow})
	c.Put("b", safety.Classification{Tier: safety.TierLow})
	// Cap is 1; "a" is live; sweep finds nothing expired; "b"
	// should be dropped rather than evicting "a".
	if _, ok := c.Get("a"); !ok {
		t.Error("live entry should not be evicted")
	}
	if _, ok := c.Get("b"); ok {
		t.Error("over-cap entry should have been dropped")
	}
}

func TestDescriptorCacheKeyStableForSameAction(t *testing.T) {
	caller := safety.CallerContext{AgentID: "a1"} // identity excluded
	d1 := safety.NewExecAction(safety.SurfaceWorkbench, "ls -la", caller)
	caller2 := safety.CallerContext{AgentID: "a2"} // different identity, same action
	d2 := safety.NewExecAction(safety.SurfaceWorkbench, "ls -la", caller2)
	if descriptorCacheKey(d1) != descriptorCacheKey(d2) {
		t.Error("cache key should be insensitive to CallerContext (same action = same verdict)")
	}
}

func TestDescriptorCacheKeyDifferentForDifferentAction(t *testing.T) {
	caller := safety.CallerContext{}
	d1 := safety.NewExecAction(safety.SurfaceWorkbench, "ls", caller)
	d2 := safety.NewExecAction(safety.SurfaceWorkbench, "rm -rf /", caller)
	if descriptorCacheKey(d1) == descriptorCacheKey(d2) {
		t.Error("different commands must produce different cache keys")
	}
}

func TestDescriptorCacheKeyDifferentForDifferentSurface(t *testing.T) {
	caller := safety.CallerContext{}
	d1 := safety.NewExecAction(safety.SurfaceWorkbench, "ls", caller)
	d2 := safety.NewExecAction(safety.SurfaceComputerUseHeadless, "ls", caller)
	if descriptorCacheKey(d1) == descriptorCacheKey(d2) {
		t.Error("same command on different surfaces must produce different keys")
	}
}

func TestDescriptorCacheKeyFSPathsSorted(t *testing.T) {
	caller := safety.CallerContext{}
	d1 := safety.NewFSAction(safety.SurfaceWorkbench, safety.ActionFSRead, []string{"/a", "/b"}, caller)
	d2 := safety.NewFSAction(safety.SurfaceWorkbench, safety.ActionFSRead, []string{"/b", "/a"}, caller)
	if descriptorCacheKey(d1) != descriptorCacheKey(d2) {
		t.Error("FS action key should be insensitive to path order (same paths = same verdict)")
	}
}

func TestDescriptorCacheKeyHTTPCaseInsensitive(t *testing.T) {
	caller := safety.CallerContext{}
	d1 := safety.NewHTTPAction(safety.SurfaceWorkbench, "GET", "https://X.COM/Foo", "", caller)
	d2 := safety.NewHTTPAction(safety.SurfaceWorkbench, "get", "https://x.com/Foo", "", caller)
	if descriptorCacheKey(d1) != descriptorCacheKey(d2) {
		t.Error("HTTP method + host case differences must not split the cache (path stays case-sensitive)")
	}
}
