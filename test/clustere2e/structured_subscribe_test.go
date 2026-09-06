//go:build clustere2e

// Cross-node STRUCTURED graph subscription (memql#2460).
//
// Structured graph subscriptions (concept + actions; the server composes
// the bus topic) must deliver across the mesh exactly like the retired
// free-text form did. This test opens a CONCEPT-SCOPED structured
// subscription (concept = v1:planner:plan, action = created) on
// every connection, produces one plan on the producer replica, and
// asserts every subscriber -- including ones anchored on a different
// replica than the producer -- observes the created row by its bare id.
//
// It would FAIL if the server mis-composed the concept-scoped topic, if
// structured subscriptions didn't forward cross-node, or if the delivered
// id regressed to canonical. Gated on MEMQL_E2E_TOKEN like the rest of the
// suite.
//
// The concept is v1:planner:plan rather than the v1:cognition:utterance this
// suite used to drive, because cognition is deleted (memql#4988); see the
// package comment in delivery_test.go for why a plan row carries the same
// meaning here.

package clustere2e

import (
	"context"
	"testing"
	"time"

	"github.com/znasllc-io/memql/core/id"
	memqlclient "github.com/znasllc-io/memql/sdk/go/client"
)

// subscribePlansForConcept opens a CONCEPT-SCOPED structured graph
// subscription (v1:planner:plan / created) on conn and returns a channel of
// created-plan ids observed by THAT connection's replica. Unlike
// subscribePlans (all concepts), this exercises the server-composed
// concept-scoped pattern graph.node.created.v1:planner:plan.
func subscribePlansForConcept(ctx context.Context, t *testing.T, conn *memqlclient.Connection) <-chan string {
	t.Helper()
	sm := memqlclient.NewSubscriptionManager(conn.Dispatcher())
	_, events, err := sm.SubscribeGraph(ctx, memqlclient.GraphSubscribeOptions{
		Concept: "v1:planner:plan",
		Actions: []memqlclient.GraphAction{memqlclient.GraphActionCreated},
	})
	if err != nil {
		t.Fatalf("structured subscribe: %v", err)
	}
	out := make(chan string, 64)
	go func() {
		for ev := range events {
			if pid := planIDFor(ev); pid != "" {
				out <- pid
			}
		}
	}()
	return out
}

func TestClusterStructuredGraphSubscription(t *testing.T) {
	tok := token(t) // skips when MEMQL_E2E_TOKEN is unset
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	conns := openConnections(ctx, t, tok, connCount)
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	producer := conns[0]

	userID := userIDFromToken(t, tok)
	scope := newProbeScope()
	t.Logf("opened %d connections (round-robined across bff replicas); scope %s", len(conns), scope)

	// Each connection opens a CONCEPT-SCOPED structured subscription, then
	// the subscriptions settle before the insert.
	chans := make([]<-chan string, len(conns))
	for i, c := range conns {
		chans[i] = subscribePlansForConcept(ctx, t, c)
	}
	time.Sleep(1500 * time.Millisecond)

	shortID := id.NewShortId()
	createProbePlan(ctx, t, producer, scope, shortID, "clustere2e structured-subscribe probe", userID)
	t.Logf("produced plan with bare mint %s", shortID)

	// Every subscriber (concept-scoped, server-composed topic) must observe
	// the created row by its BARE id, including subscribers on a different
	// replica than the producer.
	observations := 0
	deadline := time.After(8 * time.Second)
	for collecting := true; collecting; {
		select {
		case <-deadline:
			collecting = false
		default:
			drained := false
			for _, ch := range chans {
				select {
				case pid := <-ch:
					drained = true
					if isCanonicalID(pid) {
						t.Errorf("structured subscriber received a CANONICAL id %q -- must be bare", pid)
					}
					if pid == shortID {
						observations++
					}
				default:
				}
			}
			if !drained {
				time.Sleep(50 * time.Millisecond)
			}
		}
	}

	if observations == 0 {
		t.Fatalf("no structured subscriber observed the plan %s -- concept-scoped structured subscription or its cross-node delivery regressed", shortID)
	}
	t.Logf("cross-replica structured-subscription delivery confirmed: %d observations", observations)
}
