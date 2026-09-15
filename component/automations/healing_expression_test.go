package automations

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/znasllc-io/memql/component/healing"
	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/language/compiler"
	"github.com/znasllc-io/memql/component/language/parser"
)

func TestHealingReplacementEvaluatesInCompiledStatement(t *testing.T) {
	literal, err := parser.ParseV1Expression(`"/author/path"`)
	if err != nil {
		t.Fatal(err)
	}
	body := &ast.Body{Statements: []ast.BodyStatement{
		&ast.CallStatement{Call: &ast.ConstructCall{Kind: "builtin", Name: "run", Args: []ast.NamedArg{{Name: "path", Value: literal}}}},
	}}
	steps, problems := compiler.CompileBody("automation", "probe", nil, body)
	if len(problems) != 0 {
		t.Fatal(problems)
	}
	raw, err := json.Marshal(map[string]any{"name": "probe", "steps": steps})
	if err != nil {
		t.Fatal(err)
	}
	var base map[string]any
	if err := json.Unmarshal(raw, &base); err != nil {
		t.Fatal(err)
	}
	for _, replacement := range []string{"config.path", "args.path", "event.payload.path", "fetch.path"} {
		t.Run(replacement, func(t *testing.T) {
			patch := healing.Patch{Kind: healing.PatchRelativizeLiteral, Target: "steps.run.function.args.path", Literal: "/author/path", Replacement: replacement}
			out, err := patch.Apply(base)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(out)
			if err != nil {
				t.Fatal(err)
			}
			var automation Automation
			if err := json.Unmarshal(raw, &automation); err != nil {
				t.Fatal(err)
			}
			if err := PrepareExpressions(&automation); err != nil {
				t.Fatal(err)
			}
			evaluator := NewEvaluator()
			evaluator.names = newNameFrame(nil)
			evaluator.names.bind("fetch", map[string]any{"path": "/this/machine"})
			evaluator.SetCustom("config", map[string]any{"path": "/this/machine"})
			evaluator.SetCustom("args", map[string]any{"path": "/this/machine"})
			evaluator.SetCustom("event", map[string]any{"payload": map[string]any{"path": "/this/machine"}})
			args, err := evaluator.ResolveV1Map(context.Background(), automation.Steps[0].Function.Args)
			if err != nil {
				t.Fatal(err)
			}
			if args["path"] != "/this/machine" {
				t.Fatalf("healed argument = %#v; want resolved value", args["path"])
			}
		})
	}
}
