//go:build clustere2e

package clustere2e

// concept_registry_follow_test.go -- the live-cluster gate for the follow-mode
// concept-registry-delta stream (memql#4238).
//
// WHAT THIS PROVES THAT NO UNIT TEST CAN
// --------------------------------------
// The broadcaster, the emit points and the cross-node delta are unit-tested in
// component/memql (two engines bridged by the authoring.promote broadcast) and
// the follow wire is tested in component/grpc against a real handler. What only
// a live mesh establishes: a concept promoted through a connection on ONE bff
// replica reaches a client whose follow subscription landed on ANOTHER replica,
// with NO reconnect -- the same round-robin cross-node path construct_training
// exercises for callability, here for the registry-delta push.
//
// It rides the wire directly (dispatcher Send + Events) rather than an SDK
// method: the Go SDK has no follow surface yet (the client half of this issue is
// the TS SDK + portal), and Connection does not consume Events(), so a raw
// reader on a dedicated connection is safe.
//
// HOW TO RUN (mirrors construct_training_test.go)
// ----------
//	make up SERVERS=2 && make scale N=2 && make dev
//	kubectl port-forward pod/<bff-a> 50051:50051 &
//	kubectl port-forward pod/<bff-b> 50052:50051 &
//	MEMQL_E2E_ENDPOINT=localhost:50051 MEMQL_E2E_ENDPOINT_B=localhost:50052 \
//	  MEMQL_E2E_TOKEN=<cluster-owner JWT> bash scripts/test/cluster-e2e.sh --no-build
//
// The token must be a CLUSTER OWNER (promote is owner-gated); a non-owner run
// skips at the promote with the engine's own message. Without MEMQL_E2E_ENDPOINT_B
// the reader may share a replica with the promoter, so the run still asserts the
// delta arrives but logs that the hop was not guaranteed.

import (
	"context"
	"os"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/sdk/go/authoring"
	memqlclient "github.com/znasllc-io/memql/sdk/go/client"
)

// followRegistry sends a follow-mode ConceptsSubscribe on conn and returns a
// channel of the registry deltas that carry reqId, plus a stop func. It reads
// the connection's shared event fanout; nothing else on a clustere2e connection
// consumes it.
func followRegistry(ctx context.Context, t *testing.T, conn *memqlclient.Connection, reqId string) <-chan *memqlv1.ConceptsRegistryDelta {
	t.Helper()
	d := conn.Dispatcher()
	out := make(chan *memqlv1.ConceptsRegistryDelta, 64)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-d.Events():
				if !ok {
					return
				}
				delta := msg.GetConceptsRegistryDelta()
				if delta == nil || delta.GetRequestId() != reqId {
					continue
				}
				select {
				case out <- delta:
				default:
				}
			}
		}
	}()
	if _, err := d.Send(&memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_ConceptsSubscribe{
			ConceptsSubscribe: &memqlv1.ConceptsSubscribeMsg{RequestId: reqId, Follow: true},
		},
	}); err != nil {
		t.Fatalf("send follow subscribe: %v", err)
	}
	return out
}

// waitForRegistryDeltaAdding blocks until a NON-reset delta adding conceptId
// arrives, or the deadline passes.
func waitForRegistryDeltaAdding(t *testing.T, deltas <-chan *memqlv1.ConceptsRegistryDelta, conceptId string, within time.Duration) *memqlv1.ConceptsRegistryDelta {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case d := <-deltas:
			if d.GetReset_() {
				continue
			}
			for _, a := range d.GetAdded() {
				if a.GetId() == conceptId {
					return d
				}
			}
		case <-deadline:
			t.Fatalf("no registry delta adding %q arrived within %s", conceptId, within)
			return nil
		}
	}
}

// TestConceptRegistryFollowDeltaCrossesTheMesh: a follower on one replica learns
// of a promote on another without reconnecting.
func TestConceptRegistryFollowDeltaCrossesTheMesh(t *testing.T) {
	tok := token(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	// conns[0] promotes; conns[1] follows. As in construct_training_test.go, the
	// reader's address is the whole point: MEMQL_E2E_ENDPOINT_B pins it to a
	// SPECIFIC second replica so the hop is exercised deterministically.
	conns := openConnections(ctx, t, tok, 1)
	if addrB := os.Getenv("MEMQL_E2E_ENDPOINT_B"); addrB != "" {
		connB, err := memqlclient.Connect(ctx, memqlclient.ConnectConfig{Endpoint: addrB, Token: tok})
		if err != nil {
			t.Fatalf("connect the second replica at %s: %v", addrB, err)
		}
		conns = append(conns, connB)
		t.Logf("follower pinned to a second replica at %s -- the cross-node hop is exercised", addrB)
	} else {
		conns = append(conns, openConnections(ctx, t, tok, 1)...)
		t.Logf("MEMQL_E2E_ENDPOINT_B unset: the follower may share a replica with the promoter, " +
			"so the cross-node hop is NOT guaranteed by this run")
	}
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()

	f := newTrainingFixture()
	reqId := id.NewShortId()

	// Follow BEFORE the promote, on the reader replica.
	deltas := followRegistry(ctx, t, conns[1], reqId)

	// Give the subscription a beat to register + deliver the snapshot.
	time.Sleep(500 * time.Millisecond)

	// Promote the concept on the promoter replica.
	promoter := authoring.NewClient(conns[0].Dispatcher())
	res, err := promoter.DurablePromoteBundle(ctx, f.conceptOnly())
	skipUnlessOwner(t, res, err)
	if err != nil {
		t.Fatalf("durable promote: %v", err)
	}
	if !res.OK {
		t.Fatalf("durable promote refused: %s", res.Error)
	}
	defer withdraw(ctx, t, promoter, f)

	// The follower receives the Added delta -- cross-node, no reconnect.
	d := waitForRegistryDeltaAdding(t, deltas, f.conceptId, 45*time.Second)
	if d.GetGeneration() == 0 {
		t.Error("a registry delta must carry a non-zero generation")
	}
	if len(d.GetAdded()) == 0 || d.GetAdded()[0].GetDomain() == "" {
		t.Errorf("delta ConceptInfo not projected: %+v", d.GetAdded())
	}
}
