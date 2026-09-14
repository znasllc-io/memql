package compiler

// automation_compile_test.go -- the compiled JSON of an automation, which the
// automations runtime reads (component/automations/expressions_v1.go).

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/znasllc-io/memql/component/language/parser"
)

// compileV1 parses src and compiles its one automation or logic body the way
// the loader and the LogicRunner do.
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
	f, err := parser.ParseFile(normalised)
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

// TestCompileV1AutomationTriggerFilter: the trigger filter is the lambda's
// canonical source, and a statement's args field read is an expression leaf.
func TestCompileV1AutomationTriggerFilter(t *testing.T) {
	c := asJSON(t, compileV1(t, `@trigger(event="node.created", concept="v1:probe:thing")
@filter(row => row.status startsWith "arch")
automation probe {
  args {
    status string
  }
  first := logic other(s: args.status, e: event, n: 1)
}`))
	trigger := c["trigger"].(map[string]any)
	if trigger["filter"] != `row => row.status startsWith "arch"` {
		t.Fatalf("trigger.filter = %#v", trigger["filter"])
	}
	wantJSON(t, "the step", stepsByID(t, c)["first"]["function"], `{"name": "other", "kind": "logic", "args": {
		"s": {"$expr": "args.status"}, "e": {"$expr": "event"}, "n": 1}}`)
}
