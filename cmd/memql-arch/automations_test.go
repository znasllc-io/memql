package main

import (
	"testing"

	"github.com/znasllc-io/memql/component/architecture/model"
	"github.com/znasllc-io/memql/component/automations"
)

func TestAppendAutomationGraph(t *testing.T) {
	m := &model.Model{Nodes: []model.Node{{ID: model.ClusterID("memql"), Kind: model.KindCluster}}}
	g := &automations.LoopGraph{
		Automations: []automations.GraphAutomation{
			{Name: "first", Origin: "unified:demo/automations.memql:first", Stratum: 0, Mode: &automations.ModeConfig{Kind: "queued", Max: 3}},
			{Name: "second", Stratum: 1, Loop: &automations.LoopConfig{MaxDepth: 4, Until: "row => row.done"}},
		},
		Edges: []automations.GraphEdge{{From: "first", To: "second", Topic: "demo", Decided: false, Reason: "unknown filter"}},
	}
	if err := appendAutomationGraph(m, g); err != nil {
		t.Fatal(err)
	}
	if len(m.Nodes) != 3 || len(m.Edges) != 3 {
		t.Fatalf("graph not preserved: %+v", m)
	}
	first := m.Nodes[1]
	if first.ID != model.AutomationID("first") || first.Parent != m.Nodes[0].ID || first.Attrs["mode"] != "queued max=3" || first.Source.File != "dsl/demo/automations.memql" {
		t.Fatalf("first node: %+v", first)
	}
	e := m.Edges[2]
	if e.Kind != model.EdgeTriggers || e.From != first.ID || e.Attrs["decided"] != "false" || e.Attrs["reason"] != "unknown filter" {
		t.Fatalf("edge: %+v", e)
	}
	if err := appendAutomationGraph(&model.Model{}, g); err == nil {
		t.Fatal("missing cluster accepted")
	}
}
