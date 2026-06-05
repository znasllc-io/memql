package automations

// authored_owner_gate.go -- cluster leader-gating for owner-scoped authored
// automations (epic memql#954, issue #959, increment 4).
//
// Every node runs the authored scheduler, so an authored automation's trigger
// fires on each node that subscribed it -- and a cron entry ticks on each node
// independently. For the SAME reason the core scheduler gates cron firings to
// one cluster-wide leader (#561, cron_leader.go), the authored runtime must
// gate too, or an owner's authored automation runs once-per-replica.
//
// But the authored layer is OWNER-SCOPED, and the issue asks specifically to
// "define which node owns an owner's authored automations." So instead of one
// global leader running EVERY owner's automations (a hot spot), this gate
// DISTRIBUTES owners across the live nodes: a deterministic assignment maps
// each ownerUserId to exactly one node. A node runs an owner's authored
// automation firing only when it is that owner's assigned node. The assignment
// is stable (same owner -> same node while membership is unchanged) and
// rebalances when the node set changes.
//
// Determinism: assignment is hash(ownerUserId) over the SORTED live node ids,
// so every node computes the identical owner->node map from the same
// membership view -- no coordination round-trip. This mirrors the
// rendezvous/consistent-assignment pattern the peer mesh already relies on for
// routing.
//
// Single-node / dev: when the membership view is empty or has one node, this
// node owns every owner (the pre-cluster behaviour), so authored automations
// fire normally on a single node without any gating overhead.

import (
	"hash/fnv"
	"sort"
)

// MembershipFunc returns the current set of live node ids in the cluster
// (including this node). The authored owner gate reads it on every firing so a
// membership change rebalances owner assignments without a restart. An empty
// or nil result means "single node" -- this node owns everything.
type MembershipFunc func() []string

// AuthoredOwnerGate decides whether THIS node owns a given owner's authored
// automations, so an owner's authored automation fires once cluster-wide
// (on its assigned node) rather than once per replica.
type AuthoredOwnerGate struct {
	selfNodeId string
	membership MembershipFunc
}

// NewAuthoredOwnerGate builds the gate. selfNodeId is this node's stable id
// (e.g. node.Identity.NodeId()); membership returns the live node set. A nil
// membership (or one that returns <=1 node) makes this node own every owner --
// the single-node default.
func NewAuthoredOwnerGate(selfNodeId string, membership MembershipFunc) *AuthoredOwnerGate {
	return &AuthoredOwnerGate{selfNodeId: selfNodeId, membership: membership}
}

// OwnsOwner reports whether this node is the assigned runner for ownerUserId's
// authored automations. It is the func wired into the authored scheduler's
// owner gate. Owner with empty id is never owned (defensive -- an unowned
// firing should not run anywhere rather than everywhere).
func (g *AuthoredOwnerGate) OwnsOwner(ownerUserId string) bool {
	if g == nil {
		return true // no gate configured -> run (single-node / dev)
	}
	if ownerUserId == "" {
		return false
	}
	return g.assignedNode(ownerUserId) == g.selfNodeId
}

// AssignedNode returns the node id that owns ownerUserId's authored
// automations under the current membership view. Exported for management
// surfaces ("which node runs owner X's automations"). Returns this node's id
// when membership is single-node.
func (g *AuthoredOwnerGate) AssignedNode(ownerUserId string) string {
	if g == nil {
		return ""
	}
	return g.assignedNode(ownerUserId)
}

// assignedNode computes the deterministic owner->node assignment from the
// SORTED live node set. Every node derives the same mapping from the same
// membership, so exactly one node owns each owner without coordination.
func (g *AuthoredOwnerGate) assignedNode(ownerUserId string) string {
	nodes := g.liveNodes()
	switch len(nodes) {
	case 0:
		// No membership info -> single-node assumption: this node owns it.
		return g.selfNodeId
	case 1:
		return nodes[0]
	}
	idx := hashOwner(ownerUserId) % uint64(len(nodes))
	return nodes[idx]
}

// liveNodes returns the sorted, de-duplicated live node set. When membership
// is nil or empty, falls back to just this node (single-node default).
func (g *AuthoredOwnerGate) liveNodes() []string {
	var raw []string
	if g.membership != nil {
		raw = g.membership()
	}
	if len(raw) == 0 {
		if g.selfNodeId == "" {
			return nil
		}
		return []string{g.selfNodeId}
	}
	// De-dup + sort for a stable, coordination-free assignment across nodes.
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, n := range raw {
		if n == "" {
			continue
		}
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// hashOwner is a stable 64-bit hash of an owner id, used to map owners to node
// slots. FNV-1a -- fast, deterministic, no crypto needed for assignment.
func hashOwner(ownerUserId string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(ownerUserId))
	return h.Sum64()
}
