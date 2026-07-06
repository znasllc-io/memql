//go:build clustere2e

// Cross-node bare-ids contract (#2441, WS-A A2).
//
// Seam 3 (event egress) crosses the node mesh: an utterance created on the bff
// replica a producer connection landed on is fanned out (durable backbone) to
// subscribers anchored on OTHER replicas. The bare-ids invariant says the
// canonical `{concept}:{shortId}` id stays inside the process boundary and
// across the mesh, but every id that reaches a CLIENT subscriber is a bare
// shortId. This test asserts exactly that on the 2-replica parity cluster: a
// subscriber (very likely on a different replica than the producer, since
// connCount connections round-robin across replicas) receives the created-row
// event carrying a BARE id, never a canonical one.
//
// Gated on MEMQL_E2E_TOKEN like the rest of the suite -- runs under
// `make cluster-e2e`, skips otherwise.

package clustere2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/core/id"
	memqlclient "github.com/znasllc-io/memql/sdk/go/client"
)

// bareID strips a canonical `{concept}:` prefix to the trailing bare short id,
// mirroring the SPA's stripConceptPrefix. Idempotent: a value with no colon
// (already bare) is returned unchanged, so comparisons wrapped in bareID stay
// correct whether the wire delivered a bare or (pre-cutover) canonical id.
func bareID(s string) string {
	if i := strings.LastIndex(s, ":"); i >= 0 {
		return s[i+1:]
	}
	return s
}

// isCanonicalID reports whether s is shaped like a canonical node id
// (`{version}:{namespace}:{entity}:{shortId}` -- four+ colon-delimited
// segments). A bare shortId has zero colons.
func isCanonicalID(s string) bool {
	return strings.Count(s, ":") >= 3
}

func TestClusterCrossReplicaBareIds(t *testing.T) {
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

	// Every connection opens the space + subscribes before producing, then the
	// subscriptions settle so the insert isn't raced.
	chans := make([]<-chan string, len(conns))
	for i, c := range conns {
		openSpaceOnConn(ctx, t, c, spaceID, userIDFromToken(t, tok))
		chans[i] = subscribeUtterances(ctx, t, c, spaceID)
	}
	time.Sleep(1500 * time.Millisecond)

	// Produce one utterance with a BARE mint (the bare-ids client contract:
	// clients send bare shortIds; the engine resolves them server-side).
	shortID := id.NewShortId()
	qc := memqlclient.NewQueryClient(producer.Dispatcher())
	if _, err := qc.SendTextUtterance(ctx, memqlclient.SendTextUtteranceArgs{
		UtteranceId:     shortID,
		PartitionId:     spaceID,
		ParticipantId:   participantID,
		ParticipantType: "human",
		Text:            "clustere2e cross-replica bare-id probe",
	}); err != nil {
		t.Fatalf("send utterance: %v", err)
	}
	t.Logf("produced utterance with bare mint %s", shortID)

	// Collect observations. EVERY id a subscriber receives -- including one
	// anchored on a different replica than the producer -- must be bare, and
	// must equal the bare mint.
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
						t.Errorf("cross-node subscriber received a CANONICAL id %q -- seam 3 must deliver bare shortIds to clients", uid)
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
		t.Fatalf("no subscriber observed the bare-id utterance %s -- delivery or bare-ification regressed", shortID)
	}
	t.Logf("cross-replica bare-id delivery confirmed: %d observations, all bare", observations)
}
