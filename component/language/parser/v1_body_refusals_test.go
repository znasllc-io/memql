package parser

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/ast"
)

// memqlmigrate:keep-file -- every fixture here is written to be refused, the
// retired body forms among them; carried across by the bodies rewrite, a case
// would parse and stop testing its refusal.

// v1BodyRefusalCases are parsed with ParseFile directly: the struct-form
// rewriter leaves a logic and an automation as written, so the statement
// parser is what refuses each of these.
var v1BodyRefusalCases = []struct {
	name string
	code string
	src  string
	at   string // "line:col"
	msg  string // a substring of the message
}{
	{"a step block", codeBodyStepRetired,
		"automation a {\n  step run {\n    logic l(event: event)\n  }\n}", "2:3", "`step run { ... }` is retired in edition 2026: write the step's call as a statement, `run := <call>` (memqlmigrate --rewrite=bodies rewrites it)"},
	{"a body block in a logic", codeBodyBlockRetired,
		"logic l {\n  body {\n    return 1\n  }\n}", "2:3", "`body { }` is retired in edition 2026: a logic's statements follow its args block directly"},
	{"a body block in an automation", codeBodyBlockRetired,
		"automation a {\n  body {\n    x := 1\n  }\n}", "2:3", "`body { }` is retired in edition 2026: an automation's statements follow its args block directly"},
	{"the terse header", codeBodyTerseRetired,
		"automation a @trigger(event=\"x\") => logic l", "1:1", "the terse `automation a @trigger(...) => logic L` form is retired in edition 2026: write @trigger(...) above `automation a { logic L(event: event) }`"},
	{"a steps reference", codeBodyStepsReferenceRetired,
		"automation a {\n  x := steps.s.result\n}", "2:8", "`steps.<id>...` is retired in edition 2026: a statement's name is its value"},
	{"a steps reference inside a call argument", codeBodyStepsReferenceRetired,
		"automation a {\n  mutation m(v: steps.s.result)\n}", "2:17", "`steps.<id>...` is retired"},
	{"forEach", codeBodyForEachRetired,
		"automation a {\n  forEach x in args.list {\n  }\n}", "2:3", "`forEach` is retired in edition 2026: write `for <x> in <source> if <cond> { }`"},
	{"for range", codeBodyForRangeRetired,
		"automation a {\n  for x := range args.list {\n  }\n}", "2:3", "`for x := range <source>` is retired in edition 2026: write `for x in <source>`"},
	{"a conditional assignment", codeBodyConditionalAssignRetired,
		"automation a {\n  x := if args.c {\n    query q()\n  }\n}", "2:8", "`x := if <cond> { <call> }` is retired in edition 2026: write `if <cond> { x := <call> }`"},
	{"publishEvent as a statement", codeBodyPublishEventRetired,
		"automation a {\n  publishEvent(topic: \"t\", payload: {})\n}", "2:3", "`publishEvent(...)` is retired in edition 2026: write `publish \"<topic>\" { ... }` in an automation"},
	{"publishEvent inside an expression", codeBodyPublishEventRetired,
		"automation a {\n  x := publishEvent(topic: \"t\")\n}", "2:8", "`publishEvent(...)` is retired"},
	{"the step accessor", codeBodyAccessorRetired,
		"logic l {\n  s := logic other()\n  return step(\"s\")\n}", "3:10", "`step(\"s\")` is retired in edition 2026: a statement's name is its value, so write `s`"},
	{"the input accessor", codeBodyAccessorRetired,
		"automation a {\n  mutation m(v: input())\n}", "2:17", "`input()` is retired in edition 2026: an automation declares its arguments in `args { }` and reads `args.<name>`"},
	{"the item accessor in a loop", codeBodyAccessorRetired,
		"automation a {\n  for x in args.list {\n    mutation m(v: item())\n  }\n}", "3:19", "`item()` is retired in edition 2026: a loop names its element, `for x in <source>`, and reads `x`"},
	{"the index accessor in a loop", codeBodyAccessorRetired,
		"logic l {\n  for x in args.list {\n    n := index()\n  }\n  return 0\n}", "3:10", "`index()` is retired in edition 2026: a loop names its element, `for x in <source>`, and has no index"},
	{"a call with no kind", codeBodyCallKindMissing,
		"automation a {\n  doThing(a: 1)\n}", "2:3", "`doThing(...)` names no construct kind: write `query`, `mutation`, `logic`, `builtin`, `automation` or `action` before it"},
	{"a legacy object call with no kind", codeBodyCallKindMissing,
		"automation a {\n  doThing { a: 1 }\n}", "2:3", "`doThing { ... }` names no construct kind"},
	{"a legacy object call bound to a name", codeBodyCallKindMissing,
		"automation a {\n  x := doThing { a: 1 }\n}", "2:8", "`doThing { ... }` names no construct kind: write `x := <kind> doThing(<named args>)`"},
	{"the declaration verb where the call kind belongs", codeBodyCallKindMissing,
		"automation a {\n  mutate createNode(a: 1)\n}", "2:3", "`mutate` is not a construct kind a statement calls"},
	{"an inline query block", codeBodyCallKindMissing,
		"automation a {\n  x := query { concept == \"a\" }\n}", "2:8", "`query { ... }` is not a statement: a statement calls a query declared elsewhere, by name -- `x := query <name>(<named args>)`"},
	{"an inline mutation block as a statement", codeBodyCallKindMissing,
		"automation a {\n  mutation { id: \"a\" }\n}", "2:3", "`mutation { ... }` is not a statement: a statement calls a mutation declared elsewhere, by name -- `mutation <name>(<named args>)`"},
	{"a construct kind called as a function", codeBodyCallKindMissing,
		"automation a {\n  x := mutation(concept: \"v1:a:b\")\n}", "2:8", "`mutation(...)` is not a call: a mutation is declared elsewhere and called by name, as a statement of its own"},
	{"a construct kind called as a statement", codeBodyCallKindMissing,
		"automation a {\n  query(concept: \"v1:a:b\")\n}", "2:3", "`query(...)` is not a call: a query is declared elsewhere and called by name"},
	{"a capability call", codeBodyCallKindMissing,
		"automation a {\n  capability script(script: \"x\")\n}", "2:3", "a capability is called from an action's body"},
	{"the argument pun", codeBodyPositionalArgument,
		"automation a {\n  logic l(event)\n}", "2:11", "`logic l(event)` passes event without a name: write `event: <value>`; the bare-argument pun is retired"},
	{"a pun beside named arguments", codeBodyPositionalArgument,
		"automation a {\n  x := logic l(mode: \"a\", event)\n}", "2:27", "passes event without a name"},
	{"a positional literal", codeBodyPositionalArgument,
		"automation a {\n  query q(\"x\")\n}", "2:11", "passes an argument without a name"},
	{"a call nested in an expression", codeBodyCallInExpression,
		"automation a {\n  x := 1 + query q()\n}", "2:12", "a construct call is a statement of its own: bind it, `<n> := query q(...)`, and read <n> here"},
	{"a call nested in a call argument", codeBodyCallInExpression,
		"automation a {\n  mutation m(v: query q())\n}", "2:17", "a construct call is a statement of its own"},
	{"two statements on one line", codeBodyOneStatementPerLine,
		"automation a {\n  x := 1 y := 2\n}", "2:10", "one statement per line: `y` starts a second statement on line 2"},
	{"a statement after a closing brace", codeBodyOneStatementPerLine,
		"automation a {\n  if args.c { mutation m() } mutation n()\n}", "2:30", "one statement per line"},
	{"else on its own line", codeBodyElsePlacement,
		"automation a {\n  if args.c {\n    mutation m()\n  }\n  else {\n    mutation n()\n  }\n}", "5:3", "`else` follows the closing brace on the same line: `} else {`"},
	{"retry on an expression", codeBodyRetryPlacement,
		"automation a {\n  x := 1 retry(2)\n}", "2:10", "`retry(n)` applies to a construct call statement"},
	{"retry on its own line", codeBodyRetryPlacement,
		"automation a {\n  x := query q()\n  retry(2)\n}", "3:3", "`retry(n)` follows the construct call it retries, on the same line"},
	{"retry before the call", codeBodyRetryPlacement,
		"automation a {\n  x := retry(2) query q()\n}", "2:8", "`x := retry(n) <call>` is retired in edition 2026: write the clause after the call, `x := <call> retry(n)`"},
	{"on error on an expression", codeBodyOnErrorPlacement,
		"automation a {\n  x := 1 on error continue\n}", "2:10", "`on error continue` applies to a call, a `for` or a `parallel` statement"},
	{"on error on a return", codeBodyOnErrorPlacement,
		"logic l {\n  return builtin b() on error continue\n}", "2:22", "`on error continue` applies to a call, a `for` or a `parallel` statement"},
	{"on error on an if", codeBodyOnErrorPlacement,
		"automation a {\n  if args.c {\n    mutation m()\n  } on error continue\n}", "4:5", "applies to a call, a `for` or a `parallel`"},
	{"on error on its own line", codeBodyOnErrorPlacement,
		"automation a {\n  mutation m()\n  on error continue\n}", "3:3", "`on error continue` follows its statement on the same line"},
	{"on surface on a query", codeBodySurfacePlacement,
		"automation a {\n  query q() on surface(\"ops\")\n}", "2:13", "`on surface(...)` applies to an `action` call"},
	{"clauses out of order", codeBodyClauseOrder,
		"automation a {\n  query q() on error continue retry(2)\n}", "2:31", "trailing clauses are written once each, in the order `on surface(...)`, `retry(n)`, `on error continue`"},
	{"a clause written twice", codeBodyClauseOrder,
		"automation a {\n  query q() retry(1) retry(2)\n}", "2:22", "written once each"},
	{"wait after on error", codeBodyClauseOrder,
		"automation a {\n  parallel {\n    branch x {\n      builtin b()\n    }\n  } on error continue wait any\n}", "6:23", "`wait any` comes before `on error continue`"},
	{"on error stop", codeBodyDefaultClause,
		"automation a {\n  mutation m() on error stop\n}", "2:25", "`on error stop` is the default: delete it"},
	{"wait all", codeBodyDefaultClause,
		"automation a {\n  parallel {\n    branch x {\n      builtin b()\n    }\n  } wait all\n}", "6:10", "`wait all` is the default: delete it"},
	{"a case label that is not a literal", codeBodyCaseLabel,
		"automation a {\n  switch args.s {\n    case args.t {\n      builtin b()\n    }\n  }\n}", "3:10", "a case label is a literal written once: `args.t`"},
	{"a case label written twice", codeBodyCaseLabel,
		"automation a {\n  switch args.s {\n    case \"a\" {\n      builtin b()\n    }\n    case \"b\", \"a\" {\n      builtin c()\n    }\n  }\n}", "6:15", "a case label is a literal written once: `\"a\"`"},
	{"two defaults", codeBodyCaseLabel,
		"automation a {\n  switch args.s {\n    default {\n      builtin b()\n    }\n    default {\n      builtin c()\n    }\n  }\n}", "6:5", "a switch has one `default`"},
	{"an if with no condition", codeBodyMissingExpression,
		"automation a {\n  if {\n    mutation m()\n  }\n}", "2:6", "missing condition between `if` and `{`: write `if <condition> { ... }`"},
	{"an else if with no condition", codeBodyMissingExpression,
		"automation a {\n  if args.c {\n    mutation m()\n  } else if {\n    mutation n()\n  }\n}", "4:13", "missing condition between `if` and `{`"},
	{"a for with no source", codeBodyMissingExpression,
		"automation a {\n  for x in {\n    mutation m()\n  }\n}", "2:12", "missing source between `in` and `{`: write `for <x> in <source> { ... }`"},
	{"a switch with no subject", codeBodyMissingExpression,
		"automation a {\n  switch {\n    default {\n      builtin b()\n    }\n  }\n}", "2:10", "missing subject between `switch` and `{`"},
	{"wait with another value", codeBodyWaitValue,
		"automation a {\n  parallel {\n    branch x {\n      builtin b()\n    }\n  } wait some\n}", "6:10", "`wait` takes `any`"},
	{"an automation with no statement", codeBodyEmpty,
		"automation a {\n}", "2:1", "automation a has no statement: an automation has at least one"},
	{"partition on a trigger", codeTriggerPartitionRetired,
		"@trigger(event=\"node.created\", partition=\"*\")\nautomation a {\n  mutation m()\n}", "1:1", "`partition=` on @trigger is retired in edition 2026: delete it (memqlmigrate --rewrite=bodies rewrites it)"},
	{"the schedule synonym", codeTriggerScheduleRetired,
		"@schedule(cron=\"0 * * * * *\")\nautomation a {\n  mutation m()\n}", "1:1", "`@schedule(cron=...)` is retired in edition 2026: write `@trigger(schedule=...)`"},
}

func TestV1BodyRefusals(t *testing.T) {
	for _, c := range v1BodyRefusalCases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseFile(c.src)
			var br *BodyRefusal
			if !errors.As(err, &br) {
				t.Fatalf("got %v, want a %s refusal", err, c.code)
			}
			if br.Code != c.code {
				t.Fatalf("code = %s, want %s (%v)", br.Code, c.code, err)
			}
			if at := fmt.Sprintf("%d:%d", br.Parse.Line, br.Parse.Column); at != c.at {
				t.Errorf("position = %s, want %s (%v)", at, c.at, err)
			}
			if !strings.Contains(br.Parse.Message, c.msg) {
				t.Errorf("message %q does not contain %q", br.Parse.Message, c.msg)
			}
			if !strings.HasSuffix(err.Error(), " ["+c.code+"]") {
				t.Errorf("Error() = %q: the code is the last thing it prints, in brackets", err.Error())
			}
			var pe *ParseError
			if !errors.As(err, &pe) || !errors.Is(err, ErrInvalidSyntax) {
				t.Errorf("a body refusal unwraps to the positioned *ParseError and to ErrInvalidSyntax")
			}
		})
	}
}

// TestV1BodyRefusalCodesAreEachReached holds the code list to the cases in
// both directions: a code nothing produces is a code the parser does not
// have, and a case's code must be listed.
func TestV1BodyRefusalCodesAreEachReached(t *testing.T) {
	listed := map[string]bool{}
	for _, c := range BodyRefusalCodes() {
		listed[c] = true
	}
	reached := map[string]bool{}
	for _, c := range v1BodyRefusalCases {
		if !listed[c.code] {
			t.Errorf("case %q refuses with %s, which BodyRefusalCodes does not list", c.name, c.code)
		}
		reached[c.code] = true
	}
	for c := range listed {
		if !reached[c] {
			t.Errorf("%s is listed and no case produces it: add one", c)
		}
	}
}

// TestBodyStatementFormsMatchTheParser holds BodyStatementForms to
// v1BodyCases: every listed form is accepted by at least one case, and every
// form the cases use is listed.
func TestBodyStatementFormsMatchTheParser(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range v1BodyCases {
		fn := parseV1BodyFile(t, c.src)
		ast.WalkBody(v1Body(t, fn).Statements, func(s ast.BodyStatement) bool {
			seen[ast.StatementKind(s)] = true
			var call *ast.ConstructCall
			var mods ast.StatementMods
			switch v := s.(type) {
			case *ast.AssignStatement:
				call, mods = v.Call, v.Mods
			case *ast.CallStatement:
				call, mods = v.Call, v.Mods
			case *ast.ReturnStatement:
				call, mods = v.Call, v.Mods
			case *ast.ForStatement:
				mods = v.Mods
			case *ast.ParallelStatement:
				mods = v.Mods
				if v.Wait == "any" {
					seen["wait"] = true
				}
			case *ast.IfStatement:
				if len(v.Branches) > 1 {
					seen["else"] = true
				}
			}
			if call != nil && call.Surface != "" {
				seen["onSurface"] = true
			}
			if mods.Retry > 0 {
				seen["retry"] = true
			}
			if mods.OnError != "" {
				seen["onError"] = true
			}
			return true
		})
	}
	listed := map[string]bool{}
	for _, f := range BodyStatementForms() {
		listed[f] = true
		if !seen[f] {
			t.Errorf("%s is a listed statement form no case in v1BodyCases accepts", f)
		}
	}
	var extra []string
	for f := range seen {
		if !listed[f] {
			extra = append(extra, f)
		}
	}
	sort.Strings(extra)
	if len(extra) > 0 {
		t.Errorf("the cases use forms BodyStatementForms does not list: %v", extra)
	}
	// The words Sense offers where a statement starts are forms the cases
	// accept.
	for _, kw := range BodyStatementKeywords() {
		if !listed[kw] {
			t.Errorf("%s opens a statement (BodyStatementKeywords) and BodyStatementForms does not list it", kw)
		}
	}
}

// TestBodyCallKindsAreEachCalled: every kind BodyCallKinds lists is called by
// a case in v1BodyCases, so the list Sense offers is the parser's.
func TestBodyCallKindsAreEachCalled(t *testing.T) {
	called := map[string]bool{}
	for _, c := range v1BodyCases {
		ast.WalkBody(v1Body(t, parseV1BodyFile(t, c.src)).Statements, func(s ast.BodyStatement) bool {
			switch v := s.(type) {
			case *ast.AssignStatement:
				if v.Call != nil {
					called[v.Call.Kind] = true
				}
			case *ast.CallStatement:
				if v.Call != nil {
					called[v.Call.Kind] = true
				}
			case *ast.ReturnStatement:
				if v.Call != nil {
					called[v.Call.Kind] = true
				}
			}
			return true
		})
	}
	for _, k := range BodyCallKinds() {
		if !called[k] {
			t.Errorf("no case in v1BodyCases calls a %s, which BodyCallKinds lists", k)
		}
	}
}
