package node

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
)

// fakeReconcileEngine is a scripted reconcileEngine: it answers the two reads
// (staleClusterNodes / supersededDeployments) from fixed bundles and
// records every retire mutation it is asked to run.
type fakeReconcileEngine struct {
	nodes     []*memqlv1.MemoryNode
	deps      []*memqlv1.MemoryNode
	mutations []string
	failNodes bool
}

func (f *fakeReconcileEngine) Execute(_ context.Context, q string) (*memqlengine.ExecuteResult, error) {
	switch {
	case strings.HasPrefix(q, "staleClusterNodes"):
		if f.failNodes {
			return nil, errors.New("node load boom")
		}
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: f.nodes}}, nil
	case strings.HasPrefix(q, "supersededDeployments"):
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: f.deps}}, nil
	case strings.HasPrefix(q, "updateNodeHealth"):
		f.mutations = append(f.mutations, q)
		return &memqlengine.ExecuteResult{}, nil
	}
	return &memqlengine.ExecuteResult{}, nil
}

// retiredIDs returns the bare node ids the engine was asked to mark stopped.
func (f *fakeReconcileEngine) retiredIDs(t *testing.T) []string {
	t.Helper()
	var ids []string
	for _, m := range f.mutations {
		if !strings.Contains(m, `"stopped"`) && !strings.Contains(m, `"health":"stopped"`) {
			t.Fatalf("mutation is not a stopped transition: %s", m)
		}
		// Pull the "id":"<x>" value out of the JSON arg for assertion.
		const key = `"id":"`
		i := strings.Index(m, key)
		if i < 0 {
			t.Fatalf("mutation has no id: %s", m)
		}
		rest := m[i+len(key):]
		j := strings.IndexByte(rest, '"')
		ids = append(ids, rest[:j])
	}
	return ids
}

type fakeMesh struct{ peers []*PeerEntry }

func (m fakeMesh) AllPeers() []*PeerEntry { return m.peers }

func livePeer(id string, health nodev1.NodeHealthStatus) *PeerEntry {
	return &PeerEntry{Info: &nodev1.PeerInfo{NodeId: id, Health: health}}
}

func nodeRow(t *testing.T, id, health, deploymentId, lastSeen string) *memqlv1.MemoryNode {
	t.Helper()
	fields := map[string]any{
		"nodeType":     "agent",
		"address":      "agent:50051",
		"health":       health,
		"deploymentId": deploymentId,
	}
	if lastSeen != "" {
		fields["lastSeen"] = lastSeen
	}
	p, err := structpb.NewStruct(fields)
	if err != nil {
		t.Fatalf("build node payload: %v", err)
	}
	return &memqlv1.MemoryNode{Id: "_system:v1:cluster:node:" + id, Payload: p}
}

func deploymentRow(t *testing.T, deploymentId, status string) *memqlv1.MemoryNode {
	t.Helper()
	p, err := structpb.NewStruct(map[string]any{"deploymentId": deploymentId, "status": status})
	if err != nil {
		t.Fatalf("build deployment payload: %v", err)
	}
	return &memqlv1.MemoryNode{Id: "_system:v1:cluster:deployment:" + deploymentId, Payload: p}
}

// newTestReconciler wires a reconciler around fakes with a controllable clock.
// It bypasses NewTopologyReconciler (no Component/DB needed) and drives
// reconcile() directly.
func newTestReconciler(eng reconcileEngine, mesh meshMembership, clock *time.Time) *TopologyReconciler {
	return &TopologyReconciler{
		self:        &Identity{ID: "self-node"},
		peers:       mesh,
		engine:      eng,
		grace:       20 * time.Second,
		absentSince: map[string]time.Time{},
		now:         func() time.Time { return *clock },
	}
}

func TestReconcile_OrphanRetiredImmediately(t *testing.T) {
	clock := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	fresh := clock.Format(time.RFC3339)
	eng := &fakeReconcileEngine{
		// node-a belongs to a superseded deployment; node-b is current. Both
		// are PRESENT in the mesh and freshly seen -- liveness would never reap
		// them, proving the orphan path acts on deployment status alone.
		nodes: []*memqlv1.MemoryNode{
			nodeRow(t, "node-a", "healthy", "deploy-old", fresh),
			nodeRow(t, "node-b", "healthy", "deploy-new", fresh),
		},
		deps: []*memqlv1.MemoryNode{deploymentRow(t, "deploy-old", "superseded")},
	}
	mesh := fakeMesh{peers: []*PeerEntry{
		livePeer("node-a", nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY),
		livePeer("node-b", nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY),
	}}
	r := newTestReconciler(eng, mesh, &clock)

	r.reconcile(context.Background())

	got := eng.retiredIDs(t)
	if len(got) != 1 || got[0] != "node-a" {
		t.Fatalf("expected only node-a retired as orphan, got %v", got)
	}
}

func TestReconcile_LivenessReapHonorsGrace(t *testing.T) {
	start := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	clock := start
	old := start.Add(-10 * time.Minute).Format(time.RFC3339) // lastSeen well past grace
	eng := &fakeReconcileEngine{
		nodes: []*memqlv1.MemoryNode{nodeRow(t, "ghost", "healthy", "deploy-new", old)},
		deps:  nil, // no superseded deployments
	}
	// ghost is ABSENT from the mesh (empty peer set besides self).
	r := newTestReconciler(eng, fakeMesh{}, &clock)

	// Tick 1: first time absent -> tracked, not yet retired.
	r.reconcile(context.Background())
	if n := len(eng.mutations); n != 0 {
		t.Fatalf("expected no retire before grace elapses, got %d", n)
	}
	if _, tracked := r.absentSince["ghost"]; !tracked {
		t.Fatalf("expected ghost to be tracked as absent")
	}

	// Advance past the grace window and tick again -> retired.
	clock = start.Add(25 * time.Second)
	r.reconcile(context.Background())
	got := eng.retiredIDs(t)
	if len(got) != 1 || got[0] != "ghost" {
		t.Fatalf("expected ghost retired after grace, got %v", got)
	}
	if _, tracked := r.absentSince["ghost"]; tracked {
		t.Fatalf("expected ghost cleared from tracking after retire")
	}
}

func TestReconcile_LivePeerAndSelfNeverReaped(t *testing.T) {
	start := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	clock := start
	old := start.Add(-10 * time.Minute).Format(time.RFC3339)
	eng := &fakeReconcileEngine{
		nodes: []*memqlv1.MemoryNode{
			// present in mesh + stale lastSeen -> must NOT be reaped (mesh wins)
			nodeRow(t, "node-live", "healthy", "deploy-new", old),
			// self -> never reaped even though absent from the peer list
			nodeRow(t, "self-node", "healthy", "deploy-new", old),
		},
	}
	mesh := fakeMesh{peers: []*PeerEntry{livePeer("node-live", nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY)}}
	r := newTestReconciler(eng, mesh, &clock)

	// Two ticks past the grace window: still nothing retired.
	r.reconcile(context.Background())
	clock = start.Add(30 * time.Second)
	r.reconcile(context.Background())

	if n := len(eng.mutations); n != 0 {
		t.Fatalf("expected no retirements for live peer or self, got %d (%v)", n, eng.mutations)
	}
}

func TestReconcile_FreshlySeenAbsentNodeGuarded(t *testing.T) {
	start := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	clock := start
	// lastSeen only 5s ago: younger than the 20s grace -> the just-registered
	// guard must skip it even though it's absent from the mesh (gossip may not
	// have reached the leader yet).
	recent := start.Add(-5 * time.Second).Format(time.RFC3339)
	eng := &fakeReconcileEngine{
		nodes: []*memqlv1.MemoryNode{nodeRow(t, "newcomer", "healthy", "deploy-new", recent)},
	}
	r := newTestReconciler(eng, fakeMesh{}, &clock)

	r.reconcile(context.Background())
	// Second tick still inside the fresh window (lastSeen 15s old < 20s grace),
	// so the guard must keep skipping it: never tracked, never retired.
	clock = start.Add(10 * time.Second)
	r.reconcile(context.Background())

	if n := len(eng.mutations); n != 0 {
		t.Fatalf("expected freshly-seen absent node to be guarded, got %d retirements", n)
	}
	if _, tracked := r.absentSince["newcomer"]; tracked {
		t.Fatalf("freshly-seen node should not be tracked as absent while within the grace window")
	}
}

func TestReconcile_NodeLoadFailureIsSafe(t *testing.T) {
	clock := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	eng := &fakeReconcileEngine{failNodes: true}
	r := newTestReconciler(eng, fakeMesh{}, &clock)

	r.reconcile(context.Background()) // must not panic, must not write

	if n := len(eng.mutations); n != 0 {
		t.Fatalf("expected no writes when node load fails, got %d", n)
	}
}

func TestEnvDurationSeconds(t *testing.T) {
	def := 3 * time.Second
	t.Setenv("MEMQL_TEST_RECONCILE_X", "")
	if got := envDurationSeconds("MEMQL_TEST_RECONCILE_X", def, nil); got != def {
		t.Fatalf("unset should default: got %v", got)
	}
	t.Setenv("MEMQL_TEST_RECONCILE_X", "7")
	if got := envDurationSeconds("MEMQL_TEST_RECONCILE_X", def, nil); got != 7*time.Second {
		t.Fatalf("valid value should parse: got %v", got)
	}
	t.Setenv("MEMQL_TEST_RECONCILE_X", "0")
	if got := envDurationSeconds("MEMQL_TEST_RECONCILE_X", def, nil); got != def {
		t.Fatalf("non-positive should default: got %v", got)
	}
	t.Setenv("MEMQL_TEST_RECONCILE_X", "garbage")
	if got := envDurationSeconds("MEMQL_TEST_RECONCILE_X", def, nil); got != def {
		t.Fatalf("garbage should default: got %v", got)
	}
}

func TestBareNodeID(t *testing.T) {
	cases := map[string]string{
		"_system:v1:cluster:node:node-a": "node-a",
		"node-b":                         "node-b",
		"":                               "",
	}
	for in, want := range cases {
		if got := bareNodeID(in); got != want {
			t.Fatalf("bareNodeID(%q) = %q, want %q", in, got, want)
		}
	}
}
