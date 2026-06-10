//go:build clustere2e

// Package clustere2e is the Phase-0 cluster-parity gate for the
// resilient-mesh epic (memql#1259). It boots against the local
// 2-replica cluster (docker/docker-compose.cluster.yml, the topology
// adopted as first-class in memql#1260) and asserts the delivery
// invariant that staging kept breaking: an event produced anywhere in
// the mesh reaches the specific bff replica that owns a subscriber's
// stream -- exactly once, no dup, no drop.
//
// It is a SYNTHETIC-EVENT test (memql#1261, owner decision): it injects
// a v1:cognition:utterance row via mutationSendTextUtterance and counts
// cross-replica delivery, rather than driving a real LLM reply. That
// exercises the same component/node EventBridge.forwardToPeers path as
// the live chat reply, deterministically and without SI provider keys.
//
// WHY THIS GOES RED ON CURRENT MAIN
// ---------------------------------
// bff replicas do not dial each other: bff is the mesh root (no
// MEMQL_PARENT_ADDRESS) and its MEMQL_WORKER_PEERS lists only the
// worker node types, never a sibling bff. So a row inserted on bff-1
// has, from bff-1's view, a peer bff-2 with Connection==nil; past the
// connecting-grace window the memql#1245 change classifies it
// dispositionSkip and drops the forward. A subscriber whose stream is
// anchored on bff-2 therefore never sees an utterance inserted on bff-1
// -> delivered-count 1 instead of N. The durable backbone in Phase 1
// (memql#1263/#1264) makes every replica converge -> this goes green.
//
// RUN
//   MEMQL_E2E_TOKEN=<user PAT/JWT> go test -tags clustere2e -count=1 \
//     -timeout=300s ./test/clustere2e/...
// or `make cluster-e2e`, which boots the cluster + seeds a token first.
package clustere2e

import (
	"context"
	"os"
	"testing"
	"time"

	memqlclient "github.com/znasllc-io/memql/sdk/go/client"
	"github.com/znasllc-io/memql/core/id"
)

// --- environment knobs -------------------------------------------------------

func endpoint() string {
	if v := os.Getenv("MEMQL_E2E_ENDPOINT"); v != "" {
		return v
	}
	// nginx front door gRPC, load-balanced across bff replicas (memql#1260).
	return "localhost:50050"
}

func token(t *testing.T) string {
	v := os.Getenv("MEMQL_E2E_TOKEN")
	if v == "" {
		t.Skip("MEMQL_E2E_TOKEN not set -- run via `make cluster-e2e` which seeds a user token, " +
			"or export a PAT/JWT for a cluster user")
	}
	return v
}

// wantReplicas is how many distinct bff replicas the subscriber side must
// cover. Staging + the local parity cluster both run bff at replicas: 2.
const wantReplicas = 2

// --- helpers -----------------------------------------------------------------

// connectReplicas opens connections through the front door until it has
// covered `want` DISTINCT bff replicas (keyed by the ServerHello NodeId),
// or fails. nginx round-robins each new gRPC connection across the bff
// replicas, so a handful of dials reliably covers both. Returns one live
// connection per distinct replica.
func connectReplicas(ctx context.Context, t *testing.T, tok string, want int) map[string]*memqlclient.Connection {
	t.Helper()
	byReplica := make(map[string]*memqlclient.Connection)
	const maxDials = 24
	for dials := 0; dials < maxDials && len(byReplica) < want; dials++ {
		conn, err := memqlclient.Connect(ctx, memqlclient.ConnectConfig{
			Endpoint: endpoint(),
			Token:    tok,
		})
		if err != nil {
			t.Fatalf("connect to %s (dial %d): %v", endpoint(), dials, err)
		}
		if conn.NodeId == "" {
			conn.Close()
			t.Fatalf("server handshake returned an empty NodeId; cannot tell replicas apart")
		}
		if _, seen := byReplica[conn.NodeId]; seen {
			conn.Close() // already covered this replica
			continue
		}
		byReplica[conn.NodeId] = conn
	}
	if len(byReplica) < want {
		ids := make([]string, 0, len(byReplica))
		for nodeID := range byReplica {
			ids = append(ids, nodeID)
		}
		for _, c := range byReplica {
			c.Close()
		}
		t.Fatalf("could not reach %d distinct bff replicas after %d dials (saw %d: %v). "+
			"Is this the 2-replica parity cluster (make dev-cluster-up)? A single-replica "+
			"topology structurally cannot reproduce the cross-replica delivery bug.",
			want, maxDials, len(ids), ids)
	}
	return byReplica
}

// newSpaceWithHuman creates a fresh space and joins the token's user to it
// as a human participant, so the user can both send utterances and see the
// space's graph events. Returns the space id + the human participant id.
func newSpaceWithHuman(ctx context.Context, t *testing.T, conn *memqlclient.Connection) (spaceID, participantID string) {
	t.Helper()
	qc := memqlclient.NewQueryClient(conn.Dispatcher())
	spaceID = "v1:cognition:space:" + id.NewShortId()
	if _, err := qc.MutationCreateSpace(ctx, memqlclient.MutationCreateSpaceArgs{
		SpaceId: spaceID,
		Name:    "clustere2e delivery probe",
		Status:  "active",
	}); err != nil {
		t.Fatalf("create space: %v", err)
	}
	participantID = "v1:cognition:participant:" + id.NewShortId()
	if _, err := qc.MutationJoinSpaceAsHuman(ctx, memqlclient.MutationJoinSpaceAsHumanArgs{
		SpaceId:     spaceID,
		DisplayName: "clustere2e probe",
		Status:      "active",
	}); err != nil {
		t.Fatalf("join space as human: %v", err)
	}
	return spaceID, participantID
}

// subscribeUtterances opens a graph-events subscription on conn and returns
// a channel of created-utterance ids observed by THAT replica.
func subscribeUtterances(ctx context.Context, t *testing.T, conn *memqlclient.Connection, spaceID string) <-chan string {
	t.Helper()
	sm := memqlclient.NewSubscriptionManager(conn.Dispatcher())
	// graph.node.created.# -- every node-created event; we filter to the
	// utterance concept + our space + our utterance id below. (A tighter
	// server-side filter risks masking a drop behind a filter mismatch, so
	// we subscribe broad and assert narrow.)
	_, events, err := sm.Subscribe(ctx, memqlclient.SubscriptionKindGraphEvents, "node.created.#")
	if err != nil {
		t.Fatalf("subscribe on replica %s: %v", conn.NodeId, err)
	}
	out := make(chan string, 64)
	go func() {
		for ev := range events {
			if uid := utteranceIDFor(ev, spaceID); uid != "" {
				out <- uid
			}
		}
	}()
	return out
}

// utteranceIDFor returns the utterance id if the event is the creation of a
// v1:cognition:utterance row in spaceID, else "".
func utteranceIDFor(ev memqlclient.Event, spaceID string) string {
	concept, _ := ev.Payload["concept"].(string)
	if concept != "v1:cognition:utterance" {
		// Some event shapes nest the row under "node"/"payload"; tolerate both.
		if node, ok := ev.Payload["node"].(map[string]any); ok {
			concept, _ = node["concept"].(string)
		}
	}
	if concept != "v1:cognition:utterance" {
		return ""
	}
	id, _ := ev.Payload["id"].(string)
	if id == "" {
		if node, ok := ev.Payload["node"].(map[string]any); ok {
			id, _ = node["id"].(string)
		}
	}
	return id
}

// --- the gate ----------------------------------------------------------------

// TestClusterCrossReplicaDelivery is the permanent Phase-0 gate. It must be
// RED on current main (a subscriber on a non-producing bff replica misses
// the utterance) and GREEN once the durable backbone lands (Phase 1).
func TestClusterCrossReplicaDelivery(t *testing.T) {
	tok := token(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// Subscriber side: one live connection per distinct bff replica.
	replicas := connectReplicas(ctx, t, tok, wantReplicas)
	defer func() {
		for _, c := range replicas {
			c.Close()
		}
	}()
	t.Logf("covered %d bff replicas: %v", len(replicas), keys(replicas))

	// Producer side: its own connection (lands on some replica via nginx).
	producer, err := memqlclient.Connect(ctx, memqlclient.ConnectConfig{Endpoint: endpoint(), Token: tok})
	if err != nil {
		t.Fatalf("producer connect: %v", err)
	}
	defer producer.Close()
	spaceID, participantID := newSpaceWithHuman(ctx, t, producer)
	t.Logf("producer anchored on replica %s; space %s", producer.NodeId, spaceID)

	// Subscribe on every replica BEFORE producing, then let the
	// subscriptions settle so we don't race the insert.
	seen := make(map[string]map[string]int) // replica -> utteranceId -> count
	chans := make(map[string]<-chan string)
	for nodeID, c := range replicas {
		seen[nodeID] = make(map[string]int)
		chans[nodeID] = subscribeUtterances(ctx, t, c, spaceID)
	}
	time.Sleep(1500 * time.Millisecond)

	// Produce exactly one utterance.
	utteranceID := "v1:cognition:utterance:" + id.NewShortId()
	qc := memqlclient.NewQueryClient(producer.Dispatcher())
	if _, err := qc.MutationSendTextUtterance(ctx, memqlclient.MutationSendTextUtteranceArgs{
		UtteranceId:     utteranceID,
		SpaceId:         spaceID,
		ParticipantId:   participantID,
		ParticipantType: "human",
		Text:            "clustere2e cross-replica delivery probe",
	}); err != nil {
		t.Fatalf("send utterance: %v", err)
	}
	t.Logf("produced utterance %s", utteranceID)

	// Collect for a generous window; every replica must observe it exactly once.
	deadline := time.After(8 * time.Second)
	for collecting := true; collecting; {
		select {
		case <-deadline:
			collecting = false
		case uid := <-chans[producer.NodeId]:
			recordIfMatch(seen, producer.NodeId, uid, utteranceID)
		default:
			drained := false
			for nodeID, ch := range chans {
				select {
				case uid := <-ch:
					recordIfMatch(seen, nodeID, uid, utteranceID)
					drained = true
				default:
				}
			}
			if !drained {
				time.Sleep(50 * time.Millisecond)
			}
		}
	}

	// Assert exactly-once delivery on EVERY replica.
	var dropped, duped []string
	for nodeID := range replicas {
		switch seen[nodeID][utteranceID] {
		case 1: // good
		case 0:
			dropped = append(dropped, nodeID)
		default:
			duped = append(duped, nodeID)
		}
	}
	if len(dropped) > 0 || len(duped) > 0 {
		t.Fatalf("cross-replica delivery FAILED for utterance %s:\n"+
			"  dropped on replicas: %v\n"+
			"  duplicated on replicas: %v\n"+
			"  per-replica counts: %v\n"+
			"This is the memql#1259 delivery bug: an event produced on one bff replica "+
			"does not reach a subscriber anchored on another. Expected exactly-once on all %d replicas.",
			utteranceID, dropped, duped, seen, len(replicas))
	}
	t.Logf("exactly-once delivery confirmed on all %d replicas", len(replicas))
}

func recordIfMatch(seen map[string]map[string]int, nodeID, uid, want string) {
	if uid == want {
		seen[nodeID][uid]++
	}
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
