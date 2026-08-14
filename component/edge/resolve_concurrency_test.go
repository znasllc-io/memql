// component/edge/resolve_concurrency_test.go
//
// Fix-round addition (review finding 1, not in the original brief): the
// cold-cache path had no request coalescing. Resolve released the cache
// lock before calling exec.SiteByHostname and only re-acquired it to write
// the result, so N concurrent first-requests for the SAME uncached hostname
// each drove their own query before the cache could absorb any of them --
// a residual amplification window on top of the miss-caching this package
// already does. Not a data race (every map access was already correctly
// locked; -race was clean), but exactly the burst shape the package's own
// "amplifier pointed at the database, reachable by anyone who can resolve
// the wildcard" framing warns about.
package edge

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// blockingExec counts calls and blocks every call on a shared gate until the
// test releases it. Blocking is what makes the test deterministic rather
// than a timing gamble: without a fix, EVERY goroutine that reaches
// SiteByHostname before the gate opens is guaranteed to still be there when
// it does (none can return early and populate the cache first), so the test
// does not depend on scheduler luck to expose the bug.
type blockingExec struct {
	calls int32
	gate  chan struct{}
	rows  map[string]*Site
}

func (b *blockingExec) SiteByHostname(_ context.Context, hostname string) (*Site, error) {
	atomic.AddInt32(&b.calls, 1)
	<-b.gate
	return b.rows[hostname], nil
}

// Concurrent misses for ONE hostname must collapse to a single query. This
// is the same shape as integrations/cognition's cache-miss singleflight
// groups (e.g. recentUtterSF / spaceInfoSF in prompt_context_cache.go):
// a fast-path cache check, then a slow path that must not be entered more
// than once per key while a resolution for that key is already in flight.
func TestResolveCoalescesConcurrentMissesForTheSameHostname(t *testing.T) {
	const n = 25
	ex := &blockingExec{
		gate: make(chan struct{}),
		rows: map[string]*Site{
			"shop.example.com": {ID: "site:1", Hostname: "shop.example.com", Status: "live"},
		},
	}
	r := NewResolver(ex, time.Minute)

	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if _, err := r.Resolve(context.Background(), "shop.example.com"); err != nil {
				t.Errorf("Resolve: %v", err)
			}
		}()
	}

	// Give every goroutine a chance to reach the executor (or, once fixed,
	// to pile up as singleflight followers instead) before the gate opens
	// and the first response resolves the cache. Mirrors
	// golang.org/x/sync/singleflight's own dup-suppression test, which
	// sleeps inside the blocked call for the same reason: closing the gate
	// too early would let an early responder populate the cache before the
	// rest of the goroutines are even scheduled, masking the bug behind
	// timing instead of proving it.
	time.Sleep(100 * time.Millisecond)
	close(ex.gate)

	wg.Wait()
	if got := atomic.LoadInt32(&ex.calls); got != 1 {
		t.Errorf("SiteByHostname called %d times for %d concurrent resolutions of the same hostname, want 1 -- concurrent misses are not being coalesced", got, n)
	}
}
