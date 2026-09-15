package architecture

import (
	"strconv"
	"testing"

	"github.com/znasllc-io/memql/component/architecture/embedded"
	"github.com/znasllc-io/memql/component/architecture/model"
)

func TestCommittedModelIncludesAutomationGraph(t *testing.T) {
	m, err := embedded.Load()
	if err != nil {
		t.Fatal(err)
	}
	nodes := map[model.ID]model.Node{}
	for _, n := range m.Nodes {
		if n.Kind != model.KindAutomation {
			continue
		}
		nodes[n.ID] = n
		if _, err := strconv.Atoi(n.Attrs["stratum"]); err != nil {
			t.Errorf("%s lacks its stratum", n.ID)
		}
		if n.Parent == "" {
			t.Errorf("%s lacks its cluster parent", n.ID)
		}
	}
	if len(nodes) < 40 {
		t.Fatalf("only %d automation nodes: regenerate with --automations", len(nodes))
	}
	if _, ok := nodes[model.AutomationID("routeRequest")]; !ok {
		t.Error("forge routing missing from automation graph")
	}
	edges := 0
	for _, e := range m.Edges {
		if e.Kind != model.EdgeTriggers {
			continue
		}
		edges++
		if _, ok := nodes[e.From]; !ok {
			t.Errorf("missing trigger source %s", e.From)
		}
		if _, ok := nodes[e.To]; !ok {
			t.Errorf("missing trigger target %s", e.To)
		}
		if e.Attrs["reason"] == "" {
			t.Errorf("%s -> %s lacks edge reason", e.From, e.To)
		}
	}
	if edges == 0 {
		t.Fatal("automation graph has no trigger edges")
	}
}
