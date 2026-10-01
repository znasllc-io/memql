package architecture

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/architecture/model"
)

// model_call_graph_5727_test.go -- the call graph left the committed artifact
// (memql#5727, owner decision 7) and the drift gate builds it on demand. The
// pure halves of that, on fixtures, under -short.

func TestCallGraphOnDemandSkipsOnlyInTheGateInputsLane(t *testing.T) {
	t.Setenv("RUN_GATES", "true")
	if on, _ := callGraphOnDemand(); on {
		t.Error("the call graph was built in the gate-inputs lane (RUN_GATES=true), whose docs-only PR cannot reach it")
	}
	for _, v := range []string{"", "false"} {
		t.Setenv("RUN_GATES", v)
		if on, _ := callGraphOnDemand(); !on {
			t.Errorf("RUN_GATES=%q skipped the call graph; everywhere the full suite runs must build it", v)
		}
	}
}

func TestStructuralOnlyDropsExactlyTheCallEdges(t *testing.T) {
	m := &model.Model{
		Nodes: []model.Node{{ID: "pkg:example.com/p", Kind: model.KindPackage}},
		Edges: []model.Edge{
			{From: "pkg:example.com/p", To: "pkg:example.com/q", Kind: model.EdgeImports},
			{From: "func:example.com/p.A", To: "func:example.com/p.b", Kind: model.EdgeCalls},
			{From: "type:example.com/p.T", To: "iface:example.com/p.I", Kind: model.EdgeImplements},
		},
	}
	s := structuralOnly(m)
	if len(s.Nodes) != 1 || len(s.Edges) != 2 {
		t.Fatalf("structuralOnly = %d nodes, %d edges; want 1 and 2", len(s.Nodes), len(s.Edges))
	}
	for _, e := range s.Edges {
		if e.Kind == model.EdgeCalls {
			t.Error("a calls edge survived")
		}
	}
	if len(m.Edges) != 3 {
		t.Error("structuralOnly modified its argument")
	}
}

// The consistency check fires on a call-graph symbol the structural pass should
// have modelled -- an exported function, a method of a modelled type -- and on
// nothing that is node-less by design.
func TestCallGraphIsConsistentNamesOnlyWhatTheStructuralPassShouldHave(t *testing.T) {
	m := &model.Model{
		Nodes: []model.Node{
			{ID: "func:example.com/p.Known", Kind: model.KindFunc},
			{ID: model.TypeID("example.com/p", "Server"), Kind: model.KindType},
		},
		Edges: []model.Edge{
			// Node-less by design: unexported, a closure, a method of a type the
			// types pass does not model (a named string, say).
			{From: "func:example.com/p.Known", To: "func:example.com/p.helper", Kind: model.EdgeCalls},
			{From: "func:example.com/p.Known$1", To: "method:example.com/p.(Kind).String", Kind: model.EdgeCalls},
			// Should have been modelled.
			{From: "func:example.com/p.Known", To: "func:example.com/p.Missing", Kind: model.EdgeCalls},
			{From: "func:example.com/p.Known", To: "method:example.com/p.(Server).start", Kind: model.EdgeCalls},
			// Not a calls edge, so not the call graph's claim.
			{From: "func:example.com/p.Known", To: "func:example.com/p.Other", Kind: model.EdgeImports},
		},
	}
	samples, total := callGraphIsConsistent(m)
	if total != 2 {
		t.Fatalf("callGraphIsConsistent reported %d (%v), want the exported function and the modelled type's method", total, samples)
	}
	joined := strings.Join(samples, " ")
	if !strings.Contains(joined, "p.Missing") || !strings.Contains(joined, "(Server).start") {
		t.Errorf("samples = %v", samples)
	}
}
