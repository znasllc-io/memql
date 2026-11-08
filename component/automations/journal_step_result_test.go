package automations

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// journal_step_result_test.go -- what a receipt records of an engine result
// (journal_step_result.go). The resume half, over a real mutation's receipt in
// Postgres, is TestResumeV1_DB_AMutationReceiptHoldsItsRowOnceAndNoSchema.

// conceptSchemaStruct is a stand-in for a concept's JSON schema as the engine
// hands it back on a node.
func conceptSchemaStruct(t *testing.T) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(map[string]any{
		"$id":                  "https://memql.io/schemas/v1/probe/thing.json",
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"name":   map[string]any{"type": "string", "description": "What the thing is called."},
			"status": map[string]any{"type": "string", "enum": []any{"active", "archived"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// writtenRow is the result a mutation returns: the one row it wrote, schema
// and all, in a Bundle.
func writtenRow(t *testing.T, id, name string) *memql.ExecuteResult {
	t.Helper()
	payload, err := structpb.NewStruct(map[string]any{"name": name, "status": "active"})
	if err != nil {
		t.Fatal(err)
	}
	return &memql.ExecuteResult{Bundle: &memqlv1.GraphBundle{
		Nodes: []*memqlv1.MemoryNode{{
			Id: id, Concept: "v1:probe:thing", Type: "node", CreatedBy: "system",
			CreatedAt: timestamppb.New(time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)),
			Payload:   payload, Schema: conceptSchemaStruct(t),
		}},
		RootIds: []string{id},
	}}
}

// TestAMutationReceiptRecordsItsRowOnceAndNoSchema runs statements through the
// executor and reads the receipts its journal writes: a bound mutation's
// receipt holds the row once, as its value; an unbound one keeps its nodes,
// without their schemas; and nothing anywhere carries a concept schema.
func TestAMutationReceiptRecordsItsRowOnceAndNoSchema(t *testing.T) {
	probe := newStmtProbe()
	probe.answers["writeThing"] = writtenRow(t, "v1:probe:thing:written", "acme")
	probe.answers["touchThing"] = writtenRow(t, "v1:probe:thing:touched", "acme")
	a := statementAutomation(t, `@trigger(event="probe.fired")
automation writeThenFinish {
  written := mutation writeThing(name: "acme")
  mutation touchThing(name: "acme")
  builtin finish(id: written.id, name: written.name)
}`)
	rec := &recordingJournalExecutor{}
	e := NewExecutor(ExecutorOptions{StepRegistry: probe})
	defer e.Close()
	e.journal = newWorkJournal(rec, nil)
	ev := events.NewEvent("probe.fired", events.KindMessage, nil)
	exec, err := e.ExecuteWithEvent(context.Background(), a, "test", &ev)
	if err != nil || exec.Status != "completed" {
		t.Fatalf("run: %v %+v", err, exec)
	}
	if got := probe.argsOf(t, "finish", 0); got["id"] != "v1:probe:thing:written" || got["name"] != "acme" {
		t.Fatalf("the bound row did not reach the next statement: %v", got)
	}

	names, args := journalWrites(t, rec)
	var bound, unbound map[string]any
	for i, name := range names {
		if name != "updateWorkStep" || args[i]["status"] != "done" {
			continue
		}
		raw, _ := json.Marshal(args[i])
		if strings.Contains(string(raw), `"schema"`) {
			t.Errorf("a receipt records a concept schema:\n  %s", raw)
		}
		result, _ := args[i]["result"].(map[string]any)
		switch {
		case strings.HasSuffix(args[i]["stepId"].(string), "-written"):
			bound = result
		case result != nil && result["result"] != nil:
			unbound = result
		}
	}
	if bound == nil || unbound == nil {
		t.Fatalf("receipts for both mutations were not found: bound=%v unbound=%v (writes %v)", bound, unbound, names)
	}

	// The bound statement: its row once, as the value a resume rebinds.
	if _, has := bound["result"]; has {
		t.Errorf("the bound mutation's receipt still records the Bundle beside its value: %v", bound["result"])
	}
	value, _ := bound["value"].(map[string]any)
	payload, _ := value["payload"].(map[string]any)
	if bound["valueRecorded"] != true || value["id"] != "v1:probe:thing:written" || payload["name"] != "acme" {
		t.Errorf("the bound mutation's value is not the row it wrote: %v", bound)
	}

	// The unbound statement: its nodes stay -- they are the only record of
	// what it returned -- without their schemas.
	nodes := unbound["result"].(map[string]any)["Bundle"].(map[string]any)["nodes"].([]any)
	node, _ := nodes[0].(map[string]any)
	if len(nodes) != 1 || node["id"] != "v1:probe:thing:touched" || node["payload"] == nil {
		t.Errorf("the unbound mutation's receipt lost its row: %v", unbound["result"])
	}
}

// TestJournaledResultLeavesEveryOtherResultAlone: only a Bundle-backed engine
// result is trimmed. A logic's or builtin's flat output and a plain value are
// recorded whole, bound or not -- the validator and the replan prompt read them.
func TestJournaledResultLeavesEveryOtherResultAlone(t *testing.T) {
	flat := memql.NewResultWithOutput(map[string]any{"reply": "done"})
	for name, v := range map[string]any{
		"flat output": UnwrapStepResult(flat),
		"plain map":   map[string]any{"ok": true},
		"scalar":      "text",
		"nil":         nil,
	} {
		for _, recorded := range []bool{false, true} {
			got, _ := json.Marshal(journaledResult(v, recorded))
			want, _ := json.Marshal(v)
			if string(got) != string(want) {
				t.Errorf("%s (value recorded %v) was changed: %s, want %s", name, recorded, got, want)
			}
		}
	}
	if got := journaledResult(&memql.ExecuteResult{}, true); got == nil {
		t.Error("an engine result with no bundle was dropped; it holds no rows for a value to duplicate")
	}
}

// TestBundleWithoutSchemasKeepsEveryOtherNodeField sets every field MemoryNode
// declares and requires the copy to carry each one but the schema. A field the
// message gains later fails the fixture check first, so it is looked at rather
// than silently dropped from receipts.
func TestBundleWithoutSchemasKeepsEveryOtherNodeField(t *testing.T) {
	n := &memqlv1.MemoryNode{}
	m := n.ProtoReflect()
	fields := m.Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		switch {
		case fd.IsList() || fd.IsMap():
			t.Fatalf("MemoryNode.%s is a list or a map: teach this fixture to set one", fd.Name())
		case fd.Kind() == protoreflect.StringKind:
			m.Set(fd, protoreflect.ValueOfString("v-"+string(fd.Name())))
		case fd.Kind() == protoreflect.MessageKind:
			m.Set(fd, m.NewField(fd))
		default:
			t.Fatalf("MemoryNode.%s is a %s: teach this fixture to set one", fd.Name(), fd.Kind())
		}
	}
	n.Schema = conceptSchemaStruct(t)
	got := bundleWithoutSchemas(&memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{n, nil}, RootIds: []string{"r"}})
	if len(got.GetNodes()) != 2 || got.GetNodes()[1] != nil || len(got.GetRootIds()) != 1 {
		t.Fatalf("the copy changed the bundle's shape: %v", got)
	}
	want := proto.Clone(n).(*memqlv1.MemoryNode)
	want.Schema = nil
	if !proto.Equal(want, got.GetNodes()[0]) {
		t.Fatalf("the copy differs from the node by more than its schema:\n got  %v\n want %v", got.GetNodes()[0], want)
	}
	if n.GetSchema() == nil {
		t.Fatal("the original node lost its schema: the live result must keep it")
	}
}
