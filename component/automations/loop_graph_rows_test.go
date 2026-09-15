package automations

// loop_graph_rows_test.go -- the static graph as one row per automation, the
// shape the automationGraph builtin serves and the OS's Cluster > Automations
// section draws (memql#5384), and the scheduler's cached build of it.
// DB-free: the call graph is thingReg behind fakeSource, as loop_graph_test.go
// builds it.

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// rowsFixture is three automations and every state a row can carry: b, on a
// schedule, creates a thing; a reacts to that and advances the thing again,
// closing a self-cycle its @loop covers, under a @mode; c reacts to nothing
// anything here writes and calls a builtin the graph cannot see into.
func rowsFixture(t *testing.T) *LoopGraph {
	t.Helper()
	as := graphAutomations(t,
		`@trigger(event="node.created", concept="v1:t:thing")
@filter(row => row.status != "done")
@loop(maxDepth=4, until=row => row.status == "done")
@mode(queued, max=3)
automation a {
  args {
    id any
  }
  step advance {
    mutation advanceThing(id: id, s: "open")
  }
}`,
		`@trigger(schedule="0 */5 * * * *")
automation b {
  step create {
    mutation createThing()
  }
}`,
		`@trigger(event="node.created", concept="v1:t:other")
automation c {
  step send {
    builtin sendThing()
  }
}`)
	return BuildLoopGraph(as, fakeSource{thingReg()}, 0)
}

func rowNamed(t *testing.T, rows []map[string]any, name string) map[string]any {
	t.Helper()
	for _, r := range rows {
		if r["name"] == name {
			return r
		}
	}
	t.Fatalf("no row named %q in %d rows", name, len(rows))
	return nil
}

func TestLoopGraphRows_OneRowPerAutomationInNameOrder(t *testing.T) {
	rows := LoopGraphRows(rowsFixture(t))
	var names []string
	for _, r := range rows {
		names = append(names, r["name"].(string))
	}
	if strings.Join(names, ",") != "a,b,c" {
		t.Fatalf("rows = %v, want a,b,c: the builtin's reply and the OS layout both read this order", names)
	}
}

func TestLoopGraphRows_AnAutomationOnTheMap(t *testing.T) {
	a := rowNamed(t, LoopGraphRows(rowsFixture(t)), "a")

	// What fires it, as the pair a sentence is built from rather than only
	// the folded topic the scheduler subscribes to.
	if a["trigger"] != "graph.node.created.v1:t:thing" || a["triggerKind"] != "node.created" || a["triggerConcept"] != "v1:t:thing" {
		t.Errorf("trigger = %v / %v / %v", a["trigger"], a["triggerKind"], a["triggerConcept"])
	}
	if a["triggerFilter"] != `row => row.status != "done"` {
		t.Errorf("triggerFilter = %v", a["triggerFilter"])
	}
	if a["stratum"] != 1 {
		t.Errorf("stratum = %v, want 1: b's write starts it, so it reacts to stratum 0", a["stratum"])
	}
	if !reflect.DeepEqual(a["writes"], []string{thingConcept}) {
		t.Errorf("writes = %v", a["writes"])
	}
	if !reflect.DeepEqual(a["loop"], map[string]any{"maxDepth": 4, "until": `row => row.status == "done"`}) {
		t.Errorf("loop = %v", a["loop"])
	}
	if !reflect.DeepEqual(a["mode"], map[string]any{"kind": "queued", "max": 3}) {
		t.Errorf("mode = %v", a["mode"])
	}

	edges, _ := a["edgesOut"].([]map[string]any)
	if len(edges) != 1 {
		t.Fatalf("edgesOut = %v, want the self-edge", a["edgesOut"])
	}
	e := edges[0]
	if e["to"] != "a" || e["concept"] != thingConcept || e["topic"] != "graph.node.created.v1:t:thing" || e["decided"] != true {
		t.Errorf("edge = %v", e)
	}
	if !reflect.DeepEqual(e["via"], []string{"advanceThing"}) {
		t.Errorf("via = %v, want [advanceThing]: the detail names the call that reaches the write", e["via"])
	}
	if reason, _ := e["reason"].(string); reason == "" {
		t.Error("the edge carries no reason; the detail and the dashed edge's title have nothing to say")
	}

	cycle, _ := a["cycle"].(map[string]any)
	if cycle == nil {
		t.Fatal("a closes a cycle and its row carries none")
	}
	if cycle["permitted"] != true || !reflect.DeepEqual(cycle["members"], []string{"a"}) || !reflect.DeepEqual(cycle["permittedBy"], []string{"a"}) {
		t.Errorf("cycle = %v, want a permitted cycle {a} permitted by a", cycle)
	}
}

func TestLoopGraphRows_ASourceAndAStandAlone(t *testing.T) {
	rows := LoopGraphRows(rowsFixture(t))

	b := rowNamed(t, rows, "b")
	if b["stratum"] != 0 || b["schedule"] != "0 */5 * * * *" {
		t.Errorf("b = stratum %v, schedule %v", b["stratum"], b["schedule"])
	}
	// ABSENT, not empty: a scheduled automation has no trigger topic, and a
	// key holding "" would read as a trigger nobody can describe.
	for _, key := range []string{"trigger", "triggerKind", "triggerConcept", "triggerFilter", "loop", "mode", "cycle"} {
		if _, present := b[key]; present {
			t.Errorf("b carries %q = %v; a key it has no value for must be absent", key, b[key])
		}
	}
	if edges, _ := b["edgesOut"].([]map[string]any); len(edges) != 1 || edges[0]["to"] != "a" {
		t.Errorf("b's edges = %v, want one into a", b["edgesOut"])
	}

	c := rowNamed(t, rows, "c")
	if !reflect.DeepEqual(c["opaque"], []string{"builtin sendThing"}) {
		t.Errorf("c opaque = %v, want the builtin the map cannot see into", c["opaque"])
	}
	// THE LISTS ARE NEVER NULL. A client reading `.length` must not have to
	// guard for absence on the automations that write nothing, which are
	// most of them.
	for _, key := range []string{"writes", "publishes", "edgesOut"} {
		raw, err := json.Marshal(c[key])
		if err != nil || string(raw) != "[]" {
			t.Errorf("c[%q] marshals as %s, want []", key, raw)
		}
	}
}

// A cycle no @loop covers is on the row too, marked not permitted: the check
// is report-only until the tree's cycles are fixed, so such a graph loads, and
// the page must say which cycles would be refused rather than draw them as if
// they were permitted.
func TestLoopGraphRows_AnUncoveredCycleIsMarkedNotPermitted(t *testing.T) {
	as := graphAutomations(t, `@trigger(event="node.created", concept="v1:t:thing")
automation a {
  args {
    id any
  }
  step advance {
    mutation advanceThing(id: id, s: "open")
  }
}`)
	a := rowNamed(t, LoopGraphRows(BuildLoopGraph(as, fakeSource{thingReg()}, 0)), "a")
	cycle, _ := a["cycle"].(map[string]any)
	if cycle == nil || cycle["permitted"] != false {
		t.Fatalf("cycle = %v, want one marked not permitted", a["cycle"])
	}
	if raw, _ := json.Marshal(cycle["permittedBy"]); string(raw) != "[]" {
		t.Errorf("permittedBy = %s, want []", raw)
	}
	if problem, _ := a["problem"].(string); !strings.HasSuffix(problem, "[loop_cycle]") {
		t.Errorf("problem = %q, want the loop_cycle refusal the load would make", problem)
	}
}

func TestLoopGraphRows_TheFileNotTheLoadersOrigin(t *testing.T) {
	as := graphAutomations(t, `@trigger(schedule="0 0 2 * * *")
automation nightly {
  step send {
    builtin sendThing()
  }
}`)
	as[0].Origin = "unified:forge/automations.memql:nightly"
	row := rowNamed(t, LoopGraphRows(BuildLoopGraph(as, fakeSource{thingReg()}, 0)), "nightly")
	if row["origin"] != "forge/automations.memql" {
		t.Errorf("origin = %v, want the file: the loader's prefix and the name suffix are not a place a person can open", row["origin"])
	}
}

// ---------------------------------------------------------------------------
// The scheduler's rows: built from what it registered, once per set
// ---------------------------------------------------------------------------

func registeredScheduler(t *testing.T, as ...*Automation) *Scheduler {
	t.Helper()
	ready := make(chan struct{})
	close(ready)
	s := &Scheduler{
		loader:      NewLoader(LoaderOptions{Functions: newTestFunctionRegistry()}),
		automations: map[string]*Automation{},
		readyCh:     ready,
	}
	for _, a := range as {
		s.automations[a.Name] = a
	}
	return s
}

func TestAutomationGraphRows_RefusedBeforeTheSchedulerRegistered(t *testing.T) {
	s := registeredScheduler(t, graphAutomations(t, `@trigger(schedule="0 0 2 * * *")
automation nightly {
  step send {
    builtin sendThing()
  }
}`)...)
	s.readyCh = make(chan struct{})
	rows, err := s.AutomationGraphRows()
	// An empty graph here would read "this cluster loaded no automations"
	// during every boot, which is a claim about the cluster rather than
	// about how far through starting this node is.
	if err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("rows = %v, err = %v; want a refusal saying the automations are not registered yet", rows, err)
	}

	close(s.readyCh)
	if _, err := s.AutomationGraphRows(); err != nil {
		t.Fatalf("once registered: %v", err)
	}
}

func TestAutomationGraphRows_RefusedWithoutAFunctionRegistry(t *testing.T) {
	s := registeredScheduler(t)
	s.loader = NewLoader(LoaderOptions{})
	// A graph built without the registry has no edges, and would draw every
	// automation as standing alone: a tree with no cycles, as a picture.
	if _, err := s.AutomationGraphRows(); err == nil || !strings.Contains(err.Error(), "function registry") {
		t.Fatalf("err = %v, want a refusal naming the missing function registry", err)
	}
}

func TestAutomationGraphRows_BuiltOncePerRegisteredSet(t *testing.T) {
	as := graphAutomations(t,
		`@trigger(schedule="0 0 2 * * *")
automation nightly {
  step send {
    builtin sendThing()
  }
}`,
		`@trigger(event="node.created", concept="v1:t:other")
automation onOther {
  step send {
    builtin sendThing()
  }
}`)
	s := registeredScheduler(t, as...)

	first, err := s.AutomationGraphRows()
	if err != nil || len(first) != 2 {
		t.Fatalf("first = %v, err = %v; want two rows", first, err)
	}
	again, err := s.AutomationGraphRows()
	if err != nil {
		t.Fatal(err)
	}
	if &again[0] != &first[0] {
		t.Error("an unchanged registered set was rebuilt; the graph is walked once per set")
	}

	// The same NAME under a new definition is a different set: a re-registered
	// automation must not be served its predecessor's edges.
	replaced := *as[0]
	s.automations[replaced.Name] = &replaced
	rebuilt, err := s.AutomationGraphRows()
	if err != nil {
		t.Fatal(err)
	}
	if &rebuilt[0] == &first[0] {
		t.Error("a replaced automation was served from the old build")
	}
}
