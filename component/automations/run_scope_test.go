package automations

// run_scope_test.go -- RunScope's name resolution, root by root, and the
// round trip of the compiled value-leaf encoding (memql#5367).

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/language/compiler"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// evalV1Src parses src and evaluates it over e.
func evalV1Src(t *testing.T, e *Evaluator, src string) (any, error) {
	t.Helper()
	n, err := languageParser.ParseV1Expression(src)
	if err != nil {
		t.Fatalf("parse %s: %v", src, err)
	}
	return e.EvalV1(context.Background(), n)
}

// evalV1 parses src and evaluates it over e -- the spelling for a probe that
// has no *testing.T. A parse error is returned as the error.
func evalV1(e *Evaluator, src string) (any, error) {
	n, err := languageParser.ParseV1Expression(src)
	if err != nil {
		return nil, err
	}
	return e.EvalV1(context.Background(), n)
}

// evalV1Cond parses src and evaluates it over e as a condition.
func evalV1Cond(t *testing.T, e *Evaluator, src string) (bool, error) {
	t.Helper()
	n, err := languageParser.ParseV1Expression(src)
	if err != nil {
		t.Fatalf("parse %s: %v", src, err)
	}
	return e.EvalV1Condition(context.Background(), n)
}

// TestRunScopeResolution walks the lookup order in run_scope.go's file
// comment -- the statement frames, then the roots -- over an evaluator seeded
// as the executor seeds one.
func TestRunScopeResolution(t *testing.T) {
	e := NewEvaluator()
	e.SetCustom("event", map[string]any{
		"topic": "probe.fired", "kind": "message",
		"payload": map[string]any{"x": "hello", "n": float64(2)},
	})
	e.SetCustom("args", map[string]any{"x": "argX"})
	e.SetCustom("now", "2026-09-13T10:00:00Z")
	e.SetCustom("actor", map[string]any{"userId": "user-7"})
	e.SetVariableResolver(func(_ context.Context, name string) (string, error) {
		if name == "known" {
			return "value", nil
		}
		return "", errors.New("no such variable")
	})
	e.enterStatements()
	e.Bind("rows", functionStatementValue("query", rowsResult(
		map[string]any{"id": "r1", "payload": map[string]any{"active": true}},
		map[string]any{"id": "r2", "payload": map[string]any{"active": false}},
	)))
	e.Bind("flat", map[string]any{"y": "Y"})
	e.names.declare("skipped")

	item := e.ChildFrame()
	item.Bind("thing", map[string]any{"name": "it"})

	cases := []struct {
		name string
		e    *Evaluator
		src  string
		want any
	}{
		{"event root", e, "event.payload.x", "hello"},
		{"args root", e, "args.x", "argX"},
		{"an args field the run did not bind is absent", e, "args.opt ?? \"fallback\"", "fallback"},
		{"actor", e, "actor.userId", "user-7"},
		{"a statement name is its rows", e, "rows.count()", int64(2)},
		{"a row reads its intrinsics", e, "rows.first().id", "r1"},
		{"a row reads its payload fields directly", e, "rows.where(r => r.active).count()", int64(1)},
		{"a statement name is its value", e, "flat.y", "Y"},
		{"a name no statement bound is absent", e, "skipped ?? \"absent\"", "absent"},
		{"var() resolves", e, "var(\"known\")", "value"},
		{"the loop variable in its own frame", item, "thing.name", "it"},
		{"the frames around the loop's", item, "flat.y", "Y"},
		{"the run clock", e, "now", "2026-09-13T10:00:00Z"},
		{"an unseeded root is absent", e, "partition ?? \"none\"", "none"},
		{"an unseeded actor is the denying envelope", NewEvaluator(), "actor.isClusterOwner", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := evalV1Src(t, c.e, c.src)
			if err != nil {
				t.Fatalf("%s: %v", c.src, err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("%s = %#v (%T), want %#v (%T)", c.src, got, got, c.want, c.want)
			}
		})
	}

	// An unknown name is an error, never its own text -- and so is each tier
	// the statement runtime retired: the step-result roots, the loop item, the
	// input and error roots, a bare args field, a loop variable outside its
	// loop.
	for _, src := range []string{
		`typo == "typo"`,
		`steps.rows.status`,
		`automation.errors`,
		`item.name`,
		`input`,
		`ctx.input`,
		`error`,
		`x`,
		`payload.x`,
		`thing.name`,
	} {
		t.Run("unknown: "+src, func(t *testing.T) {
			_, err := evalV1Src(t, e, src)
			var ee *memql.ExprError
			if !errors.As(err, &ee) || ee.Code != "unknown_name" {
				t.Fatalf("%s: want unknown_name, got %v", src, err)
			}
		})
	}
}

// TestRunScopeEmptyRead: a query statement that read no rows reads as no
// rows, whichever form the engine's empty answer takes -- no bundle, a bundle
// with no nodes, an empty flat output.
func TestRunScopeEmptyRead(t *testing.T) {
	for name, empty := range map[string]*memql.ExecuteResult{
		"no bundle":         {},
		"a bundle, no rows": {Bundle: &memqlv1.GraphBundle{}},
		"an empty output":   memql.NewResultWithOutput([]any{}),
	} {
		t.Run(name, func(t *testing.T) {
			e := NewEvaluator()
			e.enterStatements()
			e.Bind("rows", functionStatementValue("query", empty))
			for src, want := range map[string]any{
				"rows.empty()":             true,
				"rows.count()":             int64(0),
				"rows.first() ?? \"none\"": "none",
			} {
				got, err := evalV1Src(t, e, src)
				if err != nil {
					t.Fatalf("%s: %v", src, err)
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("%s = %#v (%T), want %#v", src, got, got, want)
				}
			}
		})
	}
}

// TestEncodeValueLeafRoundTrip: decoding compiler.EncodeValueLeaf(e) -- the
// compiled value-leaf encoding, through the JSON a load reads and the
// preparation and resolution a v1 step runs -- yields what EvalExpr(e)
// yields over the same scope, the container rule included (an absent entry
// omits its key or element).
//
// Compared through JSON: a load reads compiled JSON, and JSON has one number
// type, so a literal `1` comes back float64 where EvalExpr says int64 --
// equal as the wire value, and the wire value is what a step hands on. The
// in-memory encoding (no JSON) must match exactly.
func TestEncodeValueLeafRoundTrip(t *testing.T) {
	e := NewEvaluator()
	e.SetCustom("args", map[string]any{"x": "ex", "a": float64(2), "b": float64(3), "list": []any{"p", "q"}})
	e.SetCustom("event", map[string]any{"payload": map[string]any{"k": "v"}})

	sources := []string{
		`"literal"`,
		`42`,
		`-7`,
		`2.5`,
		`true`,
		`nil`,
		`args.x`,
		`args.missing`,
		`args.x ?? "d"`,
		`args.missing ?? "d"`,
		`(args.a + args.b)`,
		`[1, "two", args.x, args.missing, nil]`,
		`{lit: 1, ref: args.x, gone: args.missing, null: nil}`,
		`{outer: {inner: [args.x, {deep: event.payload.k}], n: args.a * 2}}`,
		`args.list.where(v => v != "p")`,
		`args.x == "ex" ? {ok: true} : {ok: false}`,
	}
	for _, src := range sources {
		t.Run(src, func(t *testing.T) {
			node, err := languageParser.ParseV1Expression(src)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			want, err := e.EvalV1(context.Background(), node)
			if err != nil {
				t.Fatalf("EvalExpr: %v", err)
			}
			encoded := compiler.EncodeValueLeaf(node)

			// In memory: prepare and resolve the encoding as it stands.
			direct := decodeValueLeaf(t, e, encoded)
			if !reflect.DeepEqual(direct, want) {
				t.Fatalf("decode(EncodeValueLeaf(%s)) = %#v, EvalExpr = %#v\n  encoding: %#v", src, direct, want, encoded)
			}

			// Through JSON, as a load reads it.
			b, err := json.Marshal(map[string]any{"v": encoded})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var loaded map[string]any
			if err := json.Unmarshal(b, &loaded); err != nil {
				t.Fatal(err)
			}
			viaJSON, present := decodeValueLeafMap(t, e, loaded)["v"]
			if want == memql.Absent {
				if present {
					t.Fatalf("an absent value came back as %#v; the container rule omits it", viaJSON)
				}
				return
			}
			if !present {
				t.Fatalf("decoding through JSON lost the value %#v", want)
			}
			if jsonOf(t, viaJSON) != jsonOf(t, want) {
				t.Fatalf("through JSON: %s, EvalExpr: %s", jsonOf(t, viaJSON), jsonOf(t, want))
			}
		})
	}

	// The rule is recursive: a map with one expression value is an object
	// with a `$expr` leaf inside, never a whole-map `$expr`.
	node, err := languageParser.ParseV1Expression(`{a: 1, b: args.x}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := jsonOf(t, compiler.EncodeValueLeaf(node)); got != `{"a":1,"b":{"$expr":"args.x"}}` {
		t.Fatalf("EncodeValueLeaf({a: 1, b: args.x}) = %s", got)
	}
	// Parentheses are stripped before the rule applies.
	paren := &ast.ParenExpr{Inner: &ast.LiteralExpr{Value: "x"}}
	if got := compiler.EncodeValueLeaf(paren); got != "x" {
		t.Fatalf("EncodeValueLeaf((\"x\")) = %#v, want the literal", got)
	}
}

// decodeValueLeaf prepares one encoded value as PrepareExpressions prepares a
// value-map leaf, then resolves it as a v1 step does. A top-level absent value
// comes back as the Absent sentinel.
func decodeValueLeaf(t *testing.T, e *Evaluator, encoded any) any {
	t.Helper()
	prepared, err := (&exprPreparer{automation: "roundTrip"}).value("v", encoded)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	v, err := e.ResolveV1Value(context.Background(), prepared)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	return v
}

// decodeValueLeafMap prepares and resolves a value map, as a v1 step's args.
func decodeValueLeafMap(t *testing.T, e *Evaluator, m map[string]any) map[string]any {
	t.Helper()
	prepared, err := (&exprPreparer{automation: "roundTrip"}).valueMap("args", m)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	out, err := e.ResolveV1Map(context.Background(), prepared)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	return out
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %#v: %v", v, err)
	}
	return string(b)
}
