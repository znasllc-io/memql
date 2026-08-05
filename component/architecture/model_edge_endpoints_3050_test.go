package architecture

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/architecture/model"
)

// model_edge_endpoints_3050_test.go -- memql#3050.
//
// TestArchitectureModelIsNotStale only ever diffed NODES, so a deleted
// unexported function left dangling call edges undetected. The incident: PR
// #3033 (memql#2951) deleted the unexported `memqlTypeToJSONType` from
// component/database/memory-nodes/concept_parser.go, and the committed model
// kept 33 kind:"calls" edges whose `to` was that function. The gate stayed green
// through every CI run.
//
// WHY THESE ARE FIXTURES AND NOT THE LIVE TREE. The incident was fixed by
// regenerating, so `memqlTypeToJSONType` has 0 endpoints in the committed
// artifact today -- there is nothing dangling left to catch. Reproducing it
// against the real tree would mean deleting a function from the source and NOT
// regenerating, which is a ~6s extractor run plus a mutation of tracked source
// that no test may perform. So the incident is reproduced as a two-model pair
// through the same pure helpers the gate calls: same code path, no regeneration,
// runs under -short.
//
// The pair is always (committed, regenerated): `want` is what is in git, `got`
// is what the extractor produces from the code as it is now.

// callEdge is shorthand for the shape the incident took.
func callEdge(from, to model.ID) model.Edge {
	return model.Edge{From: from, To: to, Kind: model.EdgeCalls}
}

// TestStaleEdgeEndpointsCatchesTheDeletedUnexportedFunction is the reproduction.
//
// The regenerated model has NO node for the deleted function and no edge to it,
// because the extractor rebuilt the call graph from source that no longer
// declares it. The committed model still fans 3 calls edges at it. Nothing about
// the NODE sets differs, which is precisely why the old gate passed.
func TestStaleEdgeEndpointsCatchesTheDeletedUnexportedFunction(t *testing.T) {
	const (
		caller  = model.ID("func:example.com/pkg.caller")
		deleted = model.ID("func:example.com/pkg.memqlTypeToJSONType")
	)
	// Identical node sets. The deleted function never had a node -- that is the
	// hole -- so its removal changes nothing here.
	nodes := []model.Node{
		{ID: "pkg:example.com/pkg", Kind: model.KindPackage, Name: "pkg"},
		{ID: caller, Kind: model.KindFunc, Name: "caller"},
	}
	want := &model.Model{
		Nodes: nodes,
		Edges: []model.Edge{
			callEdge(caller, deleted),
			callEdge(caller, deleted),
			callEdge(deleted, caller),
		},
	}
	got := &model.Model{Nodes: nodes}

	// The node check must stay silent: this is what shipped green.
	if _, total := staleNodes(want, liveSymbols(got)); total != 0 {
		t.Fatalf("staleNodes reported %d stale node(s); the whole point of memql#3050 is that "+
			"the node check CANNOT see this, so a non-zero count here means the fixture no "+
			"longer reproduces the incident", total)
	}

	samples, total := staleEdgeEndpoints(want, liveSymbols(got))
	if total != 1 {
		t.Fatalf("staleEdgeEndpoints reported %d dangling symbol(s), want 1 (%s)", total, deleted)
	}
	if len(samples) != 1 || !strings.Contains(samples[0], string(deleted)) {
		t.Fatalf("samples = %v, want one naming %s", samples, deleted)
	}
	// The edge COUNT is in the message because one deletion takes a fan of edges
	// with it, and the fan size is what tells a reader how stale the file is.
	if !strings.Contains(samples[0], "3 ") {
		t.Errorf("samples[0] = %q, want the 3-edge fan reported", samples[0])
	}
}

// TestStaleEdgeEndpointsPassesAfterRegeneration is the other half of the same
// reproduction: `make arch-model` makes it green. Regenerating drops the edges
// to the deleted symbol, so the committed model no longer references it.
func TestStaleEdgeEndpointsPassesAfterRegeneration(t *testing.T) {
	const caller = model.ID("func:example.com/pkg.caller")
	nodes := []model.Node{{ID: caller, Kind: model.KindFunc, Name: "caller"}}

	// Both sides are now the extractor's current output -- which is what
	// committing a regeneration means.
	regenerated := &model.Model{
		Nodes: nodes,
		Edges: []model.Edge{callEdge(caller, "func:example.com/pkg.stillHere")},
	}
	if _, total := staleEdgeEndpoints(regenerated, liveSymbols(regenerated)); total != 0 {
		t.Errorf("a freshly regenerated model reports %d dangling endpoint(s), want 0. "+
			"`make arch-model` must be able to make this gate green, or it is unsatisfiable "+
			"(memql#3050)", total)
	}
}

// TestStaleEdgeEndpointsKeepsTheAddOnlyAsymmetry pins DoD item 2, and it is the
// clause that makes the whole design safe under a merge queue.
//
// An edge to a symbol that EXISTS but is merely newer than the artifact must not
// fail. Concurrent merges only add symbols, and a gate that fails on someone
// else's merge blocks unrelated work -- which is why the byte-for-byte design was
// abandoned in #2844. Structurally: only committed endpoints are iterated, so
// nothing can ever fail for being present.
func TestStaleEdgeEndpointsKeepsTheAddOnlyAsymmetry(t *testing.T) {
	const caller = model.ID("func:example.com/pkg.caller")

	want := &model.Model{
		Nodes: []model.Node{{ID: caller, Kind: model.KindFunc, Name: "caller"}},
		Edges: []model.Edge{callEdge(caller, "func:example.com/pkg.old")},
	}
	// Somebody merged: new nodes, new functions, new call edges. Everything the
	// committed model referenced is still there.
	got := &model.Model{
		Nodes: []model.Node{
			{ID: caller, Kind: model.KindFunc, Name: "caller"},
			{ID: "func:example.com/pkg.brandNew", Kind: model.KindFunc, Name: "brandNew"},
		},
		Edges: []model.Edge{
			callEdge(caller, "func:example.com/pkg.old"),
			callEdge("func:example.com/pkg.brandNew", "func:example.com/pkg.alsoNew"),
		},
	}

	if _, total := staleEdgeEndpoints(want, liveSymbols(got)); total != 0 {
		t.Errorf("staleEdgeEndpoints reported %d dangling endpoint(s) against a model that only "+
			"GREW; a gate that fails on another PR's merge is unwinnable under a merge queue "+
			"(memql#2844, memql#3050)", total)
	}
	if _, total := staleNodes(want, liveSymbols(got)); total != 0 {
		t.Errorf("staleNodes reported %d stale node(s) against a model that only GREW", total)
	}
}

// TestLiveSymbolsVouchesForNodelessEndpoints is the guard on the one mistake
// that would make this gate unusable: building the live set from nodes alone.
//
// Measured on the committed artifact -- 5,006 of 25,194 distinct endpoints have
// no node (4,155 func, 816 method, 33 iface, 2 type). Unexported functions and
// generated closures like `main$1` are endpoints only. Validate endpoints against
// the node set and all 5,006 are reported as dangling on a current model, so the
// gate goes permanently red and gets deleted or skipped.
func TestLiveSymbolsVouchesForNodelessEndpoints(t *testing.T) {
	const (
		caller     = model.ID("func:example.com/pkg.caller")
		unexported = model.ID("func:example.com/pkg.helper") // real, but never a node
	)
	got := &model.Model{
		Nodes: []model.Node{{ID: caller, Kind: model.KindFunc, Name: "caller"}},
		Edges: []model.Edge{callEdge(caller, unexported)},
	}

	live := liveSymbols(got)
	if !live[unexported] {
		t.Fatalf("liveSymbols does not vouch for %s, which the regenerated model references as "+
			"an edge endpoint. Building the live set from nodes alone reports 5,006 endpoints "+
			"in the real artifact as dangling (memql#3050)", unexported)
	}
	if !live[caller] {
		t.Errorf("liveSymbols dropped the node id %s", caller)
	}

	// A committed model referencing the same node-less symbol must therefore pass.
	want := &model.Model{Edges: []model.Edge{callEdge(caller, unexported)}}
	if _, total := staleEdgeEndpoints(want, live); total != 0 {
		t.Errorf("a committed edge to the node-less-but-live %s was reported dangling (%d)",
			unexported, total)
	}
}

// TestStaleEdgeEndpointsReportsEveryKindOfEndpoint checks both directions and
// both ends. A deleted symbol can be the `from` of an edge as easily as the `to`,
// and the incident had edges in both directions.
func TestStaleEdgeEndpointsReportsEveryKindOfEndpoint(t *testing.T) {
	live := map[model.ID]bool{"func:example.com/pkg.alive": true}

	want := &model.Model{Edges: []model.Edge{
		{From: "func:example.com/pkg.deadFrom", To: "func:example.com/pkg.alive", Kind: model.EdgeCalls},
		{From: "func:example.com/pkg.alive", To: "func:example.com/pkg.deadTo", Kind: model.EdgeCalls},
		{From: "type:example.com/pkg.Gone", To: "iface:example.com/pkg.Also", Kind: model.EdgeImplements},
	}}

	samples, total := staleEdgeEndpoints(want, live)
	if total != 4 {
		t.Fatalf("staleEdgeEndpoints total = %d, want 4 (deadFrom, deadTo, Gone, Also); "+
			"got samples %v", total, samples)
	}
	joined := strings.Join(samples, "\n")
	for _, id := range []string{"deadFrom", "deadTo", "Gone", "Also"} {
		if !strings.Contains(joined, id) {
			t.Errorf("samples do not name %s:\n%s", id, joined)
		}
	}
	// The edge kind is reported so a reader knows which renderer is affected --
	// a dangling `calls` edge breaks sequence diagrams, `implements` breaks class
	// diagrams.
	if !strings.Contains(joined, "implements") || !strings.Contains(joined, "calls") {
		t.Errorf("samples do not carry the edge kinds:\n%s", joined)
	}
}

// TestStaleEdgeEndpointsIgnoresEmptyEndpoints stops the gate inventing a finding
// out of an absent optional field. An edge with an empty `from`/`to` is malformed
// rather than stale, and reporting "" as a deleted symbol would be noise that
// `make arch-model` cannot clear.
func TestStaleEdgeEndpointsIgnoresEmptyEndpoints(t *testing.T) {
	want := &model.Model{Edges: []model.Edge{{From: "", To: "", Kind: model.EdgeCalls}}}
	if samples, total := staleEdgeEndpoints(want, map[model.ID]bool{}); total != 0 {
		t.Errorf("staleEdgeEndpoints reported %d finding(s) for an empty endpoint: %v", total, samples)
	}
}

// TestStaleEdgeEndpointsCapsSamplesButCountsAll pins the reporting contract. A
// bad regeneration can dangle thousands of symbols; the message shows 10 and the
// COUNT is the true total. The count used to be the capped slice's length, which
// under-reported the blast radius as "10".
func TestStaleEdgeEndpointsCapsSamplesButCountsAll(t *testing.T) {
	want := &model.Model{}
	for i := 0; i < 25; i++ {
		id := model.ID("func:example.com/pkg.gone" + string(rune('a'+i)))
		want.Edges = append(want.Edges, callEdge("func:example.com/pkg.caller", id))
	}

	samples, total := staleEdgeEndpoints(want, map[model.ID]bool{})
	// 25 dead targets + the caller, which is itself not live in this fixture.
	if total != 26 {
		t.Errorf("total = %d, want 26 -- the count must be every dangling symbol, not the "+
			"length of the truncated sample list", total)
	}
	if len(samples) != 10 {
		t.Errorf("len(samples) = %d, want 10", len(samples))
	}
}

// TestStaleNodesCountsAllNotJustSamples is the same honesty fix on the node path,
// which shared the defect: `gone` was capped at 10 and `len(gone)` was reported
// as the number of stale symbols, so any larger drift read as exactly 10.
func TestStaleNodesCountsAllNotJustSamples(t *testing.T) {
	want := &model.Model{}
	for i := 0; i < 15; i++ {
		want.Nodes = append(want.Nodes, model.Node{
			ID:   model.ID("func:example.com/pkg.gone" + string(rune('a'+i))),
			Kind: model.KindFunc,
		})
	}

	samples, total := staleNodes(want, map[model.ID]bool{})
	if total != 15 {
		t.Errorf("total = %d, want 15", total)
	}
	if len(samples) != 10 {
		t.Errorf("len(samples) = %d, want 10", len(samples))
	}
}
