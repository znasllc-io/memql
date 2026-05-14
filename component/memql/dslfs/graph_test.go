package dslfs

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// TestImportGraph_EmptyGraph locks the trivial case.
func TestImportGraph_EmptyGraph(t *testing.T) {
	g := NewImportGraph()
	if cycle := g.DetectCycle(); cycle != nil {
		t.Errorf("empty graph should not report a cycle, got %v", cycle)
	}
	order, err := g.Topo()
	if err != nil {
		t.Fatalf("empty graph Topo: %v", err)
	}
	if len(order) != 0 {
		t.Errorf("empty graph Topo should return empty list, got %v", order)
	}
}

// TestImportGraph_SingleNodeNoEdges locks the leaf-only case.
func TestImportGraph_SingleNodeNoEdges(t *testing.T) {
	g := NewImportGraph()
	g.AddNode("a.memql")
	order, err := g.Topo()
	if err != nil {
		t.Fatalf("Topo: %v", err)
	}
	if !reflect.DeepEqual(order, []string{"a.memql"}) {
		t.Errorf("Topo = %v, want [a.memql]", order)
	}
}

// TestImportGraph_LinearChain locks the simple chain ordering.
//
//	a imports b imports c
//
// Expected emit order: c, b, a (depends-first).
func TestImportGraph_LinearChain(t *testing.T) {
	g := NewImportGraph()
	g.AddEdge("a.memql", "b.memql")
	g.AddEdge("b.memql", "c.memql")
	order, err := g.Topo()
	if err != nil {
		t.Fatalf("Topo: %v", err)
	}
	want := []string{"c.memql", "b.memql", "a.memql"}
	if !reflect.DeepEqual(order, want) {
		t.Errorf("Topo = %v, want %v", order, want)
	}
}

// TestImportGraph_StableOrdering locks the alphabetical-by-default
// rule for breaking ties.
func TestImportGraph_StableOrdering(t *testing.T) {
	g := NewImportGraph()
	// Three leaves, no order constraints between them.
	g.AddNode("z.memql")
	g.AddNode("a.memql")
	g.AddNode("m.memql")
	order, err := g.Topo()
	if err != nil {
		t.Fatalf("Topo: %v", err)
	}
	want := []string{"a.memql", "m.memql", "z.memql"}
	if !reflect.DeepEqual(order, want) {
		t.Errorf("Topo = %v, want %v (alphabetical)", order, want)
	}
}

// TestImportGraph_Diamond locks the diamond-DAG ordering.
//
//	    a
//	   / \
//	  b   c
//	   \ /
//	    d
//
// Expected: d, b, c, a (alphabetical tie-break between b and c).
func TestImportGraph_Diamond(t *testing.T) {
	g := NewImportGraph()
	g.AddEdge("a.memql", "b.memql")
	g.AddEdge("a.memql", "c.memql")
	g.AddEdge("b.memql", "d.memql")
	g.AddEdge("c.memql", "d.memql")
	order, err := g.Topo()
	if err != nil {
		t.Fatalf("Topo: %v", err)
	}
	want := []string{"d.memql", "b.memql", "c.memql", "a.memql"}
	if !reflect.DeepEqual(order, want) {
		t.Errorf("Topo = %v, want %v", order, want)
	}
}

// TestImportGraph_DirectCycle locks the smallest cycle detection.
func TestImportGraph_DirectCycle(t *testing.T) {
	g := NewImportGraph()
	g.AddEdge("a.memql", "b.memql")
	g.AddEdge("b.memql", "a.memql")
	_, err := g.Topo()
	if err == nil {
		t.Fatal("expected cycle error, got nil")
	}
	var ce *CycleError
	if !errors.As(err, &ce) {
		t.Fatalf("expected *CycleError, got %T (%v)", err, err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "a.memql") || !strings.Contains(msg, "b.memql") {
		t.Errorf("cycle error %q should mention both nodes", msg)
	}
}

// TestImportGraph_IndirectCycle locks a 3-node cycle.
//
//	a -> b -> c -> a
func TestImportGraph_IndirectCycle(t *testing.T) {
	g := NewImportGraph()
	g.AddEdge("a.memql", "b.memql")
	g.AddEdge("b.memql", "c.memql")
	g.AddEdge("c.memql", "a.memql")
	cycle := g.DetectCycle()
	if cycle == nil {
		t.Fatal("expected cycle, got nil")
	}
	if len(cycle) < 3 {
		t.Errorf("cycle path %v should have at least 3 nodes", cycle)
	}
}

// TestImportGraph_SelfLoop locks the smallest possible cycle: A
// imports itself. Author error, parser/loader should not let this
// through, but the graph still catches it as the safety net.
func TestImportGraph_SelfLoop(t *testing.T) {
	g := NewImportGraph()
	g.AddEdge("a.memql", "a.memql")
	cycle := g.DetectCycle()
	if cycle == nil {
		t.Fatal("expected cycle for self-loop, got nil")
	}
}

// TestImportGraph_AcyclicDisconnected locks a graph with two
// disconnected subgraphs. Both should emit; no false-positive cycle.
func TestImportGraph_AcyclicDisconnected(t *testing.T) {
	g := NewImportGraph()
	g.AddEdge("a.memql", "b.memql")
	g.AddEdge("x.memql", "y.memql")
	order, err := g.Topo()
	if err != nil {
		t.Fatalf("Topo: %v", err)
	}
	if len(order) != 4 {
		t.Errorf("Topo emitted %d nodes, want 4", len(order))
	}
}

// TestImportGraph_AddEdgeIdempotent locks AddEdge being safe to call
// multiple times for the same pair.
func TestImportGraph_AddEdgeIdempotent(t *testing.T) {
	g := NewImportGraph()
	g.AddEdge("a.memql", "b.memql")
	g.AddEdge("a.memql", "b.memql")
	g.AddEdge("a.memql", "b.memql")
	out := g.OutEdges("a.memql")
	if !reflect.DeepEqual(out, []string{"b.memql"}) {
		t.Errorf("OutEdges = %v, want single entry", out)
	}
}
