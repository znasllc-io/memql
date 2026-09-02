package memql

import (
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// What `took` MEANS (memql#4860).
//
// ===========================================================================
// THE BUG THIS PINS
// ===========================================================================
// memql#4860 was filed as "activeRoles is very slow on the FIRST read after a
// bff restart, then fast forever after", with two measurements taken from the
// OS boot's own result frame: took = 28,753 ms and, on another cold start,
// took = 187,487 ms. Nothing was slow. The number was not a duration.
//
// `ResultMeta.TookMs` is computed by FinalizeMeta as time.Since(startTime),
// and startTime is stamped by newExecuteResult -- wherever the result OBJECT
// happens to be built. Two consequences, and the loud one is the cache:
//
//   - A CACHED READ REPORTED THE ENTRY'S AGE. resultCache.set and .get both
//     round-trip through cloneExecuteResult, which copied startTime verbatim,
//     so a served hit still carried the clock of the miss that filled it.
//     FinalizeMeta -- called by component/grpc/server.go AFTER the engine has
//     returned -- then subtracted that from now. `activeRoles` is
//     @cache(300), so the number ranged over 0..300,000 ms with no relation
//     to any work. Both filed measurements sit inside that window, which is
//     what the issue read as a cold-start cost.
//
//   - AN UNCACHED READ UNDER-REPORTED. On the query path newExecuteResult is
//     called with the bundle already built, so the clock started AFTER the
//     parse, the plan and the database round trip. The "8 ms warm" figure in
//     the same report is that: real, but not the whole query.
//
// So the field was wrong in both directions at once, on every @cache'd query
// in the tree rather than on this one. The fix is that a result's clock is
// the REQUEST's clock: executeWith re-bases it on the moment the call
// arrived, and a clone starts a fresh one because a clone is a new delivery.
//
// These tests are the regression detector the issue asked for. They need no
// database: the defect lives entirely in how the clock travels.

// A result served from the cache reports how long THIS call took, never how
// long the entry has been sitting there.
//
// Before the fix this failed loudly -- a three-minute-old entry reported
// took=180000 -- which is exactly the shape of the number in the issue.
func TestCachedResultReportsThisCallNotTheEntrysAge(t *testing.T) {
	cache, err := newResultCache(1024)
	if err != nil {
		t.Fatalf("newResultCache: %v", err)
	}
	if cache == nil {
		t.Fatal("newResultCache returned nil")
	}

	res := newExecuteResult(&memqlv1.GraphBundle{
		Nodes: []*memqlv1.MemoryNode{{Id: "v1:rbac:role:owner", Concept: "v1:rbac:role"}},
	})
	// The entry was filled three minutes ago, which is an ordinary age inside
	// a @cache(300) window and not an edge case.
	res.startTime = time.Now().Add(-3 * time.Minute)
	cache.set("activeRoles", res, time.Minute, []string{"v1:rbac:role"})
	time.Sleep(50 * time.Millisecond) // ristretto's set is buffered

	got, ok := cache.get("activeRoles")
	if !ok {
		t.Fatal("cached result did not come back")
	}
	got.FinalizeMeta()
	if got.Meta.TookMs > 1000 {
		t.Fatalf("took = %d ms for a cache hit; a served result must time THIS call, "+
			"not the age of the entry it came from (memql#4860)", got.Meta.TookMs)
	}
}

// The clock is re-based on the request, so `took` covers the whole call --
// including the parse and the database round trip that happen before the
// result object exists. Under-reporting is quieter than the cache lie and
// just as wrong: it is the number an operator uses to decide a query is fine.
func TestStartedAtCoversWorkDoneBeforeTheResultExists(t *testing.T) {
	requestBegan := time.Now().Add(-250 * time.Millisecond)

	// The shape of the query path: the engine parses, runs the SQL and builds
	// the bundle, and only THEN constructs the result.
	res := newExecuteResult(&memqlv1.GraphBundle{})
	res.FinalizeMeta()
	if res.Meta.TookMs > 50 {
		t.Fatalf("precondition: a freshly built result should time near zero, got %d ms", res.Meta.TookMs)
	}

	res.startedAt(requestBegan)
	res.FinalizeMeta()
	if res.Meta.TookMs < 200 {
		t.Fatalf("took = %d ms after re-basing on a request that began 250 ms ago; "+
			"the number must include the work done before the result was built", res.Meta.TookMs)
	}
}

// A clone starts its own clock. This is the half that protects any future
// path which serves a stored result without going through executeWith's
// re-base -- the cache is one such path today, and it is the one that shipped
// the bug.
func TestCloneStartsItsOwnClock(t *testing.T) {
	res := newExecuteResult(&memqlv1.GraphBundle{})
	res.startTime = time.Now().Add(-90 * time.Second)

	clone := cloneExecuteResult(res)
	if clone == nil {
		t.Fatal("cloneExecuteResult returned nil")
	}
	clone.FinalizeMeta()
	if clone.Meta.TookMs > 1000 {
		t.Fatalf("took = %d ms on a clone of a 90 s old result; a clone is a new "+
			"delivery and must not inherit the original's clock", clone.Meta.TookMs)
	}
}

// startedAt on a nil result is a no-op, because the executeWith defer runs on
// every return including the error ones, where there is no result at all.
func TestStartedAtToleratesNoResult(t *testing.T) {
	var res *ExecuteResult
	res.startedAt(time.Now()) // must not panic
}
