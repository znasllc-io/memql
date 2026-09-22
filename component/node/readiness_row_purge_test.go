package node

import (
	"context"
	"errors"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/metrics"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
)

// What the STATEMENTS do is readiness_row_purge_db_test.go's job, against a
// real database. These pin the CALL: who is purged, when, and what a failure
// does to the retire it rides on.

// fakePurger records what it was asked to remove and can be made to fail.
type fakePurger struct {
	nodes  []string
	sweeps int
	err    error
}

func (f *fakePurger) purgeForNode(_ context.Context, nodeId string) (int64, error) {
	if f.err != nil {
		return 0, f.err
	}
	f.nodes = append(f.nodes, nodeId)
	return 7, nil
}

func (f *fakePurger) purgeForStoppedNodes(_ context.Context) (int64, error) {
	if f.err != nil {
		return 0, f.err
	}
	f.sweeps++
	return 14, nil
}

func TestRetiringANodePurgesItsReadinessRowsUnderItsBareId(t *testing.T) {
	clock := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	fresh := clock.Format(time.RFC3339)
	eng := &fakeReconcileEngine{
		nodes: []*memqlv1.MemoryNode{
			nodeRow(t, "agent-7d9f-x2k", "healthy", "deploy-old", fresh),
			nodeRow(t, "agent-7d9f-q4m", "healthy", "deploy-new", fresh),
		},
		deps: []*memqlv1.MemoryNode{deploymentRow(t, "deploy-old", "superseded")},
	}
	mesh := fakeMesh{peers: []*PeerEntry{
		livePeer("agent-7d9f-x2k", nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY),
		livePeer("agent-7d9f-q4m", nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY),
	}}
	r := newTestReconciler(eng, mesh, &clock)
	purger := &fakePurger{}
	r.readinessPurger = purger

	r.reconcile(context.Background())

	// The BARE id, not the v1:cluster:node:-prefixed row id. A readiness row's
	// nodeId carries MEMQL_NODE_ID, so purging under the prefixed form would
	// match nothing and fail silently -- the one way this could look like it
	// works while removing nothing.
	if len(purger.nodes) != 1 || purger.nodes[0] != "agent-7d9f-x2k" {
		t.Fatalf("expected the retired node purged under its bare id, got %v", purger.nodes)
	}
	// The node that stayed keeps its rows.
	for _, id := range purger.nodes {
		if id == "agent-7d9f-q4m" {
			t.Fatal("a node that was not retired had its readiness rows purged")
		}
	}
}

func TestAFailedPurgeStillLeavesTheNodeRetired(t *testing.T) {
	clock := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	fresh := clock.Format(time.RFC3339)
	eng := &fakeReconcileEngine{
		nodes: []*memqlv1.MemoryNode{nodeRow(t, "agent-gone", "healthy", "deploy-old", fresh)},
		deps:  []*memqlv1.MemoryNode{deploymentRow(t, "deploy-old", "superseded")},
	}
	r := newTestReconciler(eng, fakeMesh{peers: []*PeerEntry{
		livePeer("agent-gone", nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY),
	}}, &clock)
	r.readinessPurger = &fakePurger{err: errors.New("connection reset")}

	r.reconcile(context.Background())

	// The terminal health row is the thing that matters; its cleanup is not.
	// Failing the retire over a failed purge would leave a live-looking node
	// row behind, and the sweep collects the rows on its next pass anyway.
	if got := eng.retiredIDs(t); len(got) != 1 || got[0] != "agent-gone" {
		t.Fatalf("expected the node retired despite the purge failing, got %v", got)
	}
}

func TestTheStoppedNodeSweepRunsOncePerInterval(t *testing.T) {
	clock := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	r := newTestReconciler(&fakeReconcileEngine{}, fakeMesh{}, &clock)
	purger := &fakePurger{}
	r.readinessPurger = purger

	ctx := context.Background()

	// The reconciler ticks every 3 s. A sweep on every tick would be 200
	// scans of the concept per interval for the handful of rows a rollout
	// leaves, so the first call runs and the ones inside the window do not.
	r.sweepReadinessRowsOfStoppedNodes(ctx)
	if purger.sweeps != 1 {
		t.Fatalf("the first sweep must run, got %d", purger.sweeps)
	}
	for i := 0; i < 5; i++ {
		clock = clock.Add(readinessPurgeInterval / 10)
		r.sweepReadinessRowsOfStoppedNodes(ctx)
	}
	if purger.sweeps != 1 {
		t.Fatalf("sweeps inside the interval must be skipped, got %d", purger.sweeps)
	}

	clock = clock.Add(readinessPurgeInterval)
	r.sweepReadinessRowsOfStoppedNodes(ctx)
	if purger.sweeps != 2 {
		t.Fatalf("a sweep past the interval must run, got %d", purger.sweeps)
	}
}

func TestAReconcilerWithNoDatabasePurgesNothingAndDoesNotPanic(t *testing.T) {
	clock := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	// newTestReconciler leaves dbGetter nil, which is what a DB-less binary
	// and every pre-existing reconciler test look like. The purge must be a
	// no-op there rather than a nil dereference on the retire path.
	r := newTestReconciler(&fakeReconcileEngine{}, fakeMesh{}, &clock)
	if p := r.purger(); p != nil {
		t.Fatalf("a reconciler with no db must have no purger, got %#v", p)
	}
	r.purgeReadinessRowsForNode(context.Background(), "any-node")
	r.sweepReadinessRowsOfStoppedNodes(context.Background())
}

func TestABlankNodeIdIsRefusedRatherThanPurgingEveryRow(t *testing.T) {
	// A blank nodeId in `payload->>'nodeId' = ''` matches every row that
	// carries no nodeId at all. The guard runs before the nil-db shortcut so
	// it is a refusal on a build with no database too -- which is where this
	// test runs.
	_, err := sqlReadinessRowPurger{}.purgeForNode(context.Background(), "")
	if err == nil {
		t.Fatal("a blank node id must be refused, not run")
	}
	// And a real id with no database is simply nothing to do.
	n, err := sqlReadinessRowPurger{}.purgeForNode(context.Background(), "agent-1")
	if err != nil || n != 0 {
		t.Fatalf("no database means no work and no error, got %d %v", n, err)
	}
}

// The two paths are counted SEPARATELY, and that is the point of the label:
// `swept` carrying the whole rate while `retired` stays at zero is what "the
// fast path is not running" looks like, and a summed counter cannot say it.
func TestTheTwoPurgePathsAreCountedApart(t *testing.T) {
	clock := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	fresh := clock.Format(time.RFC3339)
	beforeRetired := metrics.ModuleReadinessRowsPurgedValue(readinessPurgePathRetired)
	beforeSwept := metrics.ModuleReadinessRowsPurgedValue(readinessPurgePathSwept)

	eng := &fakeReconcileEngine{
		nodes: []*memqlv1.MemoryNode{nodeRow(t, "agent-counted", "healthy", "deploy-old", fresh)},
		deps:  []*memqlv1.MemoryNode{deploymentRow(t, "deploy-old", "superseded")},
	}
	r := newTestReconciler(eng, fakeMesh{peers: []*PeerEntry{
		livePeer("agent-counted", nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY),
	}}, &clock)
	r.readinessPurger = &fakePurger{}

	r.reconcile(context.Background())                        // one retire -> 7
	r.sweepReadinessRowsOfStoppedNodes(context.Background()) // one sweep  -> 14

	if got := metrics.ModuleReadinessRowsPurgedValue(readinessPurgePathRetired) - beforeRetired; got != 7 {
		t.Errorf("retired path: want 7 versions counted, got %v", got)
	}
	if got := metrics.ModuleReadinessRowsPurgedValue(readinessPurgePathSwept) - beforeSwept; got != 14 {
		t.Errorf("swept path: want 14 versions counted, got %v", got)
	}
}
