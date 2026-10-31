//go:build clustere2e

package clustere2e

// games_pack_test.go -- the games pack on the 2-replica k3d cluster (design
// record docs/superpowers/specs/2026-10-18-games-pack-design.md).
//
// The pack's own tests prove its decisions over values and its wiring over a
// real database in one process. What only a cluster can prove is the hop: a
// game runtime writes through whichever bff replica the front door picked,
// and a browser watching the live memory view is attached to whichever one it
// is attached to. These tests write through one connection and observe on
// every connection, so with connCount connections round-robined across two
// replicas a missing routing rule shows as a subscriber that never hears.
//
// THE PACK MUST BE ENABLED and every node restarted since: it ships disabled.
// TestClusterGamesPackSetEnabled is the scripted form of Cluster > Modules >
// games > Enable, and runs only when asked (MEMQL_E2E_GAMES_ENABLE=1 or 0).
// Run it ALONE, then restart every node, then run the other two: every node
// reads the row at its NEXT boot, so until each node has restarted the pack is
// "not loaded" and the other two cannot run.
//
//	MEMQL_E2E_GAMES_ENABLE=1 MEMQL_E2E_ENDPOINT=https://api.memql.localhost MEMQL_E2E_TOKEN=<owner JWT> \
//	  go test -tags clustere2e -run TestClusterGamesPackSetEnabled -count=1 \
//	  github.com/znasllc-io/memql/test/clustere2e/
//
// NOT LOADED IS A SKIP, OR A FAILURE WHEN YOU ASK FOR ONE. `make cluster-e2e`
// runs this whole package against a default cluster, whose pack is disabled, and
// must stay green there, so by default the two behaviour tests SKIP when the pack
// is not loaded, naming the fix. A run that is meant to exercise the pack sets
// MEMQL_E2E_GAMES_REQUIRE=1 (read as MEMQL_REQUIRE_DB is) and they FAIL instead,
// so a pack nobody enabled cannot read as a pass.
//
// RUN through the front door, which spreads connections across the bff
// replicas (a port-forward pins one pod and proves nothing about the hop). The
// token is a JWT: a personal access token cannot authenticate to the bff
// stream.
//
//	MEMQL_E2E_GAMES_REQUIRE=1 MEMQL_E2E_ENDPOINT=https://api.memql.localhost MEMQL_E2E_TOKEN=<owner JWT> \
//	  go test -tags clustere2e -run TestClusterGames -count=1 -timeout=300s \
//	  github.com/znasllc-io/memql/test/clustere2e/
//
// Each test logs how many connections landed on each bff node, and the nodes its
// write and its read went through; on a failure it names the nodes of the
// connections that stayed silent. A run whose connections all landed on one node
// did not cross the hop: with MEMQL_E2E_GAMES_REQUIRE set it fails for that,
// before it writes anything, and otherwise it only warns.
//
// A run leaves rows behind: one episode and two plan traces, which the pack's
// retention expires after 30 days, and one skill, which stays in shadow. All of
// it is the token owner's, and each row carries clustere2e in its character,
// world, trigger or goal text.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/id"
	memqlclient "github.com/znasllc-io/memql/sdk/go/client"
	"github.com/znasllc-io/memql/sdk/go/modules"
)

// gamesRequireEnv turns "the pack is not loaded" from a skip into a failure.
const gamesRequireEnv = "MEMQL_E2E_GAMES_REQUIRE"

// gamesRequired reports whether MEMQL_E2E_GAMES_REQUIRE demands a loaded pack.
// It is read as dbtest.RequireDB reads MEMQL_REQUIRE_DB: unset, 0, false and no
// mean it does not; anything else, 1 above all, means it does.
func gamesRequired() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(gamesRequireEnv))) {
	case "", "0", "false", "no":
		return false
	}
	return true
}

// gamesCall runs one named construct ("builtin", "mutation" or "query") on
// one connection. A pack that is not loaded ends the test: skipped, or failed
// when MEMQL_E2E_GAMES_REQUIRE says the run must exercise the pack.
func gamesCall(ctx context.Context, t *testing.T, conn *memqlclient.Connection, kind, name string, args map[string]any) []memqlclient.Row {
	t.Helper()
	call, err := langparser.RenderCall(name, args)
	if err != nil {
		t.Fatalf("render %s: %v", name, err)
	}
	res, err := memqlclient.NewQueryClient(conn.Dispatcher()).ExecuteNamed(ctx, name, kind+" "+call)
	if err != nil {
		// A disabled pack's constructs are not loaded at all ("function ... not
		// found"); a DSL loaded without its Go half has no executor, and it is
		// the pack's own (integration.games.*) that is named: another builtin's
		// missing executor is a different fault and stays a failure. The server
		// carries either text verbatim after "Details:" in its answer.
		if strings.Contains(err.Error(), `function "`+name+`" not found`) || strings.Contains(err.Error(), `unknown builtin executor "integration.games.`) {
			const fix = "enable it with TestClusterGamesPackSetEnabled (MEMQL_E2E_GAMES_ENABLE=1) and restart every node"
			if gamesRequired() {
				t.Fatalf("the games pack is not loaded and %s is set (%v) -- %s", gamesRequireEnv, err, fix)
			}
			t.Skipf("the games pack is not loaded on this cluster (%v) -- %s, or set %s=1 to make this a failure", err, fix, gamesRequireEnv)
		}
		t.Fatalf("%s %s: %v", kind, name, err)
	}
	return res.Rows()
}

// gamesEventID is the bare id a graph event carries, at the top of its payload
// or inside its node (planIDFor's two places). The wire delivers bare ids
// (TestClusterCrossReplicaBareIds holds it to that); comparing on the bare form
// is this suite's idiom, so a regression there fails its own test by name
// instead of reading here as a subscriber that never heard.
func gamesEventID(ev memqlclient.Event) string {
	if got, _ := ev.Payload["id"].(string); got != "" {
		return bareID(got)
	}
	if node, _ := ev.Payload["node"].(map[string]any); node != nil {
		got, _ := node["id"].(string)
		return bareID(got)
	}
	return ""
}

// gamesNode names the bff node a connection landed on: the node id its
// ServerHello carried, which is the pod's MEMQL_NODE_ID.
func gamesNode(c *memqlclient.Connection) string {
	if n := c.ServerInfo().NodeID; n != "" {
		return n
	}
	return "(unnamed)"
}

// gamesLogSpread says how many connections landed on each bff node. A run that
// MEMQL_E2E_GAMES_REQUIRE says must exercise the pack is a run meant to prove the
// hop, and a run whose connections all landed on one node proves nothing about
// it, so that run fails here, before anything is written. Any other run only
// warns: against a fair round robin all connCount landing on one node has a
// chance of 2^-9, and module_registry_test.go treats its spread the same way.
func gamesLogSpread(t *testing.T, conns []*memqlclient.Connection) {
	t.Helper()
	perNode := map[string]int{}
	for _, c := range conns {
		perNode[gamesNode(c)]++
	}
	t.Logf("%d connections landed on %d bff node(s): %v", len(conns), len(perNode), perNode)
	if len(perNode) >= 2 {
		return
	}
	const fix = "scale the bff to 2 replicas and connect through the front door, not a port-forward"
	if gamesRequired() {
		t.Fatalf("every connection landed on one node, so this run cannot prove the hop and %s is set: %s", gamesRequireEnv, fix)
	}
	t.Logf("WARNING: every connection landed on one node, so this run did not cross the hop: %s", fix)
}

// gamesOtherNode picks a connection on a different bff node than conns[from]
// when the spread has one, and the next connection when it does not, so a test
// that means "through another replica" does so whenever the cluster allows it.
func gamesOtherNode(conns []*memqlclient.Connection, from int) int {
	home := gamesNode(conns[from])
	for i, c := range conns {
		if i != from && gamesNode(c) != home {
			return i
		}
	}
	return (from + 1) % len(conns)
}

// gamesHeard is what one connection has heard: the ids of the rows created, and
// the server's reason if it refused the subscription.
type gamesHeard struct {
	mu      sync.Mutex
	ids     map[string]struct{}
	refused string
}

func (h *gamesHeard) add(got string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ids[got] = struct{}{}
}

func (h *gamesHeard) has(want string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.ids[want]
	return ok
}

func (h *gamesHeard) refuse(reason string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if reason == "" {
		reason = "(the server gave no reason)"
	}
	h.refused = reason
}

func (h *gamesHeard) refusal() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.refused
}

// gamesWatch subscribes every connection to the rows of one concept being
// created, and returns what each connection hears, in connection order.
//
// Each connection records every id it hears and nothing else, so the stream is
// always drained: an unrelated writer cannot back it up, and a row heard twice
// on one connection cannot stand in for a row another never heard.
//
// The server answers a subscribe on the subscription's own channel, with
// subscription-created when it took it and subscription-error, carrying its
// reason, when it did not (component/grpc/server.go, handleSubscribe). The error
// is recorded here and reported by gamesCheckSubscribed, so a refused
// subscription fails as itself and not as a forward rule that is missing.
func gamesWatch(ctx context.Context, t *testing.T, conns []*memqlclient.Connection, concept string) []*gamesHeard {
	t.Helper()
	heard := make([]*gamesHeard, len(conns))
	for i, c := range conns {
		sm := memqlclient.NewSubscriptionManager(c.Dispatcher())
		_, events, err := sm.SubscribeGraph(ctx, memqlclient.GraphSubscribeOptions{
			Concept: concept,
			Actions: []memqlclient.GraphAction{memqlclient.GraphActionCreated},
		})
		if err != nil {
			t.Fatalf("subscribe %d to %s: %v", i, concept, err)
		}
		h := &gamesHeard{ids: map[string]struct{}{}}
		go func() {
			for ev := range events {
				if kind, _ := ev.Payload["eventType"].(string); kind == "subscription-error" {
					reason, _ := ev.Payload["message"].(string)
					h.refuse(reason)
					continue
				}
				if got := gamesEventID(ev); got != "" {
					h.add(got)
				}
			}
		}()
		heard[i] = h
	}
	return heard
}

// gamesCheckSubscribed fails the test, in the server's own words, if the server
// refused any connection's subscription. A refused subscription hears nothing
// from anyone, which would otherwise read as a forward rule that is missing.
func gamesCheckSubscribed(t *testing.T, conns []*memqlclient.Connection, heard []*gamesHeard, concept string) {
	t.Helper()
	for i, h := range heard {
		if reason := h.refusal(); reason != "" {
			t.Fatalf("the server refused connection %d's (%s) subscription to %s: %s", i, gamesNode(conns[i]), concept, reason)
		}
	}
}

// gamesAwaitAll fails the test unless every connection has heard the row want
// created within window. It names the connections that did not, and the nodes
// they are on, because a forward rule that is missing shows as one node going
// quiet. A refused subscription is reported as that, first.
func gamesAwaitAll(t *testing.T, conns []*memqlclient.Connection, heard []*gamesHeard, concept, want string, window time.Duration) {
	t.Helper()
	deadline := time.Now().Add(window)
	for {
		gamesCheckSubscribed(t, conns, heard, concept)
		var missed []string
		for i, h := range heard {
			if !h.has(want) {
				missed = append(missed, fmt.Sprintf("%d (%s)", i, gamesNode(conns[i])))
			}
		}
		if len(missed) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d connections never heard %s %s created within %s: connection(s) %s. The graph.node.created.%s forward rule is missing, or the event did not cross replicas",
				len(missed), len(heard), concept, want, window, strings.Join(missed, ", "), concept)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestClusterGamesPackSetEnabled flips the pack, and only when asked: it is an
// operator's act, and the state is left as requested. Every node reads the
// row at its NEXT boot, so a restart must follow.
func TestClusterGamesPackSetEnabled(t *testing.T) {
	want := os.Getenv("MEMQL_E2E_GAMES_ENABLE")
	if want != "1" && want != "0" {
		t.Skip("set MEMQL_E2E_GAMES_ENABLE=1 to enable the games pack, or 0 to disable it")
	}
	tok := token(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	conn := openConnections(ctx, t, tok, 1)[0]
	defer conn.Close()
	flip, err := modules.NewClient(conn.Dispatcher()).SetPackEnabled(ctx, "games", want == "1", "games pack local end-to-end test")
	if err != nil {
		t.Fatalf("set games pack enabled=%s: %v", want, err)
	}
	t.Logf("games pack: enabled %v -> %v; restart required: %v (kubectl rollout restart every node)", flip.PriorEnabled, flip.Enabled, flip.RestartRequired)
}

// An episode written through one replica reaches a subscriber on every
// replica, and recall through another replica finds it.
func TestClusterGamesMemoryCrossesReplicas(t *testing.T) {
	tok := token(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	conns := openConnections(ctx, t, tok, connCount)
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	gamesLogSpread(t, conns)

	// The episode, and the character and world it belongs to, are this run's
	// alone. Recall returns the top 8 for a character, and half of the score is
	// relevance: a character shared with earlier runs would hold their embedded
	// episodes, which outrank this run's when its own embedding failed, and
	// push it out.
	run := id.NewShortId()
	episodeID, character, world := "e-clustere2e-"+run, "clustere2e-"+run, "clustere2e-"+run
	heard := gamesWatch(ctx, t, conns, "v1:games:episode")
	time.Sleep(2 * time.Second) // let every subscription settle before the write
	gamesCheckSubscribed(t, conns, heard, "v1:games:episode")

	recorded := gamesCall(ctx, t, conns[0], "builtin", "gamesRecordEpisodes", map[string]any{"episodes": []any{map[string]any{
		"episodeId": episodeID, "characterId": character, "worldId": world,
		"at": time.Now().UTC().Format(time.RFC3339), "kind": "event",
		"summary": "a cluster test watched the sunrise", "importance": 3,
	}}})
	t.Logf("episode %s written through connection 0 (%s): %v", episodeID, gamesNode(conns[0]), recorded)

	gamesAwaitAll(t, conns, heard, "v1:games:episode", episodeID, 20*time.Second)

	reader := gamesOtherNode(conns, 0)
	rows := gamesCall(ctx, t, conns[reader], "builtin", "gamesRecall", map[string]any{
		"characterId": character, "worldId": world, "text": "sunrise",
	})
	found := false
	for _, r := range rows {
		if r["kind"] == "episode" && r["id"] == episodeID {
			found = true
			t.Logf("recall through connection %d (%s) ranked the episode %v of %d rows (relevanceAvailable %v)",
				reader, gamesNode(conns[reader]), r["rank"], len(rows), r["relevanceAvailable"])
		}
	}
	if !found {
		t.Fatalf("recall through another connection did not return episode %s: %v", episodeID, rows)
	}
}

// Two traces through two connections teach one skill, and its creation
// reaches every replica.
func TestClusterGamesSkillLearnsAcrossReplicas(t *testing.T) {
	tok := token(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	conns := openConnections(ctx, t, tok, connCount)
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	gamesLogSpread(t, conns)

	// The goal is unique to this run, so the skill this test teaches is the
	// only one of its signature however often the test is run.
	goal := "clustere2e goal " + id.NewShortId()
	trace := func(traceID string, count int) map[string]any {
		return map[string]any{
			"traceId": traceID, "characterId": "clustere2e", "goalText": goal, "trigger": "clustere2e",
			"steps": []any{
				map[string]any{"action": "gather", "args": map[string]any{"item": "sample", "count": count}, "ok": true, "durationMs": 10},
			},
			"outcome": "success", "at": time.Now().UTC().Format(time.RFC3339),
		}
	}
	heard := gamesWatch(ctx, t, conns, "v1:games:skill")
	time.Sleep(2 * time.Second) // let every subscription settle before the first trace
	gamesCheckSubscribed(t, conns, heard, "v1:games:skill")

	second := gamesOtherNode(conns, 0)
	t.Logf("trace A goes through connection 0 (%s), trace B through connection %d (%s)", gamesNode(conns[0]), second, gamesNode(conns[second]))
	gamesCall(ctx, t, conns[0], "builtin", "gamesRecordTrace", trace("t-a-"+id.NewShortId(), 3))
	rows := gamesCall(ctx, t, conns[second], "builtin", "gamesRecordTrace", trace("t-b-"+id.NewShortId(), 5))
	if len(rows) == 0 {
		t.Fatalf("recording the second trace answered no rows")
	}
	skillID, _ := rows[0]["learnedSkillId"].(string)
	if skillID == "" {
		t.Fatalf("two successful traces through two connections taught no skill: %v", rows)
	}
	gamesAwaitAll(t, conns, heard, "v1:games:skill", skillID, 20*time.Second)
}
