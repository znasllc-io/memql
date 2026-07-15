package memql

import (
	"strings"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	parser "github.com/znasllc-io/memql/component/language/parser"
)

// dot_access_test.go pins #2542 item 4: a DotAccess after a step/collection
// accessor (`rows.first().createdAt`) must convert at load and evaluate
// in-memory, instead of failing function load with "unsupported parser
// expression type: *ast.DotAccessExpr".

// convertDotAccess parses src and converts it with the logic-scope
// converter, returning the engine expression.
func convertDotAccess(t *testing.T, src string) ExpressionNode {
	t.Helper()
	pexpr, err := parser.ParseExpression(src)
	if err != nil {
		t.Fatalf("parse %q: %v", src, err)
	}
	engineExpr, err := NewASTConverter(WithCollectionMethods()).ConvertExpression(pexpr)
	if err != nil {
		t.Fatalf("convert %q: %v", src, err)
	}
	return engineExpr
}

func dotAccessSampleArgs() map[string]any {
	return map[string]any{
		"rows": []any{
			map[string]any{
				"id":        "r1",
				"createdAt": "2026-01-01T00:00:00Z",
				"score":     10,
				"payload":   map[string]any{"name": "alice"},
			},
			map[string]any{
				"id":        "r2",
				"createdAt": "2026-02-02T00:00:00Z",
				"score":     30,
				"payload":   map[string]any{"name": "bob"},
			},
		},
		"empty":   []any{},
		"scalars": []any{1, 2, 3},
	}
}

// TestConvertDotAccessExpr pins the converter shape: the parser's
// DotAccessExpr layers become nested engine DotAccessExpression nodes
// wrapping the collection-method chain.
func TestConvertDotAccessExpr(t *testing.T) {
	expr := convertDotAccess(t, `args.rows.first().payload.name`)
	outer, ok := expr.(*DotAccessExpression)
	if !ok {
		t.Fatalf("converted to %T, want *DotAccessExpression", expr)
	}
	if outer.Field != "name" {
		t.Errorf("outer field = %q, want %q", outer.Field, "name")
	}
	inner, ok := outer.Object.(*DotAccessExpression)
	if !ok {
		t.Fatalf("outer object = %T, want *DotAccessExpression", outer.Object)
	}
	if inner.Field != "payload" {
		t.Errorf("inner field = %q, want %q", inner.Field, "payload")
	}
	if _, ok := inner.Object.(*CollectionMethodExpression); !ok {
		t.Fatalf("inner object = %T, want *CollectionMethodExpression", inner.Object)
	}
}

// TestDotAccessRejectedInQueryFilterScope pins the in-memory-only gate: the
// default converter (specs / query filters) rejects the surface at load, the
// same way collection methods are rejected -- DotAccess never reaches SQL
// compilation.
func TestDotAccessRejectedInQueryFilterScope(t *testing.T) {
	pexpr, err := parser.ParseExpression(`args.rows.first().createdAt`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	_, err = NewASTConverter().ConvertExpression(pexpr)
	if err == nil {
		t.Fatalf("default converter must reject a dot access on a call result")
	}
	if !strings.Contains(err.Error(), "not allowed in specs or query filters") &&
		!strings.Contains(err.Error(), "not in specs or query filters") {
		t.Errorf("unexpected error %q; want the scope rejection", err.Error())
	}
}

// TestDotAccessEval pins the evaluation semantics over a seeded collection,
// including the empty-collection and missing-field edges: a nil object or a
// miss yields a clean nil, never a panic or an error.
func TestDotAccessEval(t *testing.T) {
	args := dotAccessSampleArgs()

	cases := []struct {
		name string
		src  string
		want any
	}{
		{"first then field", `args.rows.first().createdAt`, "2026-01-01T00:00:00Z"},
		{"last then field", `args.rows.last().createdAt`, "2026-02-02T00:00:00Z"},
		{"first then nested field", `args.rows.first().payload.name`, "alice"},
		{"chain then first then field", `args.rows.orderByDesc(r => r.score).first().id`, "r2"},
		{"where then first then field", `args.rows.where(r => r.id == "r2").first().createdAt`, "2026-02-02T00:00:00Z"},
		{"empty collection first then field", `args.empty.first().createdAt`, nil},
		{"empty collection first then nested field", `args.empty.first().payload.name`, nil},
		{"missing field", `args.rows.first().doesNotExist`, nil},
		{"field on scalar element", `args.scalars.first().x`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expr := convertDotAccess(t, tc.src)
			got, err := evalCollScalar(expr, args, nil)
			if err != nil {
				t.Fatalf("evaluate %q: %v", tc.src, err)
			}
			if got != tc.want {
				t.Errorf("evaluate %q = %#v, want %#v", tc.src, got, tc.want)
			}
		})
	}
}

// TestEvaluateCollectionChainString_TrailingField pins the string-path
// entry the multi-step logic runner and forEach sources use: a chain
// carrying a trailing field pluck evaluates end-to-end (before #2542 item 4
// the DotAccessExpr top level made isChain=false and the source string
// leaked through as a literal).
func TestEvaluateCollectionChainString_TrailingField(t *testing.T) {
	resolveBase := func(basePath string) (any, error) {
		if basePath != "args.rows" {
			t.Fatalf("resolveBase called with %q, want %q", basePath, "args.rows")
		}
		return dotAccessSampleArgs()["rows"], nil
	}

	val, isChain, err := EvaluateCollectionChainString(
		`args.rows.where(r => r.score > 20).first().payload.name`,
		resolveBase,
		nil,
	)
	if err != nil {
		t.Fatalf("evaluate chain with trailing field: %v", err)
	}
	if !isChain {
		t.Fatalf("a chain with a trailing field pluck must be recognised as a chain")
	}
	if val != "bob" {
		t.Errorf("value = %#v, want %q", val, "bob")
	}

	// The empty edge stays a clean nil.
	val, isChain, err = EvaluateCollectionChainString(
		`args.rows.where(r => r.score > 999).first().payload.name`,
		resolveBase,
		nil,
	)
	if err != nil || !isChain {
		t.Fatalf("empty-result chain: isChain=%v err=%v, want (true, nil)", isChain, err)
	}
	if val != nil {
		t.Errorf("empty-result pluck = %#v, want nil", val)
	}
}

// TestConvertDotAccessExpr_RejectsCallResultObject pins the load-time gate
// on the DotAccess OBJECT. The parser wraps EVERY call form in DotAccessExpr
// (consumePostCallDotAccess fires after plain function calls and
// kind-prefixed mutation calls too), but evaluation only walks a field off
// a collection accessor/method chain (evalCollScalar's DotAccess case). A
// plain call-result object must keep failing at LOAD -- as main did with
// "unsupported parser expression type" -- instead of loading green and
// dying at call time with a misleading lambda error or a silent nil.
func TestConvertDotAccessExpr_RejectsCallResultObject(t *testing.T) {
	for _, src := range []string{
		`getUser(id: args.id).name`,            // plain function (query) call result
		`mutation createThing(id: args.id).id`, // kind-prefixed mutation call result
	} {
		pexpr, err := parser.ParseExpression(src)
		if err != nil {
			t.Fatalf("parse %q: %v", src, err)
		}
		_, cerr := NewASTConverter(WithCollectionMethods()).ConvertExpression(pexpr)
		if cerr == nil {
			t.Fatalf("convert %q succeeded; a field access on a plain call result must be rejected at load", src)
		}
		if !strings.Contains(cerr.Error(), "collection accessor/method chain") {
			t.Errorf("convert %q error = %q; want the call-result object rejection", src, cerr.Error())
		}
	}
}

func dotAccessLoadRegistry() memoryNodes.Registry {
	return newMemoryRegistry(map[string]*memoryNodes.Concept{
		"v1:common:thing": {Name: "v1:common:thing"},
	})
}

// TestLogicDotAccess_CallResultObjectRejectedAtLoad is the loader-altitude
// twin of the converter gate: a single-statement logic body plucking a field
// off a plain query-function call result must fail tryParseNewFunctionSyntax
// (load), not defer to a runtime failure inside engine.executeWith.
func TestLogicDotAccess_CallResultObjectRejectedAtLoad(t *testing.T) {
	src := strings.Join([]string{
		"@enabled",
		"@useQuery(getUser)",
		"@description(\"call-result field access must fail at load\")",
		"logic logicCallResultPluck {",
		"  args {",
		"    id string @required",
		"  }",
		"  body {",
		"    return getUser( id: args.id ).name",
		"  }",
		"}",
	}, "\n")

	_, err := tryParseNewFunctionSyntax("logicCallResultPluck", "logic", src, "common.logic.memql", dotAccessLoadRegistry())
	if err == nil {
		t.Fatalf("load must reject a field access on a plain function-call result")
	}
	if !strings.Contains(err.Error(), "collection accessor/method chain") {
		t.Errorf("load error = %q; want the call-result object rejection", err.Error())
	}
}

// TestLogicDotAccessLoads is the exact #2542 item 4 load-rejection repro at
// the function-loader altitude: a single-statement logic body plucking a
// scalar field off a collection accessor loads end-to-end (struct rewriter
// -> language parser -> AST converter) into a DotAccessExpression fn.Expr.
// Before the fix this failed with "unsupported parser expression type:
// *ast.DotAccessExpr".
func TestLogicDotAccessLoads(t *testing.T) {
	src := strings.Join([]string{
		"@enabled",
		"@description(\"pluck the newest row's createdAt\")",
		"logic logicNewestCreatedAt {",
		"  args {",
		"    rows object @required",
		"  }",
		"  body {",
		"    return args.rows.first().createdAt",
		"  }",
		"}",
	}, "\n")

	fn, err := tryParseNewFunctionSyntax("logicNewestCreatedAt", "logic", src, "common.logic.memql", dotAccessLoadRegistry())
	if err != nil {
		t.Fatalf("load logic with .first().field return: %v", err)
	}
	if fn == nil || fn.Expr == nil {
		t.Fatalf("expected fn.Expr to be set")
	}
	if _, ok := fn.Expr.(*DotAccessExpression); !ok {
		t.Fatalf("fn.Expr = %T, want *DotAccessExpression", fn.Expr)
	}
}

// TestLogicDotAccessLoads_MultiStep pins the multi-step shape from the
// issue: `q := query(...); return q.first().createdAt`. The loader converts
// the `_return` expression for fn.Expr even when the body has intermediate
// steps, so the DotAccess case must convert here too (this was the load
// failure); the body is stashed on fn.LogicSteps for the LogicRunner.
func TestLogicDotAccessLoads_MultiStep(t *testing.T) {
	src := strings.Join([]string{
		"@enabled",
		"@useQuery(queryThing)",
		"@description(\"pluck a field off a step result\")",
		"logic logicPluckStepField {",
		"  args {",
		"    id string @required",
		"  }",
		"  body {",
		"    q := queryThing( id: args.id )",
		"    return q.first().createdAt",
		"  }",
		"}",
	}, "\n")

	fn, err := tryParseNewFunctionSyntax("logicPluckStepField", "logic", src, "common.logic.memql", dotAccessLoadRegistry())
	if err != nil {
		t.Fatalf("load multi-step logic with .first().field return: %v", err)
	}
	if fn == nil || fn.Expr == nil {
		t.Fatalf("expected fn.Expr to be set")
	}
	if _, ok := fn.Expr.(*DotAccessExpression); !ok {
		t.Fatalf("fn.Expr = %T, want *DotAccessExpression", fn.Expr)
	}
	if fn.LogicSteps == nil {
		t.Fatalf("expected fn.LogicSteps for the multi-step body")
	}
}
