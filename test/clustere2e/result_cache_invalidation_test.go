//go:build clustere2e

package clustere2e

// Cross-node result-cache invalidation, LIVE (epic5 5.4 primitive + 5.5
// adoption / memql#1969).
//
// CONTRACT under test. Each bff replica runs its OWN Ristretto result cache
// (per the Phase-0 cache audit). A graph write to a concept must evict the
// dependent cached query results on EVERY replica that holds the cache, not
// just the replica that handled the write -- otherwise a cached read on a
// sibling replica goes stale the moment a row it depends on is written.
//
// This test drives the end-to-end hit/evict/miss cycle through the SDK over
// nginx, which round-robins each new gRPC connection across the bff replicas --
// so the read that warms a cache and the write that must evict it land on
// (typically) different replicas.
//
// WHAT THE 4988 CONCEPT SWAP COST, STATED PLAINLY. The scoped read here was
// `spaceUtterances`, which carried an EXPLICIT @cache(ttl="30") -- it was the
// 5.5 ADOPTION half of this pair. Cognition is deleted and no surviving
// engine-owned read has both an explicit @cache annotation and rows a plain
// user can cheaply create (every explicitly-cached read left in the tree is a
// cluster-wide catalog or config registry -- agentRole, skill, rbac role,
// site, router budget -- whose rows are operator-visible and which a probe
// must not litter). So `plansForSpace` carries NO @cache annotation and is
// cached by the 5.6 DEFAULT, at the 60s default TTL. Two things survive that
// swap intact: the assertion, because the 5s poll window below is still far
// under 60s, so only eviction -- never a TTL lapse -- can explain a pass; and
// the reason this file is not a duplicate of result_cache_default_on_test.go,
// because v1:planner:* graph writes ARE forwarded wholesale by per-concept
// routing rules (component/node/routing.go, for the planner's own dispatch),
// which is exactly why a scoped read like this one cannot prove the
// cache.invalidate.*-with-zero-per-concept-rules claim its sibling makes on
// v1:notes:note. This file covers the SCOPED, non-actor-folded cache key; that
// one covers the default-on channel.
//
// The deterministic, always-in-CI proof of the same wired path (two engines,
// two buses, the real EventBridge forward + inbound republish, gated by the
// real routing rules) lives in component/node TestResultCacheInvalidation_CrossNode;
// this is the live-cluster confirmation.
//
// RUN
//
//	MEMQL_E2E_TOKEN=<user JWT> go test -tags clustere2e -count=1 \
//	  -timeout=300s ./test/clustere2e/... -run TestResultCacheInvalidation_CrossReplica
//
// or `make cluster-e2e`.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/znasllc-io/memql/core/id"
	memqlclient "github.com/znasllc-io/memql/sdk/go/client"
)

func TestResultCacheInvalidation_CrossReplica(t *testing.T) {
	tok := token(t)
	userID := userIDFromToken(t, tok)

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	// Several connections so a warm-read and a write land on different replicas
	// behind nginx with high probability. We read on connB and write on connA.
	conns := openConnections(ctx, t, tok, 4)
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	connA, connB := conns[0], conns[1]

	qcA := memqlclient.NewQueryClient(connA.Dispatcher())
	qcB := memqlclient.NewQueryClient(connB.Dispatcher())

	// Fresh scope, seeded on connA.
	scope := newProbeScope()

	createPlan := func(goal string) string {
		pid := "v1:planner:plan:" + id.NewShortId()
		if _, err := qcA.CreatePlan(ctx, probePlanArgs(scope, pid, goal, userID)); err != nil {
			t.Fatalf("create plan: %v", err)
		}
		return pid
	}

	// Seed a SMALL set (well under the 50-row page so the read is a single
	// stable page with no cursor -- the cache key is the empty-cursor key).
	const seed = 3
	for i := 0; i < seed; i++ {
		createPlan(fmt.Sprintf("cache-invalidation seed %02d", i))
		time.Sleep(15 * time.Millisecond)
	}

	planCount := func(qc *memqlclient.QueryClient) int {
		res, err := qc.PlansForSpace(ctx, memqlclient.PlansForSpaceArgs{PartitionId: scope})
		if err != nil {
			t.Fatalf("plansForSpace: %v", err)
		}
		return len(res.Rows())
	}

	// Warm the cache on connB: two reads so the second is a HIT on the replica
	// serving connB (and on any other replica connB round-robins to). The
	// result is now cached on the serving replica(s).
	warm1 := planCount(qcB)
	if warm1 < seed {
		t.Fatalf("warm read 1 returned %d plans, want >= %d seeded", warm1, seed)
	}
	_ = planCount(qcB) // second read -> cache HIT (no assertion; just warms).

	// WRITE a new plan on connA (a different replica than connB, typically).
	// This is graph.node.created.v1:planner:plan -- forwarded cross-node by
	// the routing rules, so the invalidation subscriber fires on connB's replica
	// and evicts the cached plansForSpace result.
	newID := createPlan("cache-invalidation POST-WRITE row")

	// The post-write read on connB MUST reflect the new row. If invalidation did
	// not evict the cache cross-node, connB would serve the stale cached page
	// (seed rows only) and miss newID -- a silent cross-node stale read.
	//
	// Allow a brief window for the mesh forward + eviction to propagate, but the
	// freshness invariant itself is hard: the new row must appear, and not after
	// the cache TTL would have lapsed on its own (which would mask a missing
	// invalidation). We poll only up to a few seconds -- far under the 60s
	// default TTL.
	deadline := time.Now().Add(5 * time.Second)
	for {
		res, err := qcB.PlansForSpace(ctx, memqlclient.PlansForSpaceArgs{PartitionId: scope})
		if err != nil {
			t.Fatalf("post-write plansForSpace: %v", err)
		}
		found := false
		for _, row := range res.Rows() {
			// #2441: query results now carry BARE ids; compare bare forms.
			if bareID(rowID(row)) == bareID(newID) {
				found = true
				break
			}
		}
		if found {
			if len(res.Rows()) < seed+1 {
				t.Fatalf("post-write read shows the new row but only %d total, want >= %d", len(res.Rows()), seed+1)
			}
			return // SUCCESS: cache was evicted cross-node, fresh read served.
		}
		if time.Now().After(deadline) {
			t.Fatalf("post-write read on connB never reflected new plan %s within 5s "+
				"(well under the 60s default cache TTL) -- cross-node cache invalidation did not evict the "+
				"stale cached plansForSpace result on the sibling replica", newID)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
