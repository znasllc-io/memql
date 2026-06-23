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
// the live chat reply, deterministically and without AI provider keys.
//
// HOW IT REPRODUCED THE BUG (historical) AND HOW IT IS NOW GREEN
// --------------------------------------------------------------
// nginx round-robins each new gRPC connection across the bff replicas,
// so a handful of subscriber connections spread across both. The producer
// inserts one utterance on whichever replica its connection landed on;
// that replica fans the event out to its LOCAL subscribers and forwards
// to peers. But bff replicas never dial each other (bff is the mesh root;
// MEMQL_WORKER_PEERS lists only worker node types), so from the producer
// replica every sibling bff is a peer with Connection==nil. Historically
// the memql#1245 change SKIPPED those Connection==nil peers and DROPPED the
// forward, so only the subscribers co-located with the producer observed
// the utterance. The durable backbone in Phase 1 (memql#1263/#1264) makes
// every replica converge regardless of the mesh fast-path, and Phase 4
// (memql#1271) reverted the #1245 skip so the fast-path now BUFFERS (never
// drops) to those Connection==nil sibling replicas -> this is green.
//
// The gate needs NO replica identification: it simply asserts that EVERY
// subscriber observes the one utterance exactly once. On current main the
// cross-replica subscribers miss it, so the count is short.
//
// RUN
//   MEMQL_E2E_TOKEN=<user JWT> go test -tags clustere2e -count=1 \
//     -timeout=300s ./test/clustere2e/...
// or `make cluster-e2e`, which boots the cluster + seeds a token first.
package clustere2e

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
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
			"or export a JWT for a cluster user")
	}
	return v
}

// connCount is how many gRPC connections the gate opens through the front
// door. nginx round-robins them across the bff replicas; with this many,
// at least one lands on a replica other than the producer's with
// overwhelming probability, so a cross-replica drop is observed
// deterministically. (P(all co-located on a 2-replica cluster) ~= 2^-N.)
const connCount = 10

// --- helpers -----------------------------------------------------------------

// openConnections opens n authenticated stream connections through the
// front door. conns[0] doubles as the producer. Each connect is retried with
// backoff: right after a cluster boot the first stream through nginx can be
// closed mid-handshake while a bff finishes wiring up (seen in CI run
// 27300481452) -- a transient, not the delivery bug under test.
func openConnections(ctx context.Context, t *testing.T, tok string, n int) []*memqlclient.Connection {
	t.Helper()
	conns := make([]*memqlclient.Connection, 0, n)
	for i := 0; i < n; i++ {
		var conn *memqlclient.Connection
		var err error
		for attempt := 1; attempt <= 4; attempt++ {
			conn, err = memqlclient.Connect(ctx, memqlclient.ConnectConfig{Endpoint: endpoint(), Token: tok})
			if err == nil {
				break
			}
			t.Logf("connect %d/%d attempt %d to %s failed (retrying): %v", i+1, n, attempt, endpoint(), err)
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
		}
		if err != nil {
			for _, c := range conns {
				c.Close()
			}
			t.Fatalf("connect %d/%d to %s: %v", i+1, n, endpoint(), err)
		}
		conns = append(conns, conn)
	}
	return conns
}

// userIDFromToken extracts the actor user id from the JWT `sub` claim
// (v1:identity:user:<uuid>). The signature isn't verified here -- the BFF
// already verified it on the stream; we just need the subject string.
func userIDFromToken(t *testing.T, tok string) string {
	t.Helper()
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("MEMQL_E2E_TOKEN is not a JWT (got %d segments); a user JWT is required", len(parts))
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode JWT payload: %v", err)
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatalf("parse JWT claims: %v", err)
	}
	if claims.Sub == "" {
		t.Fatalf("JWT has no sub claim")
	}
	return claims.Sub
}

// newSpaceWithHuman creates a fresh space, joins the token's user to it as a
// human participant, and returns the space id + the SERVER-assigned human
// participant id (content-addressed on (space, user); we read it back rather
// than guess it). The user can then send utterances and see the space's
// graph events.
func newSpaceWithHuman(ctx context.Context, t *testing.T, conn *memqlclient.Connection, userID string) (spaceID, participantID string) {
	t.Helper()
	qc := memqlclient.NewQueryClient(conn.Dispatcher())
	spaceID = "v1:cognition:space:" + id.NewShortId()
	if _, err := qc.ExecuteNamed(ctx, "mutationCreateSpace", buildMutationCreateSpace(spaceID, "clustere2e delivery probe", "active")); err != nil {
		t.Fatalf("create space: %v", err)
	}
	if _, err := qc.MutationJoinSpaceAsHuman(ctx, memqlclient.MutationJoinSpaceAsHumanArgs{
		PartitionId:     spaceID,
		UserId:      userID,
		DisplayName: "clustere2e probe",
		Status:      "active",
	}); err != nil {
		t.Fatalf("join space as human: %v", err)
	}
	// Read back the server-assigned participant id.
	res, err := qc.QuerySpaceParticipants(ctx, memqlclient.QuerySpaceParticipantsArgs{
		PartitionId:         spaceID,
		ParticipantType: "human",
	})
	if err != nil {
		t.Fatalf("query space participants: %v", err)
	}
	for _, row := range res.Rows() {
		if pid := rowID(row); strings.HasPrefix(pid, "participant-") || strings.Contains(pid, ":participant:") || pid != "" {
			participantID = pid
			break
		}
	}
	if participantID == "" {
		t.Fatalf("no human participant found in space %s after join (rows=%d)", spaceID, len(res.Rows()))
	}
	return spaceID, participantID
}

// rowID pulls the row id out of a query row, tolerating a flat `id` or a
// nested `payload.id` shape.
func rowID(row memqlclient.Row) string {
	if v, _ := row["id"].(string); v != "" {
		return v
	}
	if p, ok := row["payload"].(map[string]any); ok {
		if v, _ := p["id"].(string); v != "" {
			return v
		}
	}
	return ""
}

// openSpaceOnConn performs the client-side "open this space" sequence on one
// connection, mirroring what the CoPresent SPA does on space open
// (useCopresent.joinAsHuman): an idempotent join-as-human write. The insert is
// content-addressed on (space, user) -- a repeat join versions the SAME
// participant row -- and executes on the engine of WHICHEVER bff replica owns
// this connection, landing there as a LOCAL graph event: the space-interest
// signal that subscribes that replica to the space's durable stream
// (memql#1316). A subscriber that skips this models a client that never opened
// the space, which is not a supported delivery contract (SubscribeMsg carries
// a topic pattern, not a space id).
//
// The SPA's every-open write is actually mutationCreateSessionForParticipant
// (callable from the Go SDK since memql#1319/#1321 were fixed); the join write
// exercises the same participant-interest trigger.
func openSpaceOnConn(ctx context.Context, t *testing.T, conn *memqlclient.Connection, spaceID, userID string) {
	t.Helper()
	qc := memqlclient.NewQueryClient(conn.Dispatcher())
	if _, err := qc.MutationJoinSpaceAsHuman(ctx, memqlclient.MutationJoinSpaceAsHumanArgs{
		PartitionId:     spaceID,
		UserId:      userID,
		DisplayName: "clustere2e probe",
		Status:      "active",
	}); err != nil {
		t.Fatalf("open space (join as human) on connection: %v", err)
	}
}

// subscribeUtterances opens a graph-events subscription on conn and returns
// a channel of created-utterance ids observed by THAT connection's replica.
func subscribeUtterances(ctx context.Context, t *testing.T, conn *memqlclient.Connection, spaceID string) <-chan string {
	t.Helper()
	sm := memqlclient.NewSubscriptionManager(conn.Dispatcher())
	// graph.node.created.# -- every node-created event; we filter to the
	// utterance concept + our utterance id below. (A tighter server-side
	// filter risks masking a drop behind a filter mismatch, so we subscribe
	// broad and assert narrow.)
	_, events, err := sm.Subscribe(ctx, memqlclient.SubscriptionKindGraphEvents, "node.created.#")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
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

// utteranceIDFor returns the utterance id if the event is the creation of a
// v1:cognition:utterance row, else "". Tolerates flat or node-nested payloads.
func utteranceIDFor(ev memqlclient.Event) string {
	concept, _ := ev.Payload["concept"].(string)
	node, _ := ev.Payload["node"].(map[string]any)
	if concept == "" && node != nil {
		concept, _ = node["concept"].(string)
	}
	if concept != "v1:cognition:utterance" {
		return ""
	}
	if uid, _ := ev.Payload["id"].(string); uid != "" {
		return uid
	}
	if node != nil {
		uid, _ := node["id"].(string)
		return uid
	}
	return ""
}

// --- the gate ----------------------------------------------------------------

// TestClusterCrossReplicaDelivery is the permanent Phase-0 gate. It must be
// RED on current main (subscribers on a non-producing bff replica miss the
// utterance) and GREEN once the durable backbone lands (Phase 1).
func TestClusterCrossReplicaDelivery(t *testing.T) {
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
	t.Logf("opened %d connections (round-robined across bff replicas); space %s", len(conns), spaceID)

	// Every connection OPENS the space (the SPA's join/create-session sequence)
	// and subscribes before producing, then the subscriptions settle so we
	// don't race the insert. The open is what signals the connection's replica
	// to pull the space's durable stream (memql#1316).
	chans := make([]<-chan string, len(conns))
	for i, c := range conns {
		openSpaceOnConn(ctx, t, c, spaceID, userIDFromToken(t, tok))
		chans[i] = subscribeUtterances(ctx, t, c, spaceID)
	}
	time.Sleep(1500 * time.Millisecond)

	// Produce exactly one utterance from conns[0].
	utteranceID := "v1:cognition:utterance:" + id.NewShortId()
	qc := memqlclient.NewQueryClient(producer.Dispatcher())
	if _, err := qc.MutationSendTextUtterance(ctx, memqlclient.MutationSendTextUtteranceArgs{
		UtteranceId:     utteranceID,
		PartitionId:         spaceID,
		ParticipantId:   participantID,
		ParticipantType: "human",
		Text:            "clustere2e cross-replica delivery probe",
	}); err != nil {
		t.Fatalf("send utterance: %v", err)
	}
	t.Logf("produced utterance %s", utteranceID)

	// Collect for a generous window; every connection must observe it once.
	seen := make([]int, len(conns))
	deadline := time.After(8 * time.Second)
	for collecting := true; collecting; {
		select {
		case <-deadline:
			collecting = false
		default:
			drained := false
			for i, ch := range chans {
				select {
				case uid := <-ch:
					if uid == utteranceID {
						seen[i]++
						drained = true
					}
				default:
				}
			}
			if !drained {
				time.Sleep(50 * time.Millisecond)
			}
		}
	}

	// Sanity: the producer's OWN replica must observe its own utterance --
	// otherwise this is a subscription/authz fault, not the delivery bug.
	if seen[0] == 0 {
		t.Fatalf("producer connection did not observe its own utterance %s -- "+
			"subscription/authz setup problem, not the cross-replica delivery bug", utteranceID)
	}

	var missed, duped []int
	for i := range conns {
		switch seen[i] {
		case 1: // good
		case 0:
			missed = append(missed, i)
		default:
			duped = append(duped, i)
		}
	}
	sawCount := len(conns) - len(missed)
	t.Logf("subscribers that observed the utterance exactly once: %d/%d", sawCount, len(conns))

	if len(missed) > 0 || len(duped) > 0 {
		t.Fatalf("cross-replica delivery FAILED: only %d/%d subscribers observed utterance %s "+
			"(missed connections %v, duplicated on %v; per-conn counts %v).\n"+
			"On a 2-replica cluster a subscriber anchored on a different bff replica than the "+
			"producer never sees the event -- the memql#1259 delivery drop. Expected exactly-once "+
			"on ALL %d connections.",
			sawCount, len(conns), utteranceID, missed, duped, seen, len(conns))
	}
	t.Logf("exactly-once delivery confirmed on all %d connections", len(conns))
}
