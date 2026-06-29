package conformance

// conf_2274_test.go -- the object-literal-logic-return dimension (#2274).
//
// An object-literal `return { k: <expr>, ... }` from a logic must resolve its
// values (refs, method calls, arg reads, nested objects) and yield a flat map a
// downstream step reads via field(decide.result, "x") / decide.result.x. Two
// bugs blocked it: the compiler `%v`-stringified the return map (AST pointers ->
// invalid query), and the engine round-trip re-stringified referenced values
// (a node list rendered as `[...]`). The fix renders the object literal as valid
// MemQL in the compiler AND evaluates it locally in the LogicRunner (resolve
// each value against the evaluator, keep Go values, no re-stringify).
//
// This drives a REAL multi-step logic with an object-literal return through the
// REAL LogicRunner against a seeded DB and asserts every value resolved: a
// literal, an event-arg ref, and a prior-step method call. It FAILS on pre-fix
// main (the return is the `map[...]` garbage string / engine parse error) and
// PASSES once both halves of the fix land.

import (
	"testing"

	"github.com/znasllc-io/memql/component/automations"
	automationSteps "github.com/znasllc-io/memql/component/automations/steps"
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

func parseLogicForTest(t *testing.T, src string) *languageParser.AutomationDef {
	t.Helper()
	norm, err := languageParser.NormaliseAll(src)
	if err != nil {
		t.Fatalf("#2274: NormaliseAll: %v", err)
	}
	toks, err := languageParser.NewLexer(norm).Tokenize()
	if err != nil {
		t.Fatalf("#2274: Tokenize: %v", err)
	}
	ast, err := languageParser.NewParser(toks).Parse()
	if err != nil {
		t.Fatalf("#2274: Parse: %v", err)
	}
	for _, def := range ast.(*languageParser.File).Definitions {
		if fd, ok := def.(*languageParser.FunctionDef); ok && fd.Type == languageParser.FunctionTypeLogic {
			return fd.Body.(*languageParser.AutomationDef)
		}
	}
	t.Fatal("#2274: no logic in source")
	return nil
}

func runObjectLiteralReturn(t *testing.T, e *Env) {
	src := `logic objLitReturnProbe {
  args { event object @required }
  body {
    found := participantSession({ participantId: args.event.payload.id })
    return { wasEmpty: found.empty(), pid: args.event.payload.id, lit: "constant" }
  }
}`
	body := parseLogicForTest(t, src)
	runner := automations.NewLogicRunner(e.Eng, automationSteps.NewRegistry(), e.Eng.Logger)
	out, err := runner.RunLogic(e.Ctx, "objLitReturnProbe", body,
		map[string]any{"event": map[string]any{"payload": map[string]any{"id": "v1:cognition:participant:objlit-2274"}}})
	if err != nil {
		t.Fatalf("#2274: RunLogic failed (object-literal return not handled): %v", err)
	}
	m, ok := automations.UnwrapStepResult(out).(map[string]any)
	if !ok {
		t.Fatalf("#2274: return is not a flat map (the %%v-stringify / round-trip bug): %T %#v", out, out)
	}
	if m["lit"] != "constant" {
		t.Errorf("#2274: lit = %#v, want \"constant\"", m["lit"])
	}
	if m["pid"] != "v1:cognition:participant:objlit-2274" {
		t.Errorf("#2274: pid = %#v, want the event id (arg ref did not resolve)", m["pid"])
	}
	if m["wasEmpty"] != true {
		t.Errorf("#2274: wasEmpty = %#v, want true (prior-step method ref did not resolve)", m["wasEmpty"])
	}
	t.Logf("#2274: object-literal return resolved flat (literal + arg ref + step method) OK")
}
