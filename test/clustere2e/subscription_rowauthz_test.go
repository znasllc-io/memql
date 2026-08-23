//go:build clustere2e

// Row authorization on graph subscriptions, against a LIVE 2-replica
// cluster (memql#4309, memql#4311).
//
// WHAT THIS FILE COVERS, AND WHAT IT DELIBERATELY DOES NOT.
//
// The cross-node DENIAL -- user B writes an owned row on node 1, user A
// subscribed on node 2 receives nothing, B on node 2 receives it -- needs two
// authenticated users, and the suite seeds ONE token (MEMQL_E2E_TOKEN). It
// also needs a concept that both declares a tier AND is forwarded across the
// mesh, and no concept satisfies both today: declaring the live-surface set
// is memql#4313, which is deferred (see its follow-up). So that assertion is
// gated deterministically instead, with no cluster, in
// component/grpc/subscription_rowauthz_mesh_test.go -- which runs on every
// lane rather than only where a cluster is booted, and which is proven
// load-bearing by disabling the gate.
//
// What IS testable here, on one token and today's declarations, is the half
// that a deterministic test cannot reach: that the new gates did not break
// live delivery on a real mesh, and that the subscribe-time kind gate
// answers a real role over a real front door.

package clustere2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/core/id"
	memqlclient "github.com/znasllc-io/memql/sdk/go/client"
)

// The kind gate (memql#4311) over a real connection: TELEMETRY / ALL are
// owner/admin-only, and the answer must match the token's actual role rather
// than being refused or admitted uniformly.
func TestClusterNonGraphSubscriptionKindGate(t *testing.T) {
	tok := token(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	conns := openConnections(ctx, t, tok, 1)
	defer conns[0].Close()

	// The role comes from the SERVER, not from decoding the JWT: the
	// cluster role is a database fact about the user row, and the gate reads
	// it off the resolved AccessContext. Inferring it from a claim would be
	// asserting against a different value than the one under test.
	access, err := memqlclient.NewQueryClient(conns[0].Dispatcher()).GetMyAccess(ctx)
	if err != nil {
		t.Fatalf("GetMyAccess: %v", err)
	}
	ownerOrAdmin := access.ClusterRole == memqlclient.RoleOwner || access.ClusterRole == memqlclient.RoleAdmin
	t.Logf("MEMQL_E2E_TOKEN resolves to cluster role %q", access.ClusterRole)

	sm := memqlclient.NewSubscriptionManager(conns[0].Dispatcher())

	for _, kind := range []memqlclient.SubscriptionKind{
		memqlclient.SubscriptionKindTelemetry,
		memqlclient.SubscriptionKindAll,
	} {
		_, _, err := sm.Subscribe(ctx, kind, "")
		switch {
		case ownerOrAdmin && err != nil:
			t.Errorf("an owner/admin token was refused a %s subscription: %v", kind, err)
		case !ownerOrAdmin && err == nil:
			t.Errorf("a token below admin was GRANTED a %s subscription. These kinds carry "+
				"node-level events with no row owner to authorize against, and ALL includes every "+
				"graph topic besides (memql#4311).", kind)
		case !ownerOrAdmin && err != nil && !strings.Contains(err.Error(), "PermissionDenied"):
			t.Errorf("the %s refusal does not name PermissionDenied, so a client cannot tell it "+
				"apart from a malformed request: %v", kind, err)
		}
	}
}

// GRAPH subscriptions still deliver across the mesh with the fan-out gate in
// place. The gate runs on every graph event, so a mistake in it -- a concept
// it cannot resolve, a payload shape it misreads -- presents as the live feed
// going quiet, which is precisely the failure a single-node test cannot see
// and an operator reads as "the cluster is idle".
//
// This is the memql#2460 delivery assertion re-run under the new gate; it
// would have failed at any point while the gate denied an undeclared concept.
func TestClusterGraphSubscriptionStillDeliversUnderTheRowGate(t *testing.T) {
	tok := token(t)
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
		Text:            "clustere2e row-authz fan-out probe",
	}); err != nil {
		t.Fatalf("send utterance: %v", err)
	}

	observed := 0
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
					if uid == shortID {
						observed++
					}
				default:
				}
			}
			if !drained {
				time.Sleep(50 * time.Millisecond)
			}
		}
	}
	if observed == 0 {
		t.Fatalf("NO subscriber observed %s. v1:cognition:utterance declares no tier, so row "+
			"admission must admit it to everyone exactly as its reads already return it to "+
			"everyone (design D1). Zero observations means the fan-out gate is denying an "+
			"UNDECLARED concept -- the live feed goes quiet and an operator reads it as an idle "+
			"cluster.", shortID)
	}
	t.Logf("%d/%d subscribers observed the row under the fan-out gate", observed, len(chans))
}
