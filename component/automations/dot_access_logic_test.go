package automations

import (
	"context"
	"reflect"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

// dot_access_logic_test.go pins #2542 item 4 END-TO-END through the
// LogicRunner: a logic whose statements pluck a scalar field off a query's
// rows via `.first().field` must load AND evaluate in process -- only the
// query is a call; the return never reaches the engine.

// pluckLogic is a two-statement logic: a query, then a return over it.
func pluckLogic(returnExpr string) string {
	return `@enabled
@description("pluck a scalar field off a query's rows (#2542 item 4)")
logic logicPluckStepField {
  args {
    id string @required
  }
  q := query queryThing(id: args.id)
  return ` + returnExpr + `
}`
}

// runPluck runs a logic through the LogicRunner, its query answered with rows.
func runPluck(t *testing.T, src string, args map[string]any, rows ...map[string]any) (any, *stmtProbe) {
	t.Helper()
	r, probe, _ := newLogicRig(nil)
	probe.answers["queryThing"] = rowsResult(rows...)
	name, body := compiledLogic(t, src)
	out, err := r.RunLogicBody(context.Background(), name, body, args)
	if err != nil {
		t.Fatalf("RunLogicBody returned error (#2542 item 4 -- .first().field must load and evaluate): %v", err)
	}
	return out, probe
}

// TestLogicRunner_DotAccessAfterFirst is the issue's exact shape:
// `q := query ...; return q.first().createdAt`. The query is served by the
// probe; the `.first().createdAt` return resolves locally against the bound
// rows. Only the query may reach the registry.
func TestLogicRunner_DotAccessAfterFirst(t *testing.T) {
	out, probe := runPluck(t, pluckLogic("q.first().createdAt"), map[string]any{"id": "x"},
		map[string]any{"id": "r1", "createdAt": "2026-01-01T00:00:00Z", "payload": map[string]any{"active": true}},
		map[string]any{"id": "r2", "createdAt": "2026-02-02T00:00:00Z", "payload": map[string]any{"active": false}},
	)
	if out != "2026-01-01T00:00:00Z" {
		t.Errorf("return = %#v, want the first row's createdAt", out)
	}
	if got := probe.callees(); !reflect.DeepEqual(got, []string{"queryThing"}) {
		t.Errorf("calls = %v, want exactly [queryThing] (the return must resolve locally)", got)
	}
}

// TestLogicRunner_DotAccessAfterFirst_EmptyResult pins the empty edge: an
// empty query result makes `.first()` nil, and the field access yields a
// clean nil return -- no panic, no error.
func TestLogicRunner_DotAccessAfterFirst_EmptyResult(t *testing.T) {
	out, _ := runPluck(t, pluckLogic("q.first().createdAt"), map[string]any{"id": "x"})
	if out != nil {
		t.Errorf("return = %#v, want nil for an empty result", out)
	}
}

// TestLogicRunner_DotAccessAfterChain pins the chain form: a collection chain
// over the rows followed by the field pluck (`q.skip(1).first().createdAt`,
// and a lambda-carrying one) evaluates in memory with the trailing field
// applied.
func TestLogicRunner_DotAccessAfterChain(t *testing.T) {
	rows := []map[string]any{
		{"id": "r1", "createdAt": "2026-01-01T00:00:00Z", "payload": map[string]any{"active": false}},
		{"id": "r2", "createdAt": "2026-02-02T00:00:00Z", "payload": map[string]any{"active": true}},
	}
	for _, ret := range []string{"q.skip(1).first().createdAt", "q.where(r => r.active).first().createdAt"} {
		out, probe := runPluck(t, pluckLogic(ret), map[string]any{"id": "x"}, rows...)
		if out != "2026-02-02T00:00:00Z" {
			t.Errorf("%s = %#v, want the second row's createdAt", ret, out)
		}
		if got := probe.callees(); !reflect.DeepEqual(got, []string{"queryThing"}) {
			t.Errorf("%s: calls = %v, want exactly [queryThing] (the chain return must resolve locally)", ret, got)
		}
	}
}

// TestLogicRunner_ArgsRootedDotAccess pins the ARGS-rooted sibling of the
// pluck in a multi-statement body: `q := query ...; return
// args.rows.first().createdAt` resolves against the caller's args. Only the
// query may reach the registry.
func TestLogicRunner_ArgsRootedDotAccess(t *testing.T) {
	src := `@enabled
@description("pluck a scalar field off a caller-arg collection (#2542 item 4)")
logic logicPluckArgField {
  args {
    id string @required
    rows object @required
  }
  q := query queryThing(id: args.id)
  return args.rows.first().createdAt
}`
	out, probe := runPluck(t, src, map[string]any{
		"id": "x",
		"rows": []any{
			map[string]any{"id": "r1", "createdAt": "2026-01-01T00:00:00Z"},
			map[string]any{"id": "r2", "createdAt": "2026-02-02T00:00:00Z"},
		},
	})
	if out != "2026-01-01T00:00:00Z" {
		t.Errorf("return = %#v, want the first arg row's createdAt", out)
	}
	if got := probe.callees(); !reflect.DeepEqual(got, []string{"queryThing"}) {
		t.Errorf("calls = %v, want exactly [queryThing] (the return must resolve locally)", got)
	}
}

// TestReturnDotAccessAfterAccessor pins the return shapes directly,
// table-driven across the accessor + field shapes over a query statement's
// rows and over a caller-arg collection. An accessor over an empty collection
// reads no value; an unknown name is refused.
func TestReturnDotAccessAfterAccessor(t *testing.T) {
	newEval := func() *Evaluator {
		e := NewEvaluator()
		e.SetCustom("args", map[string]any{
			"members": []any{
				map[string]any{"id": "m1", "joinedAt": "2025-05-05T00:00:00Z", "payload": map[string]any{"name": "mia"}},
				map[string]any{"id": "m2", "joinedAt": "2025-06-06T00:00:00Z", "payload": map[string]any{"name": "moe"}},
			},
			"empty": []any{},
		})
		e.enterStatements()
		e.Bind("rows", functionStatementValue("query", rowsResult(
			map[string]any{"id": "a", "createdAt": "2026-01-01T00:00:00Z", "payload": map[string]any{"name": "alice"}},
			map[string]any{"id": "b", "createdAt": "2026-02-02T00:00:00Z", "payload": map[string]any{"name": "bob"}},
		)))
		e.Bind("none", functionStatementValue("query", rowsResult()))
		return e
	}

	cases := []struct {
		name string
		expr string
		want any // nil: no value (absent or nil)
	}{
		{"first then field", "rows.first().createdAt", "2026-01-01T00:00:00Z"},
		{"last then field", "rows.last().id", "b"},
		{"first then payload field", "rows.first().name", "alice"},
		{"empty result first then field", "none.first().createdAt", nil},
		{"args root first then field", "args.members.first().joinedAt", "2025-05-05T00:00:00Z"},
		{"args root last then field", "args.members.last().id", "m2"},
		{"args root first then nested field", "args.members.first().payload.name", "mia"},
		{"args root empty then field", "args.empty.first().joinedAt", nil},
		{"chain then field", `rows.where(r => r.id == "b").first().createdAt`, "2026-02-02T00:00:00Z"},
		{"?? over an accessor", `rows.first().id ?? "fallback"`, "a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			val, err := evalV1(newEval(), tc.expr)
			if err != nil {
				t.Fatalf("%s: %v", tc.expr, err)
			}
			if tc.want == nil {
				if val != nil && !memql.IsAbsent(val) {
					t.Errorf("%s = %#v, want no value", tc.expr, val)
				}
				return
			}
			if val != tc.want {
				t.Errorf("%s = %#v, want %#v", tc.expr, val, tc.want)
			}
		})
	}

	if val, err := evalV1(newEval(), "notAStatement.first().createdAt"); err == nil {
		t.Errorf("an unknown name read = %#v, want an unknown-name refusal", val)
	}
}
