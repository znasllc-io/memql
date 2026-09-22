package clustere2e

// The cross-node half of "revocation ends the stream" (epic memql#5327,
// design D1), gated with no cluster.
//
// WHY THIS FILE CARRIES NO BUILD TAG
// ----------------------------------
// The same reason automation_run_routing_test.go beside it does not. The live
// assertion is in worker_revocation_test.go and needs a 2-replica k3d cluster
// and a paired machine, which is skipped on every CI lane and every
// developer's machine -- and a gate skipped by default cannot be what stands
// between a feature and the mesh bug it prevents.
//
// WHAT THIS ACTUALLY PROVES
// -------------------------
// D1 rests on one fact about the mesh: that a write to
// v1:worker:registration is FORWARDED to every node, so the replica holding a
// machine's stream hears about a revoke that landed on a different replica.
// That fact is a routing rule (component/node/routing.go) and nothing in the
// engine asks for it out loud -- so its absence is silent, and it presents as
// "revoking a machine sometimes ends its stream, depending which pod you were
// talking to".
//
// So: two in-process buses joined through node.ForwardDecisionFor -- the exact
// call EventBridge.onLocalEvent makes -- with a real RegistrationWatcher on
// node B holding a real Registry entry. The revoke is published on node A.
//
// HOW TO CONFIRM IT IS LOAD-BEARING
// ---------------------------------
// Delete the graph.node.updated.v1:worker:registration line from
// component/node/routing.go's forward rules and run this file. The event
// never crosses, node B's watcher never sees it, and
// TestARevokeOnOneReplicaEndsTheStreamOnAnother fails naming the stream that
// stayed open. If it passes both ways it is worthless, which is the whole
// risk D1 carries.

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/node"
	workerservice "github.com/znasllc-io/memql/component/worker"
)

const (
	workerNodeA = "agent-a"
	workerNodeB = "agent-b"
	// The registration topic the mesh has to carry. Spelled out rather than
	// composed, because what is under test is the LITERAL the routing rule
	// matches -- a helper that built both sides from one string would agree
	// with itself whatever either one said.
	registrationUpdated = "graph.node.updated.v1:worker:registration"
	registrationCreated = "graph.node.created.v1:worker:registration"
)

// endedStream records why node B's watcher ended a stream, if it did.
type endedStream struct {
	mu      sync.Mutex
	reasons []string
}

func (e *endedStream) record(reason string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.reasons = append(e.reasons, reason)
}

func (e *endedStream) all() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.reasons...)
}

// workerMesh is node A (where the revoke lands) and node B (which holds the
// machine's stream), joined by the real routing decision.
type workerMesh struct {
	link  *meshLink
	busA  *events.Bus
	ended *endedStream
}

func newWorkerMesh(t *testing.T) *workerMesh {
	t.Helper()
	busA := events.NewBus(nil)
	busB := events.NewBus(nil)
	link := newMeshLink(t, workerNodeA, busA, workerNodeB, busB)

	// Node B: a real registry holding one machine, and the real watcher.
	ended := &endedStream{}
	registry := workerservice.NewRegistry(nil, time.Now)
	w := &workerservice.Worker{RegistrationId: "reg-1", OwnerUserId: "user-1"}
	w.SetTerminateFunc(ended.record)
	registry.Add(w)

	sub := workerservice.NewRegistrationEventSubscriber(
		nil, busB, workerservice.NewRegistrationWatcher(registry, workerNodeB, nil))
	sub.Start(t.Context())
	t.Cleanup(func() { sub.Stop(t.Context()) })

	return &workerMesh{link: link, busA: busA, ended: ended}
}

// publishOnA writes a registration event on node A, in the envelope shape
// component/memql's executeUpdate publishes: the payload's fields flattened
// onto the event AND nested under "payload".
func (m *workerMesh) publishOnA(topic string, payload map[string]any) {
	envelope := map[string]any{"id": "reg-1", "concept": "v1:worker:registration"}
	for k, v := range payload {
		envelope[k] = v
	}
	envelope["payload"] = payload
	m.busA.Publish(events.Event{Topic: topic, Payload: envelope})
}

// settle waits for the forward and the subscriber's own handler to run. Both
// are synchronous within a bus publish, so this is a bounded poll rather than
// a sleep -- an assertion that merely slept would pass on a mesh that
// delivered late and fail on a loaded machine.
func (m *workerMesh) settle(t *testing.T, want int) []string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		got := m.ended.all()
		if len(got) >= want || time.Now().After(deadline) {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestTheMeshForwardsRegistrationWrites(t *testing.T) {
	// THE FACT D1 RESTS ON, asked out loud. Nothing in the engine states this
	// requirement, so its absence is silent -- and it presents as "revoking a
	// machine sometimes ends its stream", depending which replica the revoke
	// landed on.
	for _, topic := range []string{registrationCreated, registrationUpdated} {
		forward, _, _ := node.ForwardDecisionFor(topic)
		if !forward {
			t.Errorf("%s must cross the mesh, or a revoke landing on one replica can never reach "+
				"the replica holding the machine's stream", topic)
		}
	}
}

func TestARevokeOnOneReplicaEndsTheStreamOnAnother(t *testing.T) {
	// THE WHOLE HOP. The revoke is written on node A -- which is where an OS
	// click lands, since the shell's connection terminates wherever the front
	// door sent it -- and the stream is on node B.
	m := newWorkerMesh(t)

	m.publishOnA(registrationUpdated, map[string]any{
		"revokedAt":       time.Now().UTC().Format(time.RFC3339Nano),
		"connectedNodeId": workerNodeB,
	})

	got := m.settle(t, 1)
	if len(got) != 1 {
		t.Fatalf("the machine's stream on %s must end when the revoke lands on %s; ended = %v",
			workerNodeB, workerNodeA, got)
	}
	if got[0] != workerservice.DisconnectReasonRevoked {
		t.Fatalf("the reason must name the decision, got %q", got[0])
	}
}

func TestATakeoverOnOneReplicaEndsTheStreamOnTheOther(t *testing.T) {
	// THE SUPERSEDE HALF, across the same hop. A machine that reconnects to
	// node A while node B still holds its old stream: node A's register
	// stamps connectedNodeId at itself, the write crosses, and node B lets go.
	m := newWorkerMesh(t)

	m.publishOnA(registrationUpdated, map[string]any{"connectedNodeId": workerNodeA})

	got := m.settle(t, 1)
	if len(got) != 1 || got[0] != workerservice.DisconnectReasonSuperseded {
		t.Fatalf("a takeover on %s must end the stream on %s as %q; ended = %v",
			workerNodeA, workerNodeB, workerservice.DisconnectReasonSuperseded, got)
	}
}

func TestAnOrdinaryHeartbeatFlushCrossingTheMeshEndsNothing(t *testing.T) {
	// THE CONTROL, and the one that matters most for a broadcast rule. Every
	// heartbeat flush re-asserts connectedNodeId and therefore crosses the
	// mesh -- once per machine per fifteen seconds, to every replica. A
	// watcher that acted on its own node's stamp would drain every stream in
	// the cluster on a fifteen-second cycle.
	m := newWorkerMesh(t)

	m.publishOnA(registrationUpdated, map[string]any{"connectedNodeId": workerNodeB})

	if got := m.settle(t, 1); len(got) != 0 {
		t.Fatalf("a flush naming the holder itself must end nothing; ended = %v", got)
	}
	if m.link.count(registrationUpdated) == 0 {
		t.Fatal("...and it must still have CROSSED, or this test passes because nothing was delivered")
	}
}

func TestTheForwardedPayloadSurvivesTheWireEncoding(t *testing.T) {
	// FIDELITY THAT MATTERS. The mesh encodes a payload with structpb, which
	// rejects any Go value it does not recognise, and meshLink does the same
	// -- so a registration field that could not cross would fail HERE rather
	// than in production only. The watcher reads two fields off the far side;
	// this is what says both of them arrive.
	m := newWorkerMesh(t)
	revokedAt := time.Now().UTC().Format(time.RFC3339Nano)

	m.publishOnA(registrationUpdated, map[string]any{
		"revokedAt":       revokedAt,
		"revokedBy":       "v1:identity:user:admin",
		"revokeReason":    "returned it",
		"connectedNodeId": workerNodeB,
		"activeCount":     float64(0),
		"labels":          map[string]any{"machineId": "mac-123"},
	})

	got := m.settle(t, 1)
	if len(got) != 1 || !strings.EqualFold(got[0], workerservice.DisconnectReasonRevoked) {
		t.Fatalf("a full registration payload must cross and be acted on; ended = %v", got)
	}
}
