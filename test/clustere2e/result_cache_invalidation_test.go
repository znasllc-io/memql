//go:build clustere2e

package clustere2e

// Cross-node result-cache invalidation (epic5 5.4).
//
// CONTRACT under test. Each bff replica runs its OWN Ristretto result
// cache (per the Phase-0 cache audit). A graph write to a concept must
// evict the dependent cached query results on EVERY replica that holds
// the cache, not just the replica that handled the write -- otherwise a
// cached read on a sibling replica goes stale the moment a row it
// depends on is written. The invalidation primitive (the concept ->
// cached-plan-keys dependency index + the graph-event subscriber that
// evicts index-keyed, plus a node.RegisterRoutingRule forward so the
// write fans out across the mesh) is what makes turning @cache on safe.
//
// WHY THIS LIVE-CLUSTER ASSERTION IS DEFERRED (and where it is proven
// today). Observing a cache HIT then a post-write MISS through the SDK
// requires at least one query annotated @cache(ttl=...). As of 5.4 ZERO
// queries use @cache (verified -- the cache is built but dormant); 5.4
// builds + proves the invalidation primitive, 5.5 adopts @cache on hot
// reads. So the end-to-end live-cluster hit/miss assertion lands with
// 5.5, when a cached query exists to drive it through nginx (which
// round-robins connections across replicas, guaranteeing the read and
// the write land on different replicas with high probability -- the same
// cross-replica setup the delivery gate uses).
//
// Until then, the cross-node guarantee is proven deterministically and
// in CI by TestResultCacheInvalidation_CrossNode in component/node,
// which wires the SAME real path (two engines, two buses, the real
// EventBridge forward + inbound republish, gated by the real routing
// rules) and FAILS if the routing rule or the inbound republish is
// missing -- the single-node false-green trap. This file is the
// live-cluster placeholder so the harness slot exists; flip the skip in
// 5.5 once a query carries @cache.

import "testing"

func TestResultCacheInvalidation_CrossReplica(t *testing.T) {
	t.Skip("5.4 builds the invalidation primitive (proven cross-node in " +
		"component/node TestResultCacheInvalidation_CrossNode); the live-cluster " +
		"hit/miss assertion needs an @cache query, which 5.5 adopts. Enable here in 5.5.")
}
