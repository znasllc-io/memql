//go:build clustere2e

// Package clustere2e is the Phase-0 cluster-parity gate for the
// resilient-mesh epic (memql#1259). It boots against the local
// 2-replica k3d + ArgoCD cluster (deploy/k8s/overlays/local, scaled to
// 2 replicas per Deployment via `make scale N=2` -- the topology
// adopted as first-class in memql#1260, migrated off Docker Compose in
// memql#2068/#2088) and asserts the delivery
// invariant that staging kept breaking: an event produced anywhere in
// the mesh reaches the specific bff replica that owns a subscriber's
// stream -- exactly once, no dup, no drop.
//
// It is a SYNTHETIC-EVENT test (memql#1261, owner decision): it injects
// a v1:planner:plan row via createPlan and counts cross-replica delivery,
// rather than driving a real agent turn. That exercises the same
// component/node EventBridge.forwardToPeers path as any live graph write,
// deterministically and without AI provider keys.
//
// THE ROW IS A VEHICLE, AND IT CHANGED (memql#4988). Every probe in this
// suite used to write a v1:cognition:utterance into a space and watch it
// cross replicas. Cognition, spaces and the chat-reply delivery half of
// component/node are deleted, so the suite writes v1:planner:plan rows
// instead. The invariant is unchanged and so is the substrate underneath it:
// component/node/plan_delivery.go rides the same DeliverySubstrate the
// deleted ChatReplyDelivery rode, and graph.node.created.v1:planner:* is a
// broadcast forward rule in component/node/routing.go -- so a plan row
// produced on one replica must reach a subscriber anchored on every other
// replica exactly as an utterance had to.
//
// WHAT WENT AWAY WITH THE ROW, AND WHY NOTHING REPLACED IT. The old probes
// opened each connection's space first (a join-as-human write) because the
// chat-reply substrate subscribed a replica to a space's durable stream
// lazily, on a per-space INTEREST signal (memql#1316). Plan delivery has no
// per-key interest signal to send: PlanDelivery publishes to one fixed
// logical key (plan:lifecycle), and the graph event itself is broadcast to
// every node type by the routing rule. So that setup step is GONE rather than
// ported, and the assertion below is unchanged -- a reader arriving from the
// old file should not go looking for where the open-the-space step went.
//
// HOW IT REPRODUCED THE BUG (historical) AND HOW IT IS NOW GREEN
// --------------------------------------------------------------
// nginx round-robins each new gRPC connection across the bff replicas,
// so a handful of subscriber connections spread across both. The producer
// inserts one row on whichever replica its connection landed on;
// that replica fans the event out to its LOCAL subscribers and forwards
// to peers. But bff replicas never dial each other (bff is the mesh root;
// MEMQL_WORKER_PEERS lists only worker node types), so from the producer
// replica every sibling bff is a peer with Connection==nil. Historically
// the memql#1245 change SKIPPED those Connection==nil peers and DROPPED the
// forward, so only the subscribers co-located with the producer observed
// the write. The durable backbone in Phase 1 (memql#1263/#1264) makes
// every replica converge regardless of the mesh fast-path, and Phase 4
// (memql#1271) reverted the #1245 skip so the fast-path now BUFFERS (never
// drops) to those Connection==nil sibling replicas -> this is green.
//
// The gate needs NO replica identification: it simply asserts that EVERY
// subscriber observes the one row exactly once. On current main the
// cross-replica subscribers miss it, so the count is short.
//
// RUN
//
//	MEMQL_E2E_TOKEN=<user JWT> go test -tags clustere2e -count=1 \
//	  -timeout=300s ./test/clustere2e/...
//
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

	"github.com/znasllc-io/memql/core/id"
	memqlclient "github.com/znasllc-io/memql/sdk/go/client"
)

// --- environment knobs -------------------------------------------------------

func endpoint() string {
	if v := os.Getenv("MEMQL_E2E_ENDPOINT"); v != "" {
		return v
	}
	// bff gRPC, exposed via the k3d port-forward and load-balanced by the
	// k8s Service across bff replicas (memql#1260; k3d migration #2088).
	return "localhost:50051"
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

// newProbeScope mints a fresh v1:planner:plan.partitionId for one test run.
//
// partitionId is a free-form product scope tag -- "the engine derives nothing
// from it" (dsl/planner/concepts.memql) -- so a per-run value is all the
// isolation these probes need: plansForSpace filters on it exactly, which is
// what keeps one run's rows out of another run's page counts on a cluster
// that accumulates them.
func newProbeScope() string {
	return "clustere2e-" + id.NewShortId()
}

// probePlanArgs builds the createPlan arguments every probe in this suite
// writes with.
//
// kind is "adHocAction" DELIBERATELY, and it is the load-bearing field. A
// v1:planner:plan row has three graph.node.created subscribers on a planner
// node (integrations/planner/integration.go): the Planner Agent loop, the
// trainSpecialist dispatcher and the embedDomainItems dispatcher. The latter
// two gate on their own kind and ignore everything else; the first returns
// early for adHocAction (integrations/planner/agent_loop.go), as does the
// stranded-plan watchdog. So a probe row is decomposed by nobody, dispatched
// by nobody, and costs no LLM call -- the same posture automation_run_test.go
// records for naming an automation that does not exist. Any other kind would
// make every one of these probes fire the real planner agent, which is a gate
// nobody dares run.
func probePlanArgs(scope, planID, goal, userID string) memqlclient.CreatePlanArgs {
	return memqlclient.CreatePlanArgs{
		PlanId:      planID,
		PartitionId: scope,
		Kind:        "adHocAction",
		Goal:        goal,
		RequestedBy: userID,
		Input:       map[string]any{"probe": "clustere2e"},
	}
}

// createProbePlan writes one probe plan on conn's replica, failing the test if
// the write is refused.
func createProbePlan(ctx context.Context, t *testing.T, conn *memqlclient.Connection, scope, planID, goal, userID string) {
	t.Helper()
	qc := memqlclient.NewQueryClient(conn.Dispatcher())
	if _, err := qc.CreatePlan(ctx, probePlanArgs(scope, planID, goal, userID)); err != nil {
		t.Fatalf("create probe plan %s in scope %s: %v", planID, scope, err)
	}
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

// rowIDs projects a result page down to its row ids, in page order.
func rowIDs(rows []memqlclient.Row) []string {
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		if pid := rowID(r); pid != "" {
			ids = append(ids, pid)
		}
	}
	return ids
}

// subscribePlans opens a graph-events subscription on conn and returns a
// channel of created-plan ids observed by THAT connection's replica.
func subscribePlans(ctx context.Context, t *testing.T, conn *memqlclient.Connection) <-chan string {
	t.Helper()
	sm := memqlclient.NewSubscriptionManager(conn.Dispatcher())
	// graph.node.created.# -- every node-created event; we filter to the
	// plan concept + our plan id below. (A tighter server-side
	// filter risks masking a drop behind a filter mismatch, so we subscribe
	// broad and assert narrow.) Structured graph subscribe (#2460): empty
	// concept = all concepts, actions = created only.
	_, events, err := sm.SubscribeGraph(ctx, memqlclient.GraphSubscribeOptions{
		Actions: []memqlclient.GraphAction{memqlclient.GraphActionCreated},
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
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

// planIDFor returns the plan id if the event is the creation of a
// v1:planner:plan row, else "". Tolerates flat or node-nested payloads.
func planIDFor(ev memqlclient.Event) string {
	concept, _ := ev.Payload["concept"].(string)
	node, _ := ev.Payload["node"].(map[string]any)
	if concept == "" && node != nil {
		concept, _ = node["concept"].(string)
	}
	if concept != "v1:planner:plan" {
		return ""
	}
	if pid, _ := ev.Payload["id"].(string); pid != "" {
		return pid
	}
	if node != nil {
		pid, _ := node["id"].(string)
		return pid
	}
	return ""
}

// --- the gate ----------------------------------------------------------------

// TestClusterCrossReplicaDelivery is the permanent Phase-0 gate. It must be
// RED on current main (subscribers on a non-producing bff replica miss the
// row) and GREEN once the durable backbone lands (Phase 1).
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

	userID := userIDFromToken(t, tok)
	scope := newProbeScope()
	t.Logf("opened %d connections (round-robined across bff replicas); scope %s", len(conns), scope)

	// Every connection subscribes before producing, then the subscriptions
	// settle so we don't race the insert.
	chans := make([]<-chan string, len(conns))
	for i, c := range conns {
		chans[i] = subscribePlans(ctx, t, c)
	}
	time.Sleep(1500 * time.Millisecond)

	// Produce exactly one plan from conns[0].
	planID := "v1:planner:plan:" + id.NewShortId()
	createProbePlan(ctx, t, producer, scope, planID, "clustere2e cross-replica delivery probe", userID)
	t.Logf("produced plan %s", planID)

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
				case pid := <-ch:
					// #2441: the wire now delivers BARE ids; compare on the
					// bare form (idempotent -- also matches a pre-cutover
					// canonical id).
					if bareID(pid) == bareID(planID) {
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

	// Sanity: the producer's OWN replica must observe its own write --
	// otherwise this is a subscription/authz fault, not the delivery bug.
	if seen[0] == 0 {
		t.Fatalf("producer connection did not observe its own plan %s -- "+
			"subscription/authz setup problem, not the cross-replica delivery bug", planID)
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
	t.Logf("subscribers that observed the plan exactly once: %d/%d", sawCount, len(conns))

	if len(missed) > 0 || len(duped) > 0 {
		t.Fatalf("cross-replica delivery FAILED: only %d/%d subscribers observed plan %s "+
			"(missed connections %v, duplicated on %v; per-conn counts %v).\n"+
			"On a 2-replica cluster a subscriber anchored on a different bff replica than the "+
			"producer never sees the event -- the memql#1259 delivery drop. Expected exactly-once "+
			"on ALL %d connections.",
			sawCount, len(conns), planID, missed, duped, seen, len(conns))
	}
	t.Logf("exactly-once delivery confirmed on all %d connections", len(conns))
}
