package conformance

// conf_2274_test.go -- the object-literal-logic-return dimension (#2274).
//
// An object-literal `return { k: <expr>, ... }` from a logic must resolve its
// values (names, method calls, arg reads, nested objects) and yield a flat map
// the calling statement reads by name (`decide.x`). Two bugs once blocked it:
// the compiler `%v`-stringified the return map (AST pointers -> invalid
// query), and the engine round-trip re-stringified referenced values (a node
// list rendered as `[...]`).
//
// This drives a REAL logic with an object-literal return through the REAL
// LogicRunner's statement runner against a seeded DB and asserts every value
// resolved: a literal, an event-arg read, and a method call on an earlier
// statement's value.

import (
	"testing"

	"github.com/znasllc-io/memql/component/automations"
	automationSteps "github.com/znasllc-io/memql/component/automations/steps"
	"github.com/znasllc-io/memql/component/language/compiler"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

func objectLiteralReturnCheck() check {
	return check{
		Issue:   "#2274",
		Dim:     "object-literal-logic-return",
		NeedsDB: true,
		Run:     runObjectLiteralReturn,
	}
}

// compileLogicForTest parses src's one logic and compiles its statements as
// the function loader does.
func compileLogicForTest(t *testing.T, src string) []map[string]any {
	t.Helper()
	f, err := languageParser.ParseFile(src)
	if err != nil {
		t.Fatalf("#2274: parse: %v", err)
	}
	for _, def := range f.Definitions {
		fd, ok := def.(*languageParser.FunctionDef)
		if !ok || fd.Type != languageParser.FunctionTypeLogic {
			continue
		}
		var args []string
		if fd.ArgsSchema != nil {
			for _, a := range fd.ArgsSchema.Fields {
				args = append(args, a.Name)
			}
		}
		steps, problems := compiler.CompileBody("logic", fd.Name, args, fd.Body.(*languageParser.AutomationDef).Body)
		if len(problems) > 0 {
			t.Fatalf("#2274: compile: %v", compiler.BodyProblems(problems))
		}
		return steps
	}
	t.Fatal("#2274: no logic in source")
	return nil
}

func runObjectLiteralReturn(t *testing.T, e *Env) {
	src := `logic objLitReturnProbe {
  args {
    event object!
  }
  found := query workspaceForRun(runId: args.event.payload.id)
  return { wasEmpty: found.empty(), pid: args.event.payload.id, lit: "constant" }
}`
	body := compileLogicForTest(t, src)
	runner := automations.NewLogicRunner(e.Eng, automationSteps.NewRegistry(), e.Eng.Logger)
	out, err := runner.RunLogicBody(e.Ctx, "objLitReturnProbe", body,
		map[string]any{"event": map[string]any{"payload": map[string]any{"id": "v1:work:run:objlit-2274"}}})
	if err != nil {
		t.Fatalf("#2274: the logic failed (object-literal return not handled): %v", err)
	}
	m, ok := automations.UnwrapStepResult(out).(map[string]any)
	if !ok {
		t.Fatalf("#2274: return is not a flat map (the %%v-stringify / round-trip bug): %T %#v", out, out)
	}
	if m["lit"] != "constant" {
		t.Errorf("#2274: lit = %#v, want \"constant\"", m["lit"])
	}
	if m["pid"] != "v1:work:run:objlit-2274" {
		t.Errorf("#2274: pid = %#v, want the event id (arg ref did not resolve)", m["pid"])
	}
	if m["wasEmpty"] != true {
		t.Errorf("#2274: wasEmpty = %#v, want true (the method call on an earlier value did not resolve)", m["wasEmpty"])
	}
	t.Logf("#2274: object-literal return resolved flat (literal + arg read + method call) OK")
}
