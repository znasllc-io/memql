package architecture

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/architecture/model"
)

// model_edge_set_3288_test.go -- memql#3288.
//
// memql#3050 taught the gate to check edge ENDPOINTS, which is a question about
// SYMBOLS. It never asked whether the RELATIONSHIPS themselves survived. Delete
// the only call from A to B, where A and B both still exist and both still have
// other callers and callees, and every endpoint stays live: staleNodes sees an
// unchanged node set, staleEdgeEndpoints sees every endpoint vouched for, and the
// committed model keeps drawing an arrow the code does not contain.
//
// Not hypothetical, and not a fixture-only story. Measured on `main` at f9a2d046
// with the whole suite green: 15 committed `calls` triples were absent from a
// fresh regeneration, ALL 15 with both endpoints alive. Two examples, both from
// the identity refactors that landed in that window:
//
//	(Server).handleToken -calls-> setRefreshCookie
//	deriveGRPCEndpoint   -calls-> isLoopbackHost
//
// Fixtures rather than the live tree, for the reason the #3050 file gives: the
// incident was fixed by regenerating, so there is nothing left in the artifact to
// catch, and reproducing it for real would mean editing tracked source and not
// regenerating. Same pure helpers the gate calls, no extractor run, runs under
// -short.
//
// The pair is always (committed, regenerated): `want` is what is in git, `got` is
// what the extractor produces from the code as it is now.

// pkgNodes is the node set both sides of every fixture share. Nothing about the
// NODES may differ in these tests -- that is the whole point.
func pkgNodes() []model.Node {
	return []model.Node{
		{ID: "pkg:example.com/pkg", Kind: model.KindPackage, Name: "pkg"},
		{ID: "func:example.com/pkg.a", Kind: model.KindFunc, Name: "a"},
		{ID: "func:example.com/pkg.b", Kind: model.KindFunc, Name: "b"},
	}
}

// TestStaleEdgesCatchesADeletedCallBetweenSurvivingSymbols is the reproduction.
//
// Both functions survive and both remain live in the regenerated model, so the
// two pre-#3288 checks are structurally incapable of noticing. This test asserts
// that they stay silent -- if either starts reporting, the fixture has stopped
// reproducing the blind spot and the new check is being credited for a catch it
// did not make.
func TestStaleEdgesCatchesADeletedCallBetweenSurvivingSymbols(t *testing.T) {
	const (
		a = model.ID("func:example.com/pkg.a")
		b = model.ID("func:example.com/pkg.b")
	)
	want := &model.Model{
		Nodes: pkgNodes(),
		Edges: []model.Edge{
			callEdge(a, b), // the call that has since been deleted
			callEdge(b, a), // the reciprocal one, which still exists
		},
	}
	got := &model.Model{
		Nodes: pkgNodes(),
		Edges: []model.Edge{callEdge(b, a)},
	}

	live := liveSymbols(got)
	if _, total := staleNodes(want, live); total != 0 {
		t.Fatalf("staleNodes reported %d stale node(s); this fixture must be invisible to the "+
			"node check or it is not reproducing memql#3288", total)
	}
	if _, total := staleEdgeEndpoints(want, live); total != 0 {
		t.Fatalf("staleEdgeEndpoints reported %d dangling endpoint(s); both endpoints are alive "+
			"here, so a non-zero count means the fixture no longer reproduces memql#3288", total)
	}

	samples, total := staleEdges(want, got)
	if total != 1 {
		t.Fatalf("staleEdges reported %d stale relationship(s), want 1 (%s -calls-> %s)", total, a, b)
	}
	if len(samples) != 1 || !strings.Contains(samples[0], string(a)) ||
		!strings.Contains(samples[0], string(b)) || !strings.Contains(samples[0], "calls") {
		t.Errorf("samples = %v, want one naming both endpoints and the kind", samples)
	}
}

// TestStaleEdgesPassesAfterRegeneration is the other half: `make arch-model` must
// be able to make the gate green, or it is unsatisfiable rather than strict.
func TestStaleEdgesPassesAfterRegeneration(t *testing.T) {
	regenerated := &model.Model{
		Nodes: pkgNodes(),
		Edges: []model.Edge{
			callEdge("func:example.com/pkg.a", "func:example.com/pkg.b"),
			{From: "pkg:example.com/pkg", To: "func:example.com/pkg.a", Kind: model.EdgeContains},
		},
	}
	if _, total := staleEdges(regenerated, regenerated); total != 0 {
		t.Errorf("a freshly regenerated model reports %d stale relationship(s), want 0", total)
	}
}

// TestStaleEdgesKeepsTheAddOnlyAsymmetry is the clause that makes this check
// safe to add at all, and the reason it is a subset test rather than the count
// equality memql#3288 originally proposed.
//
// A relationship the CODE has and the artifact does not is the concurrent-merge
// case: somebody else's PR added a call while yours was open. It must not fail.
// Structurally it cannot -- only `want.Edges` is iterated -- and this pins that.
//
// It is also the mutation the issue used to demonstrate the blind spot (drop one
// edge from the committed file), which is why that mutation stays green: from
// here, "the artifact is missing A->B" and "a merge added A->B" are the same
// observation, and no rule can fail one and pass the other.
func TestStaleEdgesKeepsTheAddOnlyAsymmetry(t *testing.T) {
	const (
		a = model.ID("func:example.com/pkg.a")
		b = model.ID("func:example.com/pkg.b")
	)
	want := &model.Model{Nodes: pkgNodes(), Edges: []model.Edge{callEdge(a, b)}}
	got := &model.Model{
		Nodes: pkgNodes(),
		Edges: []model.Edge{
			callEdge(a, b),
			callEdge(b, a), // added by a merge that landed after the artifact was written
			{From: "func:example.com/pkg.a", To: "func:example.com/pkg.new", Kind: model.EdgeCalls},
		},
	}
	if samples, total := staleEdges(want, got); total != 0 {
		t.Errorf("staleEdges reported %d stale relationship(s) %v on an artifact that is merely "+
			"BEHIND. Nothing may ever fail for being present in the code -- that is what makes "+
			"this gate survivable under concurrent merges (memql#2844).", total, samples)
	}
}

// TestStaleEdgesDistinguishesKindAndDirection stops the check degrading into
// "are these two symbols related somehow".
//
// A -contains-> B is a different fact from A -calls-> B, and A -calls-> B is a
// different fact from B -calls-> A. Keying on the unordered pair, or dropping
// Kind, would silently pass a model that has the arrow pointing the wrong way --
// which is exactly the sort of thing a renderer shows a human.
func TestStaleEdgesDistinguishesKindAndDirection(t *testing.T) {
	const (
		a = model.ID("func:example.com/pkg.a")
		b = model.ID("func:example.com/pkg.b")
	)
	want := &model.Model{
		Nodes: pkgNodes(),
		Edges: []model.Edge{
			callEdge(a, b),
			{From: a, To: b, Kind: model.EdgeEmbeds},
		},
	}
	// Same pair, wrong direction on one and only one kind present.
	got := &model.Model{Nodes: pkgNodes(), Edges: []model.Edge{callEdge(b, a)}}

	samples, total := staleEdges(want, got)
	if total != 2 {
		t.Fatalf("staleEdges reported %d stale relationship(s) %v, want 2 -- the reversed call "+
			"and the missing embeds", total, samples)
	}
}

// TestStaleEdgesIgnoresEdgeAttributes pins the deliberate narrowing.
//
// `calls` edges carry call_sites, which changes when a body gains a second call
// to something it already called. That is not a relationship changing, and
// gating on it is the same mistake as gating on source.line: measured against
// the live tree, keying on Attrs turned 15 real findings into 49, the extra 34
// all being call_sites counts on calls that still happen.
func TestStaleEdgesIgnoresEdgeAttributes(t *testing.T) {
	const (
		a = model.ID("func:example.com/pkg.a")
		b = model.ID("func:example.com/pkg.b")
	)
	want := &model.Model{
		Nodes: pkgNodes(),
		Edges: []model.Edge{{From: a, To: b, Kind: model.EdgeCalls,
			Attrs: map[string]string{"algorithm": "cha", "call_sites": "1"}}},
	}
	got := &model.Model{
		Nodes: pkgNodes(),
		Edges: []model.Edge{{From: a, To: b, Kind: model.EdgeCalls,
			Attrs: map[string]string{"algorithm": "cha", "call_sites": "3"}}},
	}
	if samples, total := staleEdges(want, got); total != 0 {
		t.Errorf("staleEdges reported %d finding(s) %v on a call whose SITE COUNT moved. The "+
			"relationship is the fact worth gating; how many times it occurs in a body is not.",
			total, samples)
	}
}

// TestStaleEdgesCapsSamplesButCountsAll is the reporting contract its two
// siblings already hold: a capped sample list must not become a capped COUNT.
func TestStaleEdgesCapsSamplesButCountsAll(t *testing.T) {
	want := &model.Model{}
	for i := 0; i < 25; i++ {
		want.Edges = append(want.Edges,
			callEdge(model.ID("func:example.com/pkg.a"), model.ID("func:example.com/pkg.gone"+
				string(rune('a'+i)))))
	}
	samples, total := staleEdges(want, &model.Model{})
	if total != 25 {
		t.Errorf("total = %d, want 25 -- the message must report the TRUE count, not the "+
			"sample cap", total)
	}
	if len(samples) != 10 {
		t.Errorf("len(samples) = %d, want 10", len(samples))
	}
}

// TestStaleEdgesIgnoresEmptyEndpoints mirrors the same guard on the endpoint
// check: an edge with a blank end is a malformed row, not a finding, and
// inventing a report for it makes the gate's message untrustworthy.
func TestStaleEdgesIgnoresEmptyEndpoints(t *testing.T) {
	want := &model.Model{Edges: []model.Edge{
		{From: "", To: "func:example.com/pkg.b", Kind: model.EdgeCalls},
		{From: "func:example.com/pkg.a", To: "", Kind: model.EdgeCalls},
	}}
	if samples, total := staleEdges(want, &model.Model{}); total != 0 {
		t.Errorf("staleEdges reported %d finding(s) %v for edges with a blank endpoint", total, samples)
	}
}

// TestDriftCeilingToleratesAMergeButNotAbandonment pins both ends of the only
// assertion in this file that can fail on an artifact containing no lies.
//
// The lower end is the load-bearing one: a handful of symbols behind is the
// normal, deliberate state of the artifact and must stay green. The upper end is
// what memql#3288 asked for -- "behind" was previously bounded by nothing at all.
func TestDriftCeilingToleratesAMergeButNotAbandonment(t *testing.T) {
	// A merge's worth: measured at 2.5% of nodes and 5.8% of edges after three
	// weeks of merges on f9a2d046.
	if driftExceedsCeiling(550, 21699, 7800, 134960) {
		t.Error("the drift ceiling failed on three weeks of ordinary merge drift. It exists to " +
			"catch abandonment; if it fires on the repo's normal state it will be deleted, not " +
			"obeyed.")
	}

	if !driftExceedsCeiling(100, 21699, 60000, 134960) {
		t.Error("the drift ceiling passed an artifact missing 44% of the edge set. Nothing else " +
			"in the suite bounds staleness, so this is the only thing standing between the " +
			"cockpit and a model of a different codebase.")
	}
}
