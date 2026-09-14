package compiler

// automation_generator_v1_test.go -- the compiled JSON of a body parsed in
// the edition-2026 expression grammar (memql#5367), which the automations
// runtime reads (component/automations/expressions_v1.go).

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/language/parser"
)

// compileV1 parses src with the edition-2026 grammar on and compiles its one
// automation or logic body the way the loader and the LogicRunner do.
func compileV1(t *testing.T, src string) map[string]any {
	t.Helper()
	out, err := compileV1Err(t, src)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return out
}

func compileV1Err(t *testing.T, src string) (map[string]any, error) {
	t.Helper()
	normalised, err := parser.NormaliseAll(src)
	if err != nil {
		t.Fatalf("NormaliseAll: %v", err)
	}
	f, err := parser.ParseFileWithOptions(normalised, parser.Options{ExpressionsV1: true})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var def *parser.FunctionDef
	for _, d := range f.Definitions {
		if fn, ok := d.(*parser.FunctionDef); ok {
			def = fn
		}
	}
	if def == nil {
		t.Fatal("no definition in the source")
	}
	// A logic body compiles as the LogicRunner compiles it: wrapped as an
	// automation.
	wrapped := &parser.FunctionDef{Name: def.Name, Type: parser.FunctionTypeAutomation, Body: def.Body, ArgsSchema: def.ArgsSchema, Attributes: def.Attributes}
	res, err := NewDefault().CompileFile(&parser.File{Definitions: []parser.Node{wrapped}})
	if err != nil {
		return nil, err
	}
	return res.Automations[0].JSON, nil
}

// asJSON re-reads a compiled automation through JSON, as the loader reads it.
func asJSON(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func stepsByID(t *testing.T, compiled map[string]any) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	var walk func(v any)
	walk = func(v any) {
		list, _ := v.([]any)
		for _, el := range list {
			s := el.(map[string]any)
			out[s["id"].(string)] = s
			if fe, ok := s["forEach"].(map[string]any); ok {
				walk(fe["do"])
			}
		}
	}
	walk(compiled["steps"])
	return out
}

func wantJSON(t *testing.T, what string, got any, want string) {
	t.Helper()
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var g, w any
	_ = json.Unmarshal(b, &g)
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("bad want JSON for %s: %v", what, err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("%s:\n  got  %s\n  want %s", what, b, want)
	}
}

// TestCompileV1LogicBody pins the compiled shape of a v1 logic body: the
// marker, conditions and sources as canonical source with no rewrite, value
// leaves as literals and `{"$expr"}` leaves, a string-typed field as v1
// source, a catalog call as an in-process query step, and the return.
func TestCompileV1LogicBody(t *testing.T) {
	c := asJSON(t, compileV1(t, `logic conflictDetection {
  args {
    event object!
  }
  body {
    matchingConfirmed := query detectConflicts( partitionId: args.event.payload.partitionId, recordType: "invoice" )
    emitConflicts := if !matchingConfirmed.empty() { publishEvent( topic: "data.conflicts.detected", payload: { partitionId: args.event.payload.partitionId, matchCount: matchingConfirmed.count(), tags: ["a", args.event.payload.tag], requiresHumanApproval: true, at: now } ) }
    lowered := lower(args.event.payload.recordType)
    fallback := args.event.payload.a ?? "d"
    for item := range matchingConfirmed.nodes() {
      touch := if item.payload.stale == true {
        markStale(id: item.id)
      }
    }
    return emitConflicts
  }
}`))
	if c["expressions"] != "v1" {
		t.Fatalf(`"expressions" = %#v, want "v1"`, c["expressions"])
	}
	if c["_return"] != "emitConflicts" {
		t.Fatalf("_return = %#v", c["_return"])
	}
	steps := stepsByID(t, c)

	wantJSON(t, "the construct call", steps["matchingConfirmed"], `{
		"id": "matchingConfirmed", "type": "function",
		"function": {"name": "detectConflicts", "args": {
			"partitionId": {"$expr": "args.event.payload.partitionId"},
			"recordType": "invoice"}}}`)

	wantJSON(t, "the event step", steps["emitConflicts"], `{
		"id": "emitConflicts", "type": "event",
		"condition": "!matchingConfirmed.empty()",
		"event": {"topic": "data.conflicts.detected", "payload": {
			"partitionId": {"$expr": "args.event.payload.partitionId"},
			"matchCount": {"$expr": "matchingConfirmed.count()"},
			"tags": ["a", {"$expr": "args.event.payload.tag"}],
			"requiresHumanApproval": true,
			"at": {"$expr": "now"}}}}`)

	// A catalog function is evaluated in process: a query step.
	wantJSON(t, "the catalog call", steps["lowered"], `{
		"id": "lowered", "type": "query",
		"query": {"query": "lower(args.event.payload.recordType)"}}`)
	wantJSON(t, "an expression statement", steps["fallback"], `{
		"id": "fallback", "type": "query",
		"query": {"query": "args.event.payload.a ?? \"d\""}}`)

	loop := steps["forEach_item_0"]["forEach"].(map[string]any)
	if loop["source"] != "matchingConfirmed.nodes()" || loop["as"] != "item" {
		t.Fatalf("forEach = %#v", loop)
	}
	wantJSON(t, "the nested step", steps["touch"], `{
		"id": "touch", "type": "function",
		"condition": "item.payload.stale == true",
		"function": {"name": "markStale", "args": {"id": {"$expr": "item.id"}}}}`)
}

// TestCompileV1AutomationTriggerFilter: the trigger filter is the lambda's
// canonical source, and a step's bare args field is an expression leaf.
func TestCompileV1AutomationTriggerFilter(t *testing.T) {
	c := asJSON(t, compileV1(t, `@trigger(event="node.created", concept="v1:probe:thing")
@filter(row => row.status startsWith "arch")
automation probe {
  args {
    status string
  }
  step first {
    logic other(s: status, e: event, n: 1)
  }
}`))
	if c["expressions"] != "v1" {
		t.Fatalf(`"expressions" = %#v`, c["expressions"])
	}
	trigger := c["trigger"].(map[string]any)
	if trigger["filter"] != `row => row.status startsWith "arch"` {
		t.Fatalf("trigger.filter = %#v", trigger["filter"])
	}
	wantJSON(t, "the step", stepsByID(t, c)["first"]["function"], `{"name": "other", "args": {
		"s": {"$expr": "status"}, "e": {"$expr": "event"}, "n": 1}}`)
}

// TestCompileV1OrdersByV1References: the topological sort reads a v1 step's
// free names -- a bare step name included, which the legacy extractor could
// not see -- so a step is emitted after the steps it reads.
func TestCompileV1OrdersByV1References(t *testing.T) {
	c := compileV1(t, `logic ordered {
  args {
    event object!
  }
  body {
    second := useIt(v: first, w: steps.third.status)
    first := makeIt()
    third := makeOther()
    return second
  }
}`)
	var order []string
	for _, s := range c["steps"].([]map[string]any) {
		order = append(order, s["id"].(string))
	}
	pos := map[string]int{}
	for i, id := range order {
		pos[id] = i
	}
	if pos["second"] < pos["first"] || pos["second"] < pos["third"] {
		t.Fatalf("order %v: `second` reads `first` and `steps.third` and must follow both", order)
	}
}

// TestCompileV1RefusesALegacyNode: a v1 body holds v1 nodes only; a node of
// the internal query form cannot be printed as v1 source, so it is refused
// rather than written as text the runtime cannot read back.
func TestCompileV1RefusesALegacyNode(t *testing.T) {
	def := &parser.FunctionDef{Name: "mixed", Type: parser.FunctionTypeAutomation, Body: &parser.AutomationDef{
		ExpressionsV1: true,
		Steps: []parser.StepDef{{ID: "s", Type: parser.StepTypeFunction, Config: &parser.FunctionStepConfig{
			Name: "f", Args: map[string]any{"v": &ast.ArgRefExpr{Path: "x"}},
		}}},
	}}
	_, err := NewDefault().CompileFile(&parser.File{Definitions: []parser.Node{def}})
	if err == nil || !strings.Contains(err.Error(), "not an edition-2026 expression") {
		t.Fatalf("want the legacy-node refusal, got %v", err)
	}
}

// TestEncodeValueLeafRule pins the one encoding rule, case by case.
func TestEncodeValueLeafRule(t *testing.T) {
	parse := func(src string) ast.ExpressionNode {
		n, err := parser.ParseV1Expression(src)
		if err != nil {
			t.Fatalf("parse %s: %v", src, err)
		}
		return n
	}
	for src, want := range map[string]string{
		`"s"`:                     `"s"`,
		`12`:                      `12`,
		`1.5`:                     `1.5`,
		`true`:                    `true`,
		`nil`:                     `null`,
		`("paren")`:               `"paren"`,
		`args.x`:                  `{"$expr":"args.x"}`,
		`(args.a + args.b)`:       `{"$expr":"args.a + args.b"}`,
		`args.x ?? "d"`:           `{"$expr":"args.x ?? \"d\""}`,
		`[1, args.x]`:             `[1,{"$expr":"args.x"}]`,
		`{a: 1, b: args.x}`:       `{"a":1,"b":{"$expr":"args.x"}}`,
		`{a: {b: [nil, args.y]}}`: `{"a":{"b":[null,{"$expr":"args.y"}]}}`,
		`{}`:                      `{}`,
		`[]`:                      `[]`,
	} {
		b, err := json.Marshal(EncodeValueLeaf(parse(src)))
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != want {
			t.Errorf("EncodeValueLeaf(%s) = %s, want %s", src, b, want)
		}
	}
	if EncodeValueLeaf(nil) != nil {
		t.Error("EncodeValueLeaf(nil) is not null")
	}
}
