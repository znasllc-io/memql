package workbench

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/node"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
)

// workspace_affinity_test.go -- memql#4354.
//
// A workspace is a FILESYSTEM, and a filesystem does not follow the request.
// The base manifest runs two workbench replicas and the agent's peer picker was
// any-fit, so a plan's first call made a directory on one replica and its
// second call landed on the other with even odds. The failure that produced was
// an fs_write followed by an fs_read of the same path answering "not found",
// with both calls reporting ok=true and neither result naming a node -- so it
// read as the agent having imagined the write.
//
// The tests below are the two halves of the fix: the pin (a row that records
// which replica holds the directory, written under the plan owner's actor) and
// the picker that honours it.

// ---------------------------------------------------------------------------
// A fake graph, one row store shared by every replica in the fake cluster.
// ---------------------------------------------------------------------------

// fakeGraph stands in for the engine + database. It answers the four
// v1:workbench:workspace calls plus planById, and -- the part that matters --
// it ENFORCES THE ROW-AUTHZ TIER: workspaceForPlan returns only rows owned by
// the actor on the context, and no actor means no rows.
//
// That is not decoration. @rowAuthz(owner="ownerUserId", clusterOwner) has no
// internal-origin bypass, so an unactored read comes back EMPTY rather than
// failing, which the integration would read as "this plan has no workspace" and
// answer by provisioning another one. A fake that ignored the actor would let
// every test below pass against code that never stamped anything.
type fakeGraph struct {
	mu sync.Mutex

	// planOwner is what planById reports as requestedBy.
	planOwner string

	rows []map[string]string

	provisionCalls []map[string]string
	releaseCalls   []map[string]string
	touchCalls     []string

	// unactoredReads counts reads that arrived with no actor bound. A test
	// asserts this stays zero on the happy path: the tier is only satisfied
	// because the integration stamps, not because the fake is lenient.
	unactoredReads int
}

var fakeArgPattern = regexp.MustCompile(`(\w+)\s*:\s*"((?:[^"\\]|\\.)*)"`)

func fakeArgs(q string) map[string]string {
	out := map[string]string{}
	for _, m := range fakeArgPattern.FindAllStringSubmatch(q, -1) {
		out[m[1]] = strings.ReplaceAll(m[2], `\"`, `"`)
	}
	return out
}

// run is the workspaceStore.exec seam: one MemQL call in, projected rows out.
func (g *fakeGraph) run(ctx context.Context, q string) ([]map[string]any, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	actor := ""
	if access, ok := auth.AccessFromContext(ctx); ok && access != nil {
		actor = access.UserId
	}
	args := fakeArgs(q)

	switch {
	case strings.HasPrefix(q, "query planById("):
		if g.planOwner == "" {
			// An unreadable / absent plan. Deliberately NOT "a row with a blank
			// requestedBy": planOwnerFromRow falls back to the row-intrinsic
			// createdBy (memql#952), which on the planner path is
			// "system:planner" -- so a row that exists always resolves to
			// something. The unresolvable case is the row not being there.
			return nil, nil
		}
		return []map[string]any{{
			"id":          args["planId"],
			"requestedBy": g.planOwner,
			"createdBy":   "system:planner",
		}}, nil

	case strings.HasPrefix(q, "query workspaceForPlan("):
		if actor == "" {
			g.unactoredReads++
			// THE TIER. No actor, no rows -- and no error either, which is the
			// shape that makes an unstamped read look like an empty plan.
			return nil, nil
		}
		var out []map[string]any
		for _, row := range g.rows {
			if row["planId"] != args["planId"] || row["ownerUserId"] != actor {
				continue
			}
			projected := map[string]any{}
			for k, v := range row {
				projected[k] = v
			}
			out = append(out, projected)
		}
		return out, nil

	case strings.HasPrefix(q, "mutation provisionWorkspace("):
		if actor == "" {
			return nil, fmt.Errorf("provisionWorkspace with no actor would stamp ownerUserId=\"\"")
		}
		g.provisionCalls = append(g.provisionCalls, args)
		g.rows = append(g.rows, map[string]string{
			"id":          args["workspaceId"],
			"planId":      args["planId"],
			"storageRoot": args["storageRoot"],
			"nodeId":      args["nodeId"],
			"status":      workspaceStatusProvisioned,
			"ownerUserId": actor,
		})
		return nil, nil

	case strings.HasPrefix(q, "mutation releaseWorkspace("):
		g.releaseCalls = append(g.releaseCalls, args)
		for _, row := range g.rows {
			if row["id"] == args["workspaceId"] {
				row["status"] = workspaceStatusReleased
				row["releasedReason"] = args["reason"]
			}
		}
		return nil, nil

	case strings.HasPrefix(q, "mutation touchWorkspace("):
		g.touchCalls = append(g.touchCalls, args["workspaceId"])
		return nil, nil
	}
	// Everything else (createGeneratedOutput, ...) is out of scope here.
	return nil, nil
}

func (g *fakeGraph) liveRows(planId string) []map[string]string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []map[string]string
	for _, row := range g.rows {
		if row["planId"] == planId && row["status"] == workspaceStatusProvisioned {
			out = append(out, row)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// A fake two-replica workbench cluster.
// ---------------------------------------------------------------------------

// replica is one workbench node: its own Manager (its own disk root, which is
// the whole point) and its own node id, over the shared graph.
type replica struct {
	nodeId string
	integ  *Integration
	entry  *node.PeerEntry
}

// newCluster builds n workbench replicas over one shared graph. Each gets its
// own workspace root directory, so a file written through one is genuinely
// absent from the others -- exactly as two pods with two emptyDirs.
func newCluster(t *testing.T, graph *fakeGraph, logger *slog.Logger, nodeIds ...string) []*replica {
	t.Helper()
	out := make([]*replica, 0, len(nodeIds))
	for _, id := range nodeIds {
		root := t.TempDir()
		integ := &Integration{
			manager: &Manager{root: root, cache: map[string]*workspace{}, clock: time.Now},
			logger:  logger,
			store:   &workspaceStore{exec: graph.run},
		}
		out = append(out, &replica{
			nodeId: id,
			integ:  integ,
			entry: &node.PeerEntry{Info: &nodev1.PeerInfo{
				NodeId: id,
				Health: nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY,
			}},
		})
	}
	return out
}

// alwaysReachable is the reachability predicate stand-in. A live
// *peerConnection cannot be constructed outside component/node, which is why
// selectWorkbenchPeer takes the predicate as a parameter.
func alwaysReachable(p *node.PeerEntry) bool { return p != nil && p.Info != nil }

// dispatchTo runs one call against a specific replica, standing in for the hop
// the agent's forward router makes. The receiving side is the ordinary local
// path -- exactly what ForwardHandler invokes on the workbench node -- so this
// exercises the real recordWorkspace bookkeeping.
func dispatchTo(t *testing.T, r *replica, ctx context.Context, planId string, args map[string]any) dispatchResult {
	t.Helper()
	t.Setenv("MEMQL_NODE_ID", r.nodeId)
	full := map[string]any{"planId": planId, "action": args["action"], "args": args["args"]}
	nodes, err := r.integ.handleDispatchHost(ctx, full, 0)
	if err != nil {
		t.Fatalf("dispatch on %s: %v", r.nodeId, err)
	}
	return decodeDispatch(t, nodes)
}

// pinFor asks the graph, through the integration, which replica holds the
// plan's workspace -- the same read the agent does before picking a peer.
func pinFor(t *testing.T, r *replica, ctx context.Context, planId, owner string) string {
	t.Helper()
	return r.integ.pinnedWorkspaceNode(ctx, planId, owner)
}

const (
	testPlanId    = "v1:planner:plan:p4354"
	testPlanOwner = "v1:identity:user:alice"
)

// ---------------------------------------------------------------------------
// The affinity test.
// ---------------------------------------------------------------------------

// TestOnePlanKeepsOneWorkspaceAcrossThreeCallsWithTwoReplicas is the issue.
//
// Three calls, two healthy replicas, one plan. The candidate order is ROTATED
// between calls because that is what really happens: PeerManager.ByType builds
// its slice from a map, so any-fit is a fresh coin flip per call rather than a
// stable wrong answer. Rotating makes the any-fit outcome deterministic --
// call 2 would land on the other replica every time -- so this test fails
// against any-fit rather than failing half the time, while affinity pins all
// three calls regardless of the order it is handed.
//
// The assertion that carries the weight is the fs_read on calls 2 and 3. One
// provisionWorkspace call would be satisfied by bookkeeping that happened to
// dedupe; a file that is still there is the property the user experiences.
func TestOnePlanKeepsOneWorkspaceAcrossThreeCallsWithTwoReplicas(t *testing.T) {
	graph := &fakeGraph{planOwner: testPlanOwner}
	reps := newCluster(t, graph, slog.New(slog.DiscardHandler), "workbench-1", "workbench-2")
	byId := map[string]*replica{reps[0].nodeId: reps[0], reps[1].nodeId: reps[1]}
	ctx := context.Background()

	// Call 1: no workspace yet, so no pin. Writes the file.
	first := selectWorkbenchPeer([]*node.PeerEntry{reps[0].entry, reps[1].entry}, "", alwaysReachable)
	if first == nil {
		t.Fatal("no peer selected with two healthy replicas")
	}
	res := dispatchTo(t, byId[first.Info.GetNodeId()], ctx, testPlanId, map[string]any{
		"action": "fs_write",
		"args":   map[string]any{"path": "note.txt", "content": "the plan's working file"},
	})
	if !res.OK {
		t.Fatalf("the first call failed: %s / %s", res.ErrorCode, res.ErrorMsg)
	}

	// Calls 2 and 3 read the file back, each handed the candidates in a
	// different order.
	orders := [][]*node.PeerEntry{
		{reps[1].entry, reps[0].entry},
		{reps[0].entry, reps[1].entry},
	}
	for n, order := range orders {
		pin := pinFor(t, reps[0], ctx, testPlanId, testPlanOwner)
		if pin == "" {
			t.Fatalf("call %d: no pin recorded, so affinity has nothing to act on -- "+
				"the workspace row was never written", n+2)
		}
		peer := selectWorkbenchPeer(order, pin, alwaysReachable)
		if peer.Info.GetNodeId() != pin {
			t.Fatalf("call %d: picked %s but the workspace is on %s. Any-fit selection sends a plan's "+
				"second call to a replica whose disk has never seen its files.",
				n+2, peer.Info.GetNodeId(), pin)
		}
		res := dispatchTo(t, byId[peer.Info.GetNodeId()], ctx, testPlanId, map[string]any{
			"action": "fs_read",
			"args":   map[string]any{"path": "note.txt"},
		})
		if !res.OK {
			t.Fatalf("call %d read back the file the plan wrote and did not find it (%s): "+
				"this is the split -- the call landed on a replica with a different disk",
				n+2, res.ErrorCode)
		}
	}

	if got := len(graph.provisionCalls); got != 1 {
		t.Fatalf("provisionWorkspace called %d times for one plan, want 1. A second row means a second "+
			"directory on a second disk, which is the bug wearing bookkeeping.", got)
	}
	if got := len(graph.liveRows(testPlanId)); got != 1 {
		t.Fatalf("%d live workspace rows for one plan, want 1", got)
	}
	if got := len(graph.touchCalls); got != 3 {
		t.Errorf("touchWorkspace called %d times across three successful dispatches, want 3", got)
	}
	if graph.unactoredReads != 0 {
		t.Errorf("%d workspace reads arrived with no actor bound. Those come back EMPTY under "+
			"@rowAuthz(owner=...), so the integration would provision a fresh workspace every call.",
			graph.unactoredReads)
	}
}

// TestAffinityPrefersThePinnedReplicaWhateverOrderTheCandidatesArriveIn pins the
// selector itself, independent of dispatch. Every permutation, so no result
// here can be an accident of iteration order.
func TestAffinityPrefersThePinnedReplicaWhateverOrderTheCandidatesArriveIn(t *testing.T) {
	a := &node.PeerEntry{Info: &nodev1.PeerInfo{NodeId: "workbench-1"}}
	b := &node.PeerEntry{Info: &nodev1.PeerInfo{NodeId: "workbench-2"}}

	for _, order := range [][]*node.PeerEntry{{a, b}, {b, a}} {
		for _, want := range []string{"workbench-1", "workbench-2"} {
			got := selectWorkbenchPeer(order, want, alwaysReachable)
			if got == nil || got.Info.GetNodeId() != want {
				t.Fatalf("pinned %q, got %v -- affinity must not depend on candidate order", want, got)
			}
		}
	}

	// No pin, and a pin naming a replica that is not a candidate, both fall
	// back to any-fit rather than refusing: a workbench call with a reachable
	// peer must not fail because bookkeeping is stale.
	if got := selectWorkbenchPeer([]*node.PeerEntry{a, b}, "", alwaysReachable); got == nil {
		t.Error("an unpinned call with healthy peers selected nothing")
	}
	if got := selectWorkbenchPeer([]*node.PeerEntry{a, b}, "workbench-9", alwaysReachable); got == nil {
		t.Error("a pin naming a departed replica selected nothing; it must fall back to any-fit")
	}

	// An unreachable pinned peer is not a peer. Preferring it would send work
	// to a replica that cannot answer, which is worse than a fresh directory.
	onlyB := func(p *node.PeerEntry) bool { return p == b }
	if got := selectWorkbenchPeer([]*node.PeerEntry{a, b}, "workbench-1", onlyB); got != b {
		t.Errorf("selected %v; an unreachable pin must not beat a reachable substitute", got)
	}
}

// ---------------------------------------------------------------------------
// Node loss.
// ---------------------------------------------------------------------------

// TestNodeLossReprovisionsExactlyOnceAndSaysWhy covers the case the pin cannot
// fix: the replica holding the directory left the mesh, so the files are gone
// with it. There is nothing to migrate -- a file tree cannot be recovered from a
// node that is not there -- so the plan gets a fresh empty workspace and the
// reason is recorded, which is what turns "my file vanished" into an answerable
// question.
//
// "Exactly once" is the part worth pinning: a re-provision that fired on every
// subsequent call would leave the plan with a new empty directory per call,
// which is a worse failure than the split it replaced.
func TestNodeLossReprovisionsExactlyOnceAndSaysWhy(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	graph := &fakeGraph{planOwner: testPlanOwner}
	reps := newCluster(t, graph, logger, "workbench-1", "workbench-2")
	lost, survivor := reps[0], reps[1]
	ctx := context.Background()

	// The plan establishes its workspace on workbench-1.
	if res := dispatchTo(t, lost, ctx, testPlanId, map[string]any{
		"action": "fs_write",
		"args":   map[string]any{"path": "note.txt", "content": "written before the node went away"},
	}); !res.OK {
		t.Fatalf("seed call failed: %s / %s", res.ErrorCode, res.ErrorMsg)
	}
	originalRows := graph.liveRows(testPlanId)
	if len(originalRows) != 1 {
		t.Fatalf("want one live row after the seed call, got %d", len(originalRows))
	}
	originalId := originalRows[0]["id"]

	// workbench-1 leaves the mesh. The pin still names it; the picker now has
	// only the survivor to offer.
	pin := pinFor(t, survivor, ctx, testPlanId, testPlanOwner)
	if pin != lost.nodeId {
		t.Fatalf("pin = %q, want %q", pin, lost.nodeId)
	}
	peer := selectWorkbenchPeer([]*node.PeerEntry{survivor.entry}, pin, alwaysReachable)
	if peer == nil || peer.Info.GetNodeId() != survivor.nodeId {
		t.Fatalf("with the pinned replica gone the call must go to a healthy substitute, got %v", peer)
	}

	// Two further calls on the survivor. The first is the takeover; the second
	// must be an ordinary call against the workspace the first one made.
	for n := 0; n < 2; n++ {
		if res := dispatchTo(t, survivor, ctx, testPlanId, map[string]any{
			"action": "fs_write",
			"args":   map[string]any{"path": "after.txt", "content": "written on the survivor"},
		}); !res.OK {
			t.Fatalf("post-loss call %d failed: %s / %s", n+1, res.ErrorCode, res.ErrorMsg)
		}
	}

	if got := len(graph.provisionCalls); got != 2 {
		t.Fatalf("provisionWorkspace called %d times, want exactly 2 (the original + one takeover). "+
			"More than that means the plan gets a fresh empty directory on every call.", got)
	}
	if got := len(graph.releaseCalls); got != 1 {
		t.Fatalf("releaseWorkspace called %d times, want exactly 1", got)
	}
	rel := graph.releaseCalls[0]
	if rel["workspaceId"] != originalId {
		t.Errorf("released %q, want the orphaned row %q", rel["workspaceId"], originalId)
	}
	if rel["reason"] != releaseReasonNodeLost {
		t.Errorf("release reason = %q, want %q -- the reason IS the answer to \"where did my file go\"",
			rel["reason"], releaseReasonNodeLost)
	}
	live := graph.liveRows(testPlanId)
	if len(live) != 1 {
		t.Fatalf("%d live rows after the takeover, want exactly 1", len(live))
	}
	if live[0]["nodeId"] != survivor.nodeId {
		t.Errorf("the live row names %q, want the surviving replica %q", live[0]["nodeId"], survivor.nodeId)
	}
	if live[0]["id"] == originalId {
		t.Error("the successor reused the released row's id; one row cannot be both released and provisioned")
	}

	// The log has to name BOTH node ids and the plan, or an operator reading it
	// cannot tell a node loss from a plan that simply started fresh.
	out := logs.String()
	for _, want := range []string{lost.nodeId, survivor.nodeId, testPlanId, "NOT migrated"} {
		if !strings.Contains(out, want) {
			t.Errorf("the node-loss log does not mention %q.\nlog:\n%s", want, out)
		}
	}
}

// ---------------------------------------------------------------------------
// The tier itself.
// ---------------------------------------------------------------------------

// TestWorkspaceReadWithNoActorReturnsNothing is the test that would have passed
// before v1:workbench:workspace declared a tier, and now proves the stamping is
// what makes the feature work.
//
// @rowAuthz(owner="ownerUserId", clusterOwner) has no internal-origin bypass,
// and this package may not stamp one anyway (integrations/workbench is
// deliberately absent from the allowlist in the repo-root
// call_origin_conformance_test.go). So an unactored read returns ZERO ROWS and
// NO ERROR -- and "no rows" is exactly what "this plan has no workspace" looks
// like. Nothing downstream can tell those apart, which is why the read has to
// carry the owner rather than hope.
func TestWorkspaceReadWithNoActorReturnsNothing(t *testing.T) {
	graph := &fakeGraph{planOwner: testPlanOwner}
	store := &workspaceStore{exec: graph.run}
	ctx := context.Background()

	if err := store.provision(ctx, testPlanOwner, workspaceRow{
		Id:          "wbws-fixture",
		PlanId:      testPlanId,
		StorageRoot: "/var/lib/memql/workbenches/p4354",
		NodeId:      "workbench-1",
	}); err != nil {
		t.Fatalf("provision: %v", err)
	}

	// With the owner bound: the row is there.
	row, err := store.forPlan(ctx, testPlanId, testPlanOwner)
	if err != nil {
		t.Fatalf("actored read: %v", err)
	}
	if row == nil || row.NodeId != "workbench-1" {
		t.Fatalf("the actored read did not find the row it just wrote: %+v", row)
	}

	// With no owner: refused before it can reach the engine. The refusal is the
	// point -- letting the call through would return no rows and no error, and
	// the caller would provision a duplicate workspace believing there was none.
	if _, err := store.forPlan(ctx, testPlanId, ""); err == nil {
		t.Fatal("a read with no owner was allowed. It comes back EMPTY, which is indistinguishable " +
			"from an absent workspace, so the caller provisions a second directory on a second disk.")
	}
	if _, err := store.forPlan(ctx, testPlanId, "   "); err == nil {
		t.Fatal("a whitespace owner was allowed; auth.ContextWithUserActor trims to a no-op on it")
	}

	// And the write half. A blank owner must never reach provisionWorkspace:
	// ContextWithUserActor is a no-op on it, so ownerUserId would be stamped ""
	// and the row would be readable by nobody at all.
	if err := store.provision(ctx, "", workspaceRow{Id: "wbws-orphan", PlanId: testPlanId}); err == nil {
		t.Fatal("provisionWorkspace ran with no owner; the row it writes is owned by nobody")
	}

	// Reading as somebody else finds nothing -- the tier, not the query.
	other, err := store.forPlan(ctx, testPlanId, "v1:identity:user:mallory")
	if err != nil {
		t.Fatalf("read as another user: %v", err)
	}
	if other != nil {
		t.Errorf("another user's read returned %+v; the owner tier is not being applied", other)
	}
}

// TestDispatchRefusesWhenThePlanOwnerCannotBeResolved pins the loud failure.
//
// The quiet alternative -- carry on and write the row under a blank actor --
// succeeds at every layer: the mutation runs, the dispatch reports ok=true, and
// the row is stamped ownerUserId="" and is invisible to the user whose files it
// describes AND to the operator. The next call reads nothing, provisions again,
// and the split returns.
func TestDispatchRefusesWhenThePlanOwnerCannotBeResolved(t *testing.T) {
	graph := &fakeGraph{planOwner: ""} // planById finds no row at all
	reps := newCluster(t, graph, slog.New(slog.DiscardHandler), "workbench-1")
	root := reps[0].integ.manager.Root()

	res := dispatchTo(t, reps[0], context.Background(), testPlanId, map[string]any{
		"action": "exec",
		"args":   map[string]any{"cmd": "touch ran.marker"},
	})
	if res.OK {
		t.Fatal("a dispatch whose plan owner could not be resolved reported success; the workspace row " +
			"it wrote would be owned by nobody")
	}
	if res.ErrorCode != ErrCodeWorkspaceOwnerUnresolved {
		t.Errorf("errorCode = %q, want %q", res.ErrorCode, ErrCodeWorkspaceOwnerUnresolved)
	}
	if got := len(graph.provisionCalls); got != 0 {
		t.Errorf("%d provisionWorkspace calls on the refused path, want 0", got)
	}
	assertNothingRan(t, root, "a refused dispatch")
}

// TestSelfNodeIdMatchesTheIdentityDerivation guards the one thing that would
// silently disable affinity: the row is stamped with this node's id and
// compared against PeerInfo.node_id, which component/node derives from
// MEMQL_NODE_ID. Two spellings of "who am I" would mean the pin never matches
// and every call re-provisions, with every row looking correct.
func TestSelfNodeIdMatchesTheIdentityDerivation(t *testing.T) {
	t.Setenv("MEMQL_NODE_ID", "  workbench-7  ")
	if got := selfNodeId(); got != "workbench-7" {
		t.Errorf("selfNodeId() = %q, want the trimmed MEMQL_NODE_ID", got)
	}
	t.Setenv("MEMQL_NODE_ID", "")
	if got := selfNodeId(); got == "" {
		t.Error("selfNodeId() is empty with MEMQL_NODE_ID unset; component/node falls back to the " +
			"hostname there, and a blank id would pin every workspace to nobody")
	}
}

// TestDeriveWorkspaceIdIsStablePerNodeAndDistinctAcrossNodes states the two
// properties the node-loss path depends on.
func TestDeriveWorkspaceIdIsStablePerNodeAndDistinctAcrossNodes(t *testing.T) {
	same := deriveWorkspaceId(testPlanId, "workbench-1")
	if again := deriveWorkspaceId(testPlanId, "workbench-1"); again != same {
		t.Errorf("id is not stable for one (plan, node): %q vs %q -- a restarted replica would "+
			"duplicate the row instead of adopting it", same, again)
	}
	if other := deriveWorkspaceId(testPlanId, "workbench-2"); other == same {
		t.Error("two replicas derive the same id for one plan; the takeover cannot release the old " +
			"row and insert a successor if both are the same row")
	}
}

// TestAnUnreachableWorkbenchStillReportsNoWorkbenchPeer pins the ORDER of the
// two refusals.
//
// The bookkeeping check needs the plan owner, and it would be natural to
// resolve that once at the top of the dispatch. Doing so would mean an
// unreachable workbench with an unreadable plan answers
// workspace_owner_unresolved -- costing the operator the one message that names
// the missing MEMQL_WORKER_PEERS seed, which is the recurring deployment fault
// (memql#3450 / memql#3506). The peer answer has to come first.
func TestAnUnreachableWorkbenchStillReportsNoWorkbenchPeer(t *testing.T) {
	root := t.TempDir()
	i := remoteIntegration(t, root) // remote asserted, no router, no peer
	i.store = &workspaceStore{exec: (&fakeGraph{planOwner: ""}).run}

	nodes, err := i.handleDispatchHost(context.Background(), execArgs(testPlanId), 0)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if got := decodeDispatch(t, nodes).ErrorCode; got != "no_workbench_peer" {
		t.Fatalf("errorCode = %q, want no_workbench_peer -- a workspace-bookkeeping error must not "+
			"mask the missing peer seed", got)
	}
	assertNothingRan(t, root, "a remote-mode refusal")
}
