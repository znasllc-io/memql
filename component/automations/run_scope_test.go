package automations

// run_scope_test.go -- RunScope's name resolution, root by root, and the
// round trip of the compiled value-leaf encoding (memql#5367).

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

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
// comment, one root at a time, over an evaluator seeded as the executor
// seeds one.
func TestRunScopeResolution(t *testing.T) {
	e := NewEvaluator()
	e.SetCustom("event", map[string]any{
		"topic": "probe.fired", "kind": "message",
		"payload": map[string]any{"x": "hello", "n": float64(2)},
	})
	e.SetCustom("args", map[string]any{"x": "argX"})
	e.SetCustom("argsDeclared", map[string]bool{"x": true, "opt": true})
	e.SetCustom("timestamp", "2026-09-13T10:00:00Z")
	e.SetCustom("actor", map[string]any{"userId": "user-7"})
	e.SetStepResult("rows", &StepResult{StepId: "rows", Status: "success",
		Result: map[string]any{"Bundle": map[string]any{"nodes": []any{
			map[string]any{"id": "r1", "payload": map[string]any{"active": true}},
			map[string]any{"id": "r2", "payload": map[string]any{"active": false}},
		}}}})
	e.SetStepResult("flat", &StepResult{StepId: "flat", Status: "success", Result: map[string]any{"y": "Y"}})
	e.SetStepResult("failed", &StepResult{StepId: "failed", Status: "failed", Error: "kaboom"})
	e.SetVariableResolver(func(_ context.Context, name string) (string, error) {
		if name == "known" {
			return "value", nil
		}
		return "", errors.New("no such variable")
	})

	item := e.Clone()
	item.SetItem(map[string]any{"name": "it"}, "thing")
	item.SetCustom("index", 3)

	cases := []struct {
		name string
		e    *Evaluator
		src  string
		want any
	}{
		{"event root", e, "event.payload.x", "hello"},
		{"event retry: an envelope key read bare", e, "payload.x", "hello"},
		{"args root", e, "args.x", "argX"},
		{"bare args field (G2)", e, "x", "argX"},
		{"declared absent optional field is unset", e, "opt ?? \"fallback\"", "fallback"},
		{"actor", e, "actor.userId", "user-7"},
		{"a bare step is its rows", e, "rows.count()", int64(2)},
		{"rows are node maps", e, "rows.first().id", "r1"},
		{"a lambda over the rows", e, "rows.where(r => r.payload.active).count()", int64(1)},
		{"step accessors under steps", e, "steps.rows.status", "success"},
		{"steps.x.result of a flat result", e, "steps.flat.result.y", "Y"},
		{"a flat step read bare", e, "flat.y", "Y"},
		{"a step accessor read bare", e, "failed.error", "kaboom"},
		{"Ran", e, "steps.flat.Ran", true},
		{"automation.errors", e, "automation.errors", []any{"failed: kaboom"}},
		{"var resolves", e, "var.known", "value"},
		{"an unresolved var is absent", e, "var.missing ?? \"off\"", "off"},
		{"the loop variable under its name", item, "thing.name", "it"},
		{"index", item, "index", 3},
		{"the run clock", e, "now", "2026-09-13T10:00:00Z"},
		{"an unseeded reserved root is absent", e, "error ?? \"none\"", "none"},
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

	t.Run("an unknown name is an error, never its own text", func(t *testing.T) {
		_, err := evalV1Src(t, e, "typo == \"typo\"")
		var ee *memql.ExprError
		if !errors.As(err, &ee) || ee.Code != "unknown_name" {
			t.Fatalf("want unknown_name, got %v", err)
		}
	})
}

// TestRunScopeEmptyRead: a query step that read no rows stands for the empty
// row list, not for its envelope -- an empty bundle's JSON omits `nodes`, so
// GetStepNodes finds none -- in both the engine's result and its decoded
// map. The string evaluator's accessors read it as zero rows; so must an
// expression, or `rows.empty()` over an empty read is false and
// `rows.nodes()` is a list holding the envelope. Found by the logic-body
// equivalence corpus (component/automations/steps).
func TestRunScopeEmptyRead(t *testing.T) {
	for name, empty := range map[string]any{
		"an ExecuteResult":        &memql.ExecuteResult{},
		"a decoded envelope":      map[string]any{"Bundle": map[string]any{}},
		"an envelope with no key": map[string]any{"Bundle": nil},
	} {
		t.Run(name, func(t *testing.T) {
			e := NewEvaluator()
			e.SetStepResult("rows", &StepResult{StepId: "rows", Status: "success", Result: empty})
			for src, want := range map[string]any{
				"rows.empty()":             true,
				"rows.nodes()":             []any{},
				"rows.count()":             int64(0),
				"rows.first() ?? \"none\"": "none",
				"rows":                     []any{},
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
