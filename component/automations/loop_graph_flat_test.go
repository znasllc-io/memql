package automations

// loop_graph_flat_test.go -- FlattenLoopGraph records exactly what
// LoopGraphRows carries, as strings (memql#5727). Same DB-free fixture as
// loop_graph_rows_test.go, so a row fact and its flattened form are compared
// on one graph.

import (
	"strings"
	"testing"
)

func flatNamed(t *testing.T, autos []FlatAutomation, name string) FlatAutomation {
	t.Helper()
	for _, a := range autos {
		if a.Name == name {
			return a
		}
	}
	t.Fatalf("no flattened automation %q", name)
	return FlatAutomation{}
}

func TestFlattenLoopGraph_FollowsTheRows(t *testing.T) {
	g := rowsFixture(t)
	rows := LoopGraphRows(g)
	autos, triggers := FlattenLoopGraph(g)

	if len(autos) != len(rows) {
		t.Fatalf("%d flattened automations for %d rows", len(autos), len(rows))
	}
	for i, r := range rows {
		if autos[i].Name != r["name"] {
			t.Fatalf("automation %d = %q, row %d = %v: the flattened order is the rows' order", i, autos[i].Name, i, r["name"])
		}
	}

	a := flatNamed(t, autos, "a")
	want := map[string]string{
		"stratum":        "1",
		"trigger":        "graph.node.created.v1:t:thing",
		"triggerKind":    "node.created",
		"triggerConcept": "v1:t:thing",
		"filter":         `row => row.status != "done"`,
		"writes":         thingConcept,
		"loop":           `maxDepth=4 until=row => row.status == "done"`,
		"mode":           "queued max=3",
		"cycle":          "a",
		"cyclePermitted": "true",
	}
	for k, v := range want {
		if a.Attrs[k] != v {
			t.Errorf("a.%s = %q, want %q", k, a.Attrs[k], v)
		}
	}
	if len(a.Writes) != 1 || a.Writes[0] != thingConcept {
		t.Errorf("a.Writes = %v", a.Writes)
	}

	b := flatNamed(t, autos, "b")
	if b.Attrs["schedule"] != "0 */5 * * * *" || b.Attrs["stratum"] != "0" {
		t.Errorf("b attrs = %v", b.Attrs)
	}
	for _, absent := range []string{"trigger", "filter", "loop", "mode", "cycle", "problem"} {
		if _, ok := b.Attrs[absent]; ok {
			t.Errorf("b carries %q; a key with no value is absent, never empty", absent)
		}
	}

	var got []string
	for _, e := range triggers {
		got = append(got, e.From+"->"+e.To)
		if e.Attrs["reason"] == "" && e.Attrs["decided"] == "" {
			t.Errorf("edge %s -> %s lost its row attributes: %v", e.From, e.To, e.Attrs)
		}
	}
	var fromRows []string
	for _, r := range rows {
		for _, e := range r["edgesOut"].([]map[string]any) {
			fromRows = append(fromRows, r["name"].(string)+"->"+e["to"].(string))
		}
	}
	if strings.Join(got, ",") != strings.Join(fromRows, ",") {
		t.Errorf("triggers = %v, rows' edgesOut = %v", got, fromRows)
	}
}

func TestFlattenLoopGraph_NilGraph(t *testing.T) {
	autos, triggers := FlattenLoopGraph(nil)
	if len(autos) != 0 || len(triggers) != 0 {
		t.Fatalf("nil graph flattened to %d automations and %d triggers", len(autos), len(triggers))
	}
}
