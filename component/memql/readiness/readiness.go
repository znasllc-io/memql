// Package readiness is the pure decision layer of configuration readiness
// (design record docs/superpowers/specs/2026-09-06-configuration-readiness-design.md,
// section 4.5): values in, verdicts out, no engine, no database, no provider.
//
// It is a package of the component/memql MODULE, and the location is the
// point rather than an accident.
//
// It was first written as a package of the ROOT module, on the reasoning that
// "component/memql already depends on the root". IT DOES NOT -- the root
// requires component/memql, not the reverse, which is the correct direction
// and the one the module-boundaries lane exists to hold. Workspace mode
// resolves the import anyway, so `make test`, every editor and every other CI
// lane were green; only `GOWORK=off` saw it, and it failed there for
// SEVENTEEN modules at once, because every module with a relative-path
// replace onto component/memql inherits its unsatisfiable import.
//
// A nested module of its own was the other option and is worse: a new go.mod
// trips a dozen gates, three of which no local test run can see. Here it is
// an ordinary sibling package of its one importer, and the root still reaches
// it through the component/memql requirement it already has.
package readiness

import (
	"sort"
	"time"
)

// State is one node's, or the fold's, verdict on one module.
type State string

const (
	Configured    State = "configured"
	Partial       State = "partial"
	Unconfigured  State = "unconfigured"
	NotApplicable State = "notApplicable"
	// Unreported is the fold's word for "no live node reported this module".
	// It is never spelled Unconfigured: not knowing and not being configured
	// are different answers, and the OS draws nothing for this one.
	Unreported State = "unreported"
	// Unknown is a NODE's word for "I could not evaluate this": a resolver the
	// verdict depends on errored (the fleet read, an integration probe). It is
	// never a vote. The fold sets it aside and names the node in
	// Verdict.Unknown, and the writer never persists it over a known row
	// (component/memql/readiness_write.go). The distinction is the one the OS
	// already keeps for passkeys -- "a failed read is unknown, never none" --
	// and it is the one the 2026-09-13 rollout lacked: a read that broke was
	// written as `unconfigured`, folded against other nodes' correct rows, and
	// read "Partly set up" on every surface until the next deploy.
	Unknown State = "unknown"
)

// Reasons a node answers Unknown. A CLOSED vocabulary: rows are broadcast to
// every signed-in reader, so an error string -- which can carry an address, a
// role name or a DSN fragment -- never rides on one. The evaluator's log line
// carries the error; the row carries only WHICH resolver could not answer.
const (
	ReasonFleetReadFailed        = "fleetReadFailed"
	ReasonIntegrationProbeFailed = "integrationProbeFailed"
)

// LaneScopeCluster marks a lane whose completeness is computed from CLUSTER
// ROWS alone -- today the two fleet doors of the inference module, read from
// v1:worker:registration. Every node that evaluates such a lane at the same
// moment answers the same thing, which is what lets the fold tell a node that
// has not caught up from a node that genuinely differs (see Fold, rule 4).
//
// An unmarked lane is NODE-scoped: its answer is this node's own environment,
// registries or plug-ins, and two nodes may honestly disagree about it. The
// federation lane is one of those even though it sits inside the same module:
// it reads the node's in-process provider registry, and a replica deployed
// without the federation configuration answers differently from one with it.
const LaneScopeCluster = "cluster"

// SlotReport is one registry entry's presence and source. Never a value.
type SlotReport struct {
	Name     string `json:"name"`
	Present  bool   `json:"present"`
	Source   string `json:"source"`
	Optional bool   `json:"optional,omitempty"`
}

// LaneReport is one lane's completeness on one node.
type LaneReport struct {
	Name             string `json:"name"`
	ConfigurableFrom string `json:"configurableFrom"`
	// Scope is LaneScopeCluster for a lane computed from cluster rows alone,
	// and empty for a node-scoped lane. Omitted from the wire when empty, so
	// every lane written before the field existed reads as node-scoped --
	// the old behaviour, which is the safe direction: an unmarked lane can
	// never set a row aside.
	Scope    string       `json:"scope,omitempty"`
	Complete bool         `json:"complete"`
	Slots    []SlotReport `json:"slots"`
}

// NodeReport is one row of v1:platform:moduleReadiness.
type NodeReport struct {
	Module   string `json:"module"`
	NodeId   string `json:"nodeId"`
	NodeType string `json:"nodeType"`
	State    State  `json:"state"`
	// Reason is set only when State is Unknown, from the Reason* vocabulary.
	Reason     string       `json:"reason,omitempty"`
	Core       bool         `json:"core"`
	Lanes      []LaneReport `json:"lanes"`
	ReportedAt time.Time    `json:"reportedAt"`
}

// NodeLiveness is what the fold needs from a v1:cluster:node row.
type NodeLiveness struct {
	NodeId   string    `json:"nodeId"`
	Health   string    `json:"health"`
	LastSeen time.Time `json:"lastSeen"`
}

// NodeVerdict is one live reporter's contribution to a Verdict.
type NodeVerdict struct {
	NodeId     string    `json:"nodeId"`
	NodeType   string    `json:"nodeType"`
	State      State     `json:"state"`
	ReportedAt time.Time `json:"reportedAt"`
}

// Verdict is the cluster-wide answer for one module.
type Verdict struct {
	Module       string        `json:"module"`
	State        State         `json:"state"`
	Core         bool          `json:"core"`
	Disagreement []string      `json:"disagreement"`
	Nodes        []NodeVerdict `json:"nodes"`
	// Unknown names every LIVE node whose row says it could not evaluate,
	// sorted. Those rows are not votes: they are neither in Nodes nor in
	// Disagreement. Never nil on the wire -- an empty list, so a reader that
	// iterates it needs no null check.
	Unknown []string `json:"unknown"`
	// Stale names every LIVE node whose row reads the cluster at an earlier
	// moment than the freshest evaluation does -- its cluster-scoped lanes
	// disagree with the newest row's -- sorted. Set aside rather than folded:
	// a node that has not yet heard about a machine being paired is not a
	// node reporting "not set up" (memql#5259). Never nil on the wire.
	Stale []string `json:"stale"`
}

// NodeLiveWindow is how recently a cluster node must have been seen for its
// report to count. Three times the reconciler's 20s grace, so a slow
// heartbeat does not flicker a verdict. MIRRORED as NODE_LIVE_WINDOW_SECONDS
// in clients/os/src/system/readinessFold.ts and pinned by
// TestNodeLiveWindowMatchesTheClient in this package.
const NodeLiveWindow = 60 * time.Second

var liveHealth = map[string]bool{
	"healthy":    true,
	"connecting": true,
	"degraded":   true,
	"draining":   true,
}

// NodeIsLive reports whether a node's report may count: a live health word
// and a heartbeat inside the window. A zero LastSeen is never live.
func NodeIsLive(n NodeLiveness, now time.Time) bool {
	if !liveHealth[n.Health] || n.LastSeen.IsZero() {
		return false
	}
	return now.Sub(n.LastSeen) <= NodeLiveWindow
}

func rank(s State) int {
	switch s {
	case Unconfigured:
		return 2
	case Partial:
		return 1
	default:
		return 0
	}
}

// Fold turns every node's report into one verdict per module.
//
//  1. Keep reports from live nodes only, so a dead replica's stale row cannot
//     pin a verdict.
//  2. Drop notApplicable.
//  3. Set aside unknown: a node that could not evaluate is named in
//     Verdict.Unknown and casts no vote.
//  4. Set aside a row whose CLUSTER-SCOPED lanes disagree with the freshest
//     row's: it read the cluster before something changed, and it is named
//     in Verdict.Stale rather than folded (see setAsideStale).
//  5. Worst state wins.
//  6. If the kept reports disagree, the verdict is partial and Disagreement
//     names every kept reporter as nodeId=state, worst state first.
//  7. A module with no kept report is unreported -- including a module whose
//     every live row is unknown, which is the honest answer and the one that
//     lets the core gate open rather than hold on a read that broke.
//
// Modules appear in name order, so two folds over the same rows are equal.
func Fold(reports []NodeReport, nodes []NodeLiveness, now time.Time) []Verdict {
	live := map[string]bool{}
	for _, n := range nodes {
		if NodeIsLive(n, now) {
			live[n.NodeId] = true
		}
	}
	kept := map[string][]NodeReport{}
	unknown := map[string][]string{}
	core := map[string]bool{}
	seen := map[string]bool{}
	var order []string
	for _, r := range reports {
		if !seen[r.Module] {
			seen[r.Module] = true
			order = append(order, r.Module)
		}
		core[r.Module] = core[r.Module] || r.Core
		if !live[r.NodeId] || r.State == NotApplicable {
			continue
		}
		if r.State == Unknown {
			unknown[r.Module] = append(unknown[r.Module], r.NodeId)
			continue
		}
		kept[r.Module] = append(kept[r.Module], r)
	}
	sort.Strings(order)
	out := make([]Verdict, 0, len(order))
	for _, module := range order {
		v := Verdict{Module: module, Core: core[module], Disagreement: []string{}, Nodes: []NodeVerdict{}, Unknown: []string{}, Stale: []string{}}
		if ids := unknown[module]; len(ids) > 0 {
			sort.Strings(ids)
			v.Unknown = ids
		}
		rs, stale := setAsideStale(kept[module])
		if len(stale) > 0 {
			v.Stale = stale
		}
		if len(rs) == 0 {
			v.State = Unreported
			out = append(out, v)
			continue
		}
		sort.Slice(rs, func(i, j int) bool {
			if rank(rs[i].State) != rank(rs[j].State) {
				return rank(rs[i].State) > rank(rs[j].State)
			}
			return rs[i].NodeId < rs[j].NodeId
		})
		states := map[State]bool{}
		for _, r := range rs {
			states[r.State] = true
			v.Nodes = append(v.Nodes, NodeVerdict{NodeId: r.NodeId, NodeType: r.NodeType, State: r.State, ReportedAt: r.ReportedAt})
		}
		if len(states) > 1 {
			v.State = Partial
			for _, r := range rs {
				v.Disagreement = append(v.Disagreement, r.NodeId+"="+string(r.State))
			}
		} else {
			v.State = rs[0].State
		}
		out = append(out, v)
	}
	return out
}

// setAsideStale splits one module's kept reports into the ones that vote and
// the ids of the nodes whose rows read the cluster at an earlier moment.
//
// ===========================================================================
// WHY RECENCY DECIDES, AND ONLY FOR CLUSTER-SCOPED LANES (memql#5259)
// ===========================================================================
// A cluster-scoped lane is a function of cluster rows and nothing else, so
// two evaluations of it can differ for exactly one reason: they read the rows
// at different moments. After a machine is paired, the node that wrote the
// registration hears its own event and re-evaluates within the debounce, but
// the mesh does not carry that event everywhere -- identity is excluded from
// every broadcast, and a node nobody dials (the edge, a product bff) receives
// no mesh event at all -- so on 2026-09-09 seven of fourteen `ai` rows still
// said `unconfigured` from boot while seven said `configured`, and the fold
// read the disagreement as "Partly set up". None of the seven was a node
// reporting a missing setup; each was a node that had not caught up.
//
// So the freshest evaluation is the reference, and a row whose cluster-scoped
// lanes disagree with it is SET ASIDE and named in Verdict.Stale -- neither a
// vote nor a disagreement. It is not deleted and it is not rewritten here: the
// node's own recompute loop restates it on its next pass (the safety net
// guarantees one within a bounded period), and until then a surface can say
// which node is behind and since when.
//
// A NODE-scoped lane never sets a row aside, which is the half that keeps this
// honest. The inference module's federation lane is node-scoped: a replica
// deployed without the federation configuration answers "no federated door"
// while its siblings answer "configured", and that difference is REAL -- the
// router on that replica cannot use the door either -- so it still folds to
// `partial` with both nodes named. Recency is only ever asked a question it
// can answer.
//
// ===========================================================================
// A TIE IS NOT A REFERENCE
// ===========================================================================
// Rows carry their evaluation time to the second. When the freshest moment
// holds reports that disagree about the cluster lanes, recency cannot say
// which of them is right, and nothing is set aside: the disagreement stands
// and folds to `partial`, which is what the rows as written support. A rule
// that picked a winner by node id would make the verdict depend on how nodes
// happen to be named.
func setAsideStale(rs []NodeReport) ([]NodeReport, []string) {
	var freshest time.Time
	found := false
	for _, r := range rs {
		if _, ok := clusterFacts(r); !ok {
			continue
		}
		if !found || r.ReportedAt.After(freshest) {
			freshest, found = r.ReportedAt, true
		}
	}
	if !found {
		return rs, nil
	}
	var ref map[string]bool
	for _, r := range rs {
		facts, ok := clusterFacts(r)
		if !ok || !r.ReportedAt.Equal(freshest) {
			continue
		}
		if ref == nil {
			ref = facts
			continue
		}
		if !sameClusterFacts(ref, facts) {
			return rs, nil
		}
	}
	voters := make([]NodeReport, 0, len(rs))
	var stale []string
	for _, r := range rs {
		if facts, ok := clusterFacts(r); ok && !sameClusterFacts(ref, facts) {
			stale = append(stale, r.NodeId)
			continue
		}
		voters = append(voters, r)
	}
	sort.Strings(stale)
	return voters, stale
}

// clusterFacts is a report's cluster-scoped lanes as lane name -> complete.
// The second result is false for a report that carries none, which is every
// module but inference and every row written before lanes carried a scope:
// such a report is never judged stale.
func clusterFacts(r NodeReport) (map[string]bool, bool) {
	var facts map[string]bool
	for _, l := range r.Lanes {
		if l.Scope != LaneScopeCluster {
			continue
		}
		if facts == nil {
			facts = map[string]bool{}
		}
		facts[l.Name] = l.Complete
	}
	return facts, facts != nil
}

// sameClusterFacts compares completeness only. A lane's `live` slot is
// presence -- a laptop that slept between two evaluations -- and two rows that
// agree a door is configured but disagree about whether it is awake right now
// are not reading the cluster at different moments in any sense the verdict
// turns on.
func sameClusterFacts(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for name, complete := range a {
		if other, ok := b[name]; !ok || other != complete {
			return false
		}
	}
	return true
}
