package automations

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/work"
)

func TestLoopGraphBeforeWriteCannotHideAFeedbackEdge(t *testing.T) {
	const concept = "v1:hookgraph:ticket"
	source := registrySource{reg: work.Registry{"finish": {ConstructKind: work.ConstructMutation, Concept: concept, Write: &work.WriteSpec{Kind: "update", Fields: map[string]work.FieldValue{"status": {HasLiteral: true, Literal: "done"}}}}}}
	after := &Automation{Name: "finishTicket", Trigger: &TriggerConfig{Event: "graph.node.created." + concept, Filter: `row => row.status != "done"`}, Steps: []*Step{{ID: "finish", Type: StepTypeFunction, Function: &FunctionStepConfig{Name: "finish", Kind: "mutation"}}}}
	hook := &Automation{Name: "reopenTicket", BeforeWrite: &BeforeWriteConfig{On: "update", Concept: concept}, Steps: []*Step{{ID: "status", Type: StepTypeFieldWrite, FieldWrite: &FieldWriteConfig{Field: "status", Value: "open"}}}}
	if g := BuildLoopGraph([]*Automation{after}, source, 0); len(g.Problems) != 0 || len(g.Edges) != 0 {
		t.Fatalf("without hook the done write should exclude filter: %+v", g)
	}
	g := BuildLoopGraph([]*Automation{after, hook}, source, 0)
	if len(g.Problems) != 1 || len(g.Edges) != 1 || g.Edges[0].From != after.Name || g.Edges[0].To != after.Name || g.Edges[0].Decided {
		t.Fatalf("hook concealed self-cycle: %+v", g)
	}
	if !strings.Contains(g.Edges[0].Reason, "before-write automation reopenTicket") {
		t.Fatal(g.Edges[0].Reason)
	}
	// A create-only hook cannot change the row an explicit update writes.
	hook.BeforeWrite.On = "create"
	if g := BuildLoopGraph([]*Automation{after, hook}, source, 0); len(g.Problems) != 0 {
		t.Fatal("create-only hook changed update proof")
	}
	hook.BeforeWrite.On = "update"
	hook.BeforeWrite.Concept = "v1:other:ticket"
	if g := BuildLoopGraph([]*Automation{after, hook}, source, 0); len(g.Problems) != 0 {
		t.Fatal("unrelated concept hook changed proof")
	}
	hook.BeforeWrite.Concept = concept
	hook.Steps[0].FieldWrite.Field = "unread"
	if g := BuildLoopGraph([]*Automation{after, hook}, source, 0); len(g.Problems) != 0 {
		t.Fatal("unread-field hook changed proof")
	}
}
func TestLoopGraphBeforeWriteNewRowsAndExistingInserts(t *testing.T) {
	const concept = "v1:hookgraph:ticket"
	source := registrySource{reg: work.Registry{"insert": {ConstructKind: work.ConstructMutation, Concept: concept, Write: &work.WriteSpec{Kind: "insert", NewRow: true, Fields: map[string]work.FieldValue{}}}}}
	after := &Automation{Name: "newTicket", Trigger: &TriggerConfig{Event: "graph.node.created." + concept, Filter: `row => row.status == "open"`}, Steps: []*Step{{ID: "insert", Type: StepTypeFunction, Function: &FunctionStepConfig{Name: "insert", Kind: "mutation"}}}}
	hook := &Automation{Name: "openTicket", BeforeWrite: &BeforeWriteConfig{On: "create", Concept: concept}, Steps: []*Step{{ID: "status", Type: StepTypeFieldWrite, FieldWrite: &FieldWriteConfig{Field: "status", Value: "open"}}}}
	if g := BuildLoopGraph([]*Automation{after}, source, 0); len(g.Problems) != 0 {
		t.Fatal("new row without status should not trigger")
	}
	if g := BuildLoopGraph([]*Automation{after, hook}, source, 0); len(g.Problems) != 1 {
		t.Fatal("hook's previously absent field was ignored")
	}
	hook.BeforeWrite.On = "update"
	if g := BuildLoopGraph([]*Automation{after, hook}, source, 0); len(g.Problems) != 0 {
		t.Fatal("update hook ran on a guaranteed new row")
	}
	source.reg["insert"].Write.NewRow = false
	if g := BuildLoopGraph([]*Automation{after, hook}, source, 0); len(g.Problems) != 1 {
		t.Fatal("insert with existing id ignored update hook")
	}
}
func TestAuthoredBeforeWriteChecksCyclesAmongExistingNodes(t *testing.T) {
	scheduler := authoredForgeScheduler(t, nil, true)
	scheduler.shipped = graphAutomations(t, `@trigger(event="node.created", concept="v1:forge:request")
@filter(row => row.status != "done")
automation finishRequest { mutation advanceRequest(status: "done") }`)
	candidate := &Automation{Name: "reopen", BeforeWrite: &BeforeWriteConfig{On: "update", Concept: "v1:forge:request"}, Steps: []*Step{{ID: "status", Type: StepTypeFieldWrite, FieldWrite: &FieldWriteConfig{Field: "status", Value: "open"}}}}
	if err := scheduler.refuseCandidateCycle(candidate, "authored:owner:reopen"); err == nil || !strings.Contains(err.Error(), "[loop_cycle]") {
		t.Fatalf("hook changed an existing node's cycle without refusal: %v", err)
	}
}
