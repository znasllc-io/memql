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
// 5.5 turned this from a dormant primitive into a live path: spaceUtterances
// now carries @cache(ttl="30") and v1:cognition:* writes (created/updated/deleted)
// are forwarded cross-node by the routing rules in component/node/routing.go.
// This test drives the end-to-end hit/evict/miss cycle through the SDK over
// nginx, which round-robins each new gRPC connection across the bff replicas --
// so the read that warms a cache and the write that must evict it land on
// (typically) different replicas.
//
// The deterministic, always-in-CI proof of the same wired path (two engines,
// two buses, the real EventBridge forward + inbound republish, gated by the
// real routing rules) lives in component/node TestResultCacheInvalidation_CrossNode;
// this is the live-cluster confirmation now that a cached query exists to drive it.
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

	// Fresh space + human, seeded on connA.
	spaceID, participantID := newSpaceWithHuman(ctx, t, connA, userID)

	sendUtterance := func(text string) string {
		uid := "v1:cognition:utterance:" + id.NewShortId()
		if _, err := qcA.SendTextUtterance(ctx, memqlclient.SendTextUtteranceArgs{
			UtteranceId:     uid,
			PartitionId:     spaceID,
			ParticipantId:   participantID,
			ParticipantType: "human",
			Text:            text,
		}); err != nil {
			t.Fatalf("send utterance: %v", err)
		}
		return uid
	}

	// Seed a SMALL set (well under the 50-row page so the read is a single
	// stable page with no cursor -- the cache key is the empty-cursor key).
	const seed = 3
	for i := 0; i < seed; i++ {
		sendUtterance(fmt.Sprintf("cache-invalidation seed %02d", i))
		time.Sleep(15 * time.Millisecond)
	}

	utteranceCount := func(qc *memqlclient.QueryClient) int {
		res, err := qc.SpaceUtterances(ctx, memqlclient.SpaceUtterancesArgs{PartitionId: spaceID})
		if err != nil {
			t.Fatalf("spaceUtterances: %v", err)
		}
		return len(res.Rows())
	}

	// Warm the cache on connB: two reads so the second is a HIT on the replica
	// serving connB (and on any other replica connB round-robins to). The
	// @cache(ttl=30) means the result is now cached on the serving replica(s).
	warm1 := utteranceCount(qcB)
	if warm1 < seed {
		t.Fatalf("warm read 1 returned %d utterances, want >= %d seeded", warm1, seed)
	}
	_ = utteranceCount(qcB) // second read -> cache HIT (no assertion; just warms).

	// WRITE a new utterance on connA (a different replica than connB, typically).
	// This is graph.node.created.v1:cognition:utterance -- forwarded cross-node by
	// the routing rules, so the invalidation subscriber fires on connB's replica
	// and evicts the cached spaceUtterances result.
	newID := sendUtterance("cache-invalidation POST-WRITE row")

	// The post-write read on connB MUST reflect the new row. If invalidation did
	// not evict the cache cross-node, connB would serve the stale cached page
	// (seed rows only) and miss newID -- a silent cross-node stale read.
	//
	// Allow a brief window for the mesh forward + eviction to propagate, but the
	// freshness invariant itself is hard: the new row must appear, and not after
	// the @cache TTL would have lapsed on its own (which would mask a missing
	// invalidation). We poll only up to a few seconds -- far under the 30s TTL.
	deadline := time.Now().Add(5 * time.Second)
	for {
		res, err := qcB.SpaceUtterances(ctx, memqlclient.SpaceUtterancesArgs{PartitionId: spaceID})
		if err != nil {
			t.Fatalf("post-write spaceUtterances: %v", err)
		}
		found := false
		for _, row := range res.Rows() {
			if rowID(row) == newID {
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
			t.Fatalf("post-write read on connB never reflected new utterance %s within 5s "+
				"(well under the 30s @cache TTL) -- cross-node cache invalidation did not evict the "+
				"stale cached spaceUtterances result on the sibling replica", newID)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
