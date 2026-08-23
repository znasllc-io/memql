package memql

// THE CROSS-NODE GATE for subscription row authorization (memql#4309).
//
// WHY THIS FILE CARRIES NO BUILD TAG, AND LIVES HERE RATHER THAN IN
// test/clustere2e. That package is `//go:build clustere2e` and needs a live
// 2-replica k3d cluster plus a seeded token; the end-to-end assertion belongs
// there and does exist there, but it is SKIPPED everywhere a cluster is not
// running -- including on a developer's machine and on every CI lane that does
// not boot one. A gate skipped by default cannot be the thing standing between
// this feature and the mesh bug it exists to prevent. (The argument, and the
// precedent, are automation_run_routing_test.go's.)
//
// WHAT COULD ACTUALLY BREAK ACROSS THE MESH. The design says admission needs
// no forwarding change, because a forwarded event is re-published on the
// receiving node's bus and fanned out there with THAT node's subscribers'
// contexts. That is true -- and it rests on a fidelity assumption worth
// testing rather than asserting: the forward encodes the payload through
// structpb.NewStruct and decodes it with AsMap(). If that round trip dropped
// or reshaped the nested `payload` object, admission on the receiving node
// would read no owner off the row and DENY IT TO EVERYONE, including its
// owner. That failure is silent, it only happens in a cluster, and it looks
// exactly like "the subscription simply never fires".
//
// HOW TO CONFIRM IT IS LOAD-BEARING. Delete the graphRowFromEvent fallback
// that reads the flattened envelope, or make the fan-out gate resolve the
// concept from the topic instead of the payload, and the owner-side
// assertions here fail while the single-node tests stay green.

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/node"
)

// meshOwnedConcept is namespaced under `planner` deliberately: forwarding is
// decided by TOPIC, and the planner graph topics are among those the mesh
// actually forwards. A fixture in an invented namespace would not cross the
// mesh at all, so the test would assert nothing about the hop -- it would
// pass because nothing moved.
const meshOwnedConcept = "v1:planner:rowauthzprobe"

func meshFixture(t *testing.T) {
	t.Helper()
	if _, err := memqlengine.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	before := memorynodes.All()
	memorynodes.MergeAll(map[string]*memorynodes.Concept{
		meshOwnedConcept: {
			Name: meshOwnedConcept, NodeType: "probe",
			RowAuthz: &langparser.RowAuthzDecl{Tier: langparser.RowAuthzOwned, Owner: "ownerUserId"},
		},
	})
	t.Cleanup(func() { memorynodes.ReplaceAll(before) })

	// POSITIVE CONTROLS, both of them. An unregistered fixture is an
	// UNDECLARED concept, which admits everyone; and a topic the mesh does
	// not forward never reaches node 2 at all. Either one turns every
	// assertion below into a tautology, in opposite directions.
	c, err := memorynodes.Get(meshOwnedConcept)
	require.NoError(t, err)
	require.NotNil(t, c.RowAuthz, "the fixture carries no tier; this file would measure an undeclared concept")
	forward, _, _ := node.ForwardDecisionFor("graph.node.created." + meshOwnedConcept)
	require.True(t, forward,
		"graph.node.created.%s is not forwarded across the mesh, so this test never exercises the "+
			"hop it is named for", meshOwnedConcept)
}

// forwardAcrossMesh reproduces what EventBridge does to an event on its way
// to a peer: the routing decision, then the structpb encode/decode the wire
// performs. Returns the event as the RECEIVING node's bus would publish it.
func forwardAcrossMesh(t *testing.T, evt events.Event, fromNodeId string) events.Event {
	t.Helper()
	forward, _, _ := node.ForwardDecisionFor(evt.Topic)
	require.True(t, forward, "%s does not forward", evt.Topic)

	encoded, err := structpb.NewStruct(evt.Payload)
	require.NoError(t, err,
		"the event payload is not structpb-encodable, so it could never cross the mesh")

	return events.Event{
		Topic:        evt.Topic,
		Kind:         evt.Kind,
		Timestamp:    evt.Timestamp,
		Payload:      encoded.AsMap(),
		Metadata:     evt.Metadata,
		OriginNodeId: fromNodeId,
	}
}

func meshSession(t *testing.T, userId string) (*streamSession, *recordingClientStream) {
	t.Helper()
	rec := newRecordingClientStream(context.Background())
	svc := &service{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s := newTestStreamSession(svc, rec)
	s.subscriptions.Store("sub-"+userId, &subscriptionInfo{
		kind:     memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_GRAPH_EVENTS,
		patterns: []string{"graph.node.*.#"},
	})
	s.accessMu.Lock()
	s.access = &auth.AccessContext{UserId: userId, Role: auth.RoleWriter}
	s.accessLoaded = true
	s.accessMu.Unlock()
	return s, rec
}

// User B writes an owned row on node 1. User A, subscribed on node 2,
// receives nothing; user B, subscribed on node 2, receives it.
//
// The point is the SECOND half. A gate that ran on the producing node would
// have to decide the row against whoever wrote it, and would then either
// deliver it to every subscriber on every node or to none -- the first is the
// leak, the second is the silent breakage. Admission runs where the
// SUBSCRIBER is, so two subscribers on the same receiving node get different
// answers to the same forwarded event.
func TestSubscriptionRowGateAppliesOnTheRECEIVINGNodeAfterAMeshForward(t *testing.T) {
	meshFixture(t)

	// Node 1: user B's write.
	produced := events.Event{
		Topic:     "graph.node.created." + meshOwnedConcept,
		Kind:      events.KindNodeCreated,
		Timestamp: time.Now(),
		Payload: map[string]any{
			"id": meshOwnedConcept + ":r1", "nodeId": meshOwnedConcept + ":r1",
			"concept": meshOwnedConcept, "createdAt": time.Now().UTC().Format(time.RFC3339),
			"ownerUserId": "user-b", "goal": "user-b's row",
			"payload": map[string]any{"ownerUserId": "user-b", "goal": "user-b's row"},
		},
	}
	delivered := forwardAcrossMesh(t, produced, "node-1")

	// Node 2, subscriber A: nothing.
	sessionA, recA := meshSession(t, "user-a")
	sessionA.handleBusEvent(delivered)
	require.Empty(t, notificationsOf(t, recA),
		"user A received user B's owned row after it crossed the mesh. Admission runs on the "+
			"receiving node with the receiving stream's context; if it did not, a forwarded event "+
			"would reach every subscriber on the far node.")

	// Node 2, subscriber B: the row, payload intact.
	sessionB, recB := meshSession(t, "user-b")
	sessionB.handleBusEvent(delivered)
	got := notificationsOf(t, recB)
	require.Len(t, got, 1,
		"the row's OWN owner did not receive it on the far node. The likely cause is fidelity, not "+
			"authorization: the mesh round-trips the payload through structpb, and a reshaped "+
			"payload leaves the gate unable to read an owner -- which denies the row to everyone, "+
			"silently, in a cluster only.")
	require.False(t, got[0].GetPayloadOmitted())
	require.Equal(t, "user-b",
		got[0].GetPayload().GetFields()["ownerUserId"].GetStringValue(),
		"the forwarded event reached its owner but lost its payload on the way")
}

// The structpb round trip preserves what the gate reads. Asserted on its own
// because it is the fidelity assumption the design leans on, and because a
// regression in it presents as an authorization bug rather than an encoding
// one.
func TestMeshForwardPreservesWhatTheRowGateReads(t *testing.T) {
	meshFixture(t)
	produced := events.Event{
		Topic:     "graph.node.updated." + meshOwnedConcept,
		Kind:      events.KindNodeUpdated,
		Timestamp: time.Now(),
		Payload: map[string]any{
			"id": meshOwnedConcept + ":r2", "concept": meshOwnedConcept,
			"ownerUserId": "user-b",
			"payload":     map[string]any{"ownerUserId": "user-b"},
		},
	}
	concept, id, payload, ok := graphRowFromEvent(forwardAcrossMesh(t, produced, "node-1"))
	require.True(t, ok, "a forwarded graph event stopped looking like a graph row")
	require.Equal(t, meshOwnedConcept, concept)
	require.Equal(t, meshOwnedConcept+":r2", id)
	require.Contains(t, string(payload), "user-b",
		"the owner field did not survive the mesh encode/decode, so admission on the receiving "+
			"node would deny this row to its own owner")
}
