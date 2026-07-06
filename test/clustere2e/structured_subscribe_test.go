//go:build clustere2e

// Cross-node STRUCTURED graph subscription (memql#2460).
//
// Structured graph subscriptions (concept + actions; the server composes
// the bus topic) must deliver across the mesh exactly like the retired
// free-text form did. This test opens a CONCEPT-SCOPED structured
// subscription (concept = v1:cognition:utterance, action = created) on
// every connection, produces one utterance on the producer replica, and
// asserts every subscriber -- including ones anchored on a different
// replica than the producer -- observes the created row by its bare id.
//
// It would FAIL if the server mis-composed the concept-scoped topic, if
// structured subscriptions didn't forward cross-node, or if the delivered
// id regressed to canonical. Gated on MEMQL_E2E_TOKEN like the rest of the
// suite.

package clustere2e

import (
	"context"
	"testing"
	"time"

	"github.com/znasllc-io/memql/core/id"
	memqlclient "github.com/znasllc-io/memql/sdk/go/client"
)

// subscribeUtterancesForConcept opens a CONCEPT-SCOPED structured graph
// subscription (v1:cognition:utterance / created) on conn and returns a
// channel of created-utterance ids observed by THAT connection's replica.
// Unlike subscribeUtterances (all concepts), this exercises the
// server-composed concept-scoped pattern graph.node.created.v1:cognition:utterance.
func subscribeUtterancesForConcept(ctx context.Context, t *testing.T, conn *memqlclient.Connection) <-chan string {
	t.Helper()
	sm := memqlclient.NewSubscriptionManager(conn.Dispatcher())
	_, events, err := sm.SubscribeGraph(ctx, memqlclient.GraphSubscribeOptions{
		Concept: "v1:cognition:utterance",
		Actions: []memqlclient.GraphAction{memqlclient.GraphActionCreated},
	})
	if err != nil {
		t.Fatalf("structured subscribe: %v", err)
	}
	out := make(chan string, 64)
	go func() {
		for ev := range events {
			if uid := utteranceIDFor(ev); uid != "" {
				out <- uid
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

	spaceID, participantID := newSpaceWithHuman(ctx, t, producer, userIDFromToken(t, tok))
	t.Logf("opened %d connections (round-robined across bff replicas); space %s", len(conns), spaceID)

	// Each connection opens the space + opens a CONCEPT-SCOPED structured
	// subscription, then the subscriptions settle before the insert.
	chans := make([]<-chan string, len(conns))
	for i, c := range conns {
		openSpaceOnConn(ctx, t, c, spaceID, userIDFromToken(t, tok))
		chans[i] = subscribeUtterancesForConcept(ctx, t, c)
	}
	time.Sleep(1500 * time.Millisecond)

	shortID := id.NewShortId()
	qc := memqlclient.NewQueryClient(producer.Dispatcher())
	if _, err := qc.SendTextUtterance(ctx, memqlclient.SendTextUtteranceArgs{
		UtteranceId:     shortID,
		PartitionId:     spaceID,
		ParticipantId:   participantID,
		ParticipantType: "human",
		Text:            "clustere2e structured-subscribe probe",
	}); err != nil {
		t.Fatalf("send utterance: %v", err)
	}
	t.Logf("produced utterance with bare mint %s", shortID)

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
				case uid := <-ch:
					drained = true
					if isCanonicalID(uid) {
						t.Errorf("structured subscriber received a CANONICAL id %q -- must be bare", uid)
					}
					if uid == shortID {
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
		t.Fatalf("no structured subscriber observed the utterance %s -- concept-scoped structured subscription or its cross-node delivery regressed", shortID)
	}
	t.Logf("cross-replica structured-subscription delivery confirmed: %d observations", observations)
}
