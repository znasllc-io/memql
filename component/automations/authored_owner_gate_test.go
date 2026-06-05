package automations_test

// authored_owner_gate_test.go -- coverage for cluster leader-gating of
// owner-scoped authored automations (epic memql#954, issue #959, increment 4).
//
// Two layers under test:
//   - AuthoredOwnerGate in isolation: single-node defaults, deterministic
//     owner->node assignment, every-node-agrees, exactly-one-node-per-owner,
//     rebalance on membership change.
//   - The gate wired into AuthoredScheduler: the OWNING node runs an owner's
//     authored automation firing; a NON-owning node does not -- so an owner's
//     automation fires once cluster-wide instead of once per replica.

import (
	"context"
	"sync"
	"testing"

	"github.com/znasllc-io/memql/component/automations"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
)

// --- gate in isolation ---

// TestOwnerGate_NilAndSingleNode: a nil gate or a single-node membership owns
// every owner (the pre-cluster default).
func TestOwnerGate_NilAndSingleNode(t *testing.T) {
	var g *automations.AuthoredOwnerGate // nil
	if !g.OwnsOwner("anyone") {
		t.Error("a nil gate must own every owner (single-node / dev)")
	}

	// Membership returning just this node -> owns everything.
	g1 := automations.NewAuthoredOwnerGate("nodeA", func() []string { return []string{"nodeA"} })
	if !g1.OwnsOwner("user-x") || !g1.OwnsOwner("user-y") {
		t.Error("single-node membership must own every owner")
	}

	// Empty membership -> assume single node (this node owns everything).
	g2 := automations.NewAuthoredOwnerGate("nodeA", func() []string { return nil })
	if !g2.OwnsOwner("user-x") {
		t.Error("empty membership must fall back to single-node ownership")
	}
}

// TestOwnerGate_ExactlyOneNodePerOwner: across a 3-node cluster, every owner is
// owned by EXACTLY one node, and all nodes agree on the assignment.
func TestOwnerGate_ExactlyOneNodePerOwner(t *testing.T) {
	nodes := []string{"nodeA", "nodeB", "nodeC"}
	membership := func() []string { return nodes }
	gates := map[string]*automations.AuthoredOwnerGate{}
	for _, n := range nodes {
		gates[n] = automations.NewAuthoredOwnerGate(n, membership)
	}

	owners := []string{"user-1", "user-2", "user-3", "user-4", "user-5", "user-6", "user-7", "user-8"}
	dist := map[string]int{}
	for _, owner := range owners {
		ownerCount := 0
		var assigned string
		for _, n := range nodes {
			if gates[n].OwnsOwner(owner) {
				ownerCount++
				assigned = n
			}
		}
		if ownerCount != 1 {
			t.Fatalf("owner %q is owned by %d nodes, want exactly 1", owner, ownerCount)
		}
		// All nodes agree on WHICH node owns it (coordination-free determinism).
		for _, n := range nodes {
			if got := gates[n].AssignedNode(owner); got != assigned {
				t.Fatalf("node %q disagrees on owner %q assignment: %q vs %q", n, owner, got, assigned)
			}
		}
		dist[assigned]++
	}
	// Sanity: the 8 owners spread across more than one node (not all on one).
	if len(dist) < 2 {
		t.Errorf("expected owners to distribute across multiple nodes, got %v", dist)
	}
}

// TestOwnerGate_StableAndRebalances: assignment is stable while membership is
// unchanged, and changes when the node set changes.
func TestOwnerGate_StableAndRebalances(t *testing.T) {
	nodes := []string{"nodeA", "nodeB", "nodeC"}
	g := automations.NewAuthoredOwnerGate("nodeA", func() []string { return nodes })

	a1 := g.AssignedNode("user-42")
	a2 := g.AssignedNode("user-42")
	if a1 != a2 {
		t.Fatalf("assignment must be stable under fixed membership: %q vs %q", a1, a2)
	}

	// Membership shrinks -> re-derives over the new set. Find an owner whose
	// assignment changes when a node leaves (at least one must, since the
	// hash space re-partitions).
	shrunk := []string{"nodeA", "nodeB"}
	g2 := automations.NewAuthoredOwnerGate("nodeA", func() []string { return shrunk })
	changed := false
	for _, owner := range []string{"u1", "u2", "u3", "u4", "u5", "u6", "u7", "u8", "u9", "u10"} {
		if g.AssignedNode(owner) != g2.AssignedNode(owner) {
			changed = true
			break
		}
	}
	if !changed {
		t.Error("expected at least one owner to rebalance when a node leaves")
	}
}

// TestOwnerGate_EmptyOwnerNeverOwned: an empty owner id is owned by no node
// (an unowned firing should run nowhere, not everywhere).
func TestOwnerGate_EmptyOwnerNeverOwned(t *testing.T) {
	g := automations.NewAuthoredOwnerGate("nodeA", func() []string { return []string{"nodeA", "nodeB"} })
	if g.OwnsOwner("") {
		t.Error("empty owner must never be owned")
	}
}

// --- gate wired into the scheduler ---

// TestOwnerGate_SchedulerFiresOnlyOnOwningNode: two schedulers model two nodes
// over the same event bus + the same 2-node membership. For a given owner,
// exactly ONE node's scheduler fires the authored automation -- so it runs
// once cluster-wide, not once per replica.
func TestOwnerGate_SchedulerFiresOnlyOnOwningNode(t *testing.T) {
	loadConceptsForAuthored(t)
	bus := events.NewBus()
	const owner = "v1:identity:user:alice"

	nodes := []string{"nodeA", "nodeB"}
	membership := func() []string { return nodes }

	// Determine which node owns `owner` so the test asserts against the truth
	// the gate computes (rather than hard-coding a hash result).
	probe := automations.NewAuthoredOwnerGate("nodeA", membership)
	owningNode := probe.AssignedNode(owner)

	type nodeRig struct {
		id    string
		sched *automations.AuthoredScheduler
		runs  *int32mu
	}
	var rigs []nodeRig
	for _, n := range nodes {
		counter := &int32mu{}
		gate := automations.NewAuthoredOwnerGate(n, membership)
		loader := automations.NewLoader(automations.LoaderOptions{Registry: concept.DefaultRegistry()})
		s, err := automations.NewAuthoredScheduler(automations.AuthoredSchedulerOptions{
			Loader:    loader,
			EventBus:  bus,
			Run:       func(_ context.Context, _ *automations.Automation, _ *events.Event) error { counter.inc(); return nil },
			OwnerGate: gate.OwnsOwner,
		})
		if err != nil {
			t.Fatalf("NewAuthoredScheduler(%s): %v", n, err)
		}
		defer s.Stop()
		// Both nodes activate the SAME owner's authored automation (each node
		// runs the authored scheduler and subscribes the trigger).
		if err := s.Activate(authoredAutomationConstruct(owner, "aliceOnUserCreate", eventAutomationSrc("aliceOnUserCreate"))); err != nil {
			t.Fatalf("activate on %s: %v", n, err)
		}
		rigs = append(rigs, nodeRig{id: n, sched: s, runs: counter})
	}

	// One event reaches both nodes' subscriptions; only the owning node runs it.
	bus.PublishSync(events.NewEvent("graph.node.created.v1:identity:user", events.KindNodeCreated, map[string]any{"id": "bob"}))

	for _, r := range rigs {
		got := r.runs.get()
		if r.id == owningNode {
			if got != 1 {
				t.Errorf("owning node %s should fire exactly once, got %d", r.id, got)
			}
		} else {
			if got != 0 {
				t.Errorf("non-owning node %s must NOT fire (leader-gated), got %d", r.id, got)
			}
		}
	}
}

// int32mu is a tiny thread-safe counter for the run tallies.
type int32mu struct {
	mu sync.Mutex
	n  int
}

func (c *int32mu) inc() { c.mu.Lock(); c.n++; c.mu.Unlock() }
func (c *int32mu) get() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}
