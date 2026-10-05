package workbench

import (
	"testing"

	"github.com/znasllc-io/memql/component/node"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
)

// TestAnExcludedReplicaIsPassedOverWhileAnotherIsHealthy pins the exclusion
// a pipeline step's re-forward uses (epic memql#5478): when the runner holding
// a step's Job went quiet on a replica the mesh still thinks healthy, the step
// is forwarded again AWAY from that replica -- handing it back would wait out
// another round of patience for nothing -- unless it is the one healthy
// replica left, where a hop that can go somewhere beats none.
func TestAnExcludedReplicaIsPassedOverWhileAnotherIsHealthy(t *testing.T) {
	a := &node.PeerEntry{Info: &nodev1.PeerInfo{NodeId: "workbench-a"}}
	b := &node.PeerEntry{Info: &nodev1.PeerInfo{NodeId: "workbench-b"}}

	for _, order := range [][]*node.PeerEntry{{a, b}, {b, a}} {
		if got := selectWorkbenchPeerExcluding(order, "", "workbench-a", alwaysReachable); got != b {
			t.Errorf("order %s,%s: picked %v, want workbench-b: the excluded replica is passed over",
				order[0].Info.GetNodeId(), order[1].Info.GetNodeId(), got.Info.GetNodeId())
		}
		// The exclusion outranks a pin on the same replica: the caller
		// excluding it has learned something the pin has not.
		if got := selectWorkbenchPeerExcluding(order, "workbench-a", "workbench-a", alwaysReachable); got != b {
			t.Errorf("pinned and excluded workbench-a: picked %v, want workbench-b", got.Info.GetNodeId())
		}
		// A pin on another replica is still a pin.
		if got := selectWorkbenchPeerExcluding(order, "workbench-b", "workbench-a", alwaysReachable); got != b {
			t.Errorf("pinned workbench-b, excluded workbench-a: picked %v", got.Info.GetNodeId())
		}
	}

	// The one healthy replica left is chosen even though it is excluded.
	if got := selectWorkbenchPeerExcluding([]*node.PeerEntry{a}, "", "workbench-a", alwaysReachable); got != a {
		t.Errorf("only workbench-a is healthy: picked %v, want it -- excluding the last replica would strand the step", got)
	}
	onlyA := func(p *node.PeerEntry) bool { return p == a }
	if got := selectWorkbenchPeerExcluding([]*node.PeerEntry{a, b}, "", "workbench-a", onlyA); got != a {
		t.Errorf("workbench-b unreachable: picked %v, want the excluded but reachable workbench-a", got)
	}
	// Nothing reachable is still nothing.
	never := func(*node.PeerEntry) bool { return false }
	if got := selectWorkbenchPeerExcluding([]*node.PeerEntry{a, b}, "", "workbench-a", never); got != nil {
		t.Errorf("no reachable replica: picked %v, want none", got.Info.GetNodeId())
	}
	// No exclusion, or one naming no candidate, is any-fit as before.
	for _, exclude := range []string{"", "workbench-9"} {
		if got := selectWorkbenchPeerExcluding([]*node.PeerEntry{a, b}, "", exclude, alwaysReachable); got != a {
			t.Errorf("exclude %q: picked %v, want the first reachable replica", exclude, got.Info.GetNodeId())
		}
	}
	if got := selectWorkbenchPeer([]*node.PeerEntry{a, b}, "workbench-b", alwaysReachable); got != b {
		t.Errorf("selectWorkbenchPeer no longer honours its pin: picked %v", got.Info.GetNodeId())
	}
}
