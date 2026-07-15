package memql

import (
	"strings"
	"testing"

	parser "github.com/znasllc-io/memql/component/language/parser"
)

// cond_projection_test.go covers the #2542 items 2 (cond over a collection
// chain) and 3 (arithmetic in a groupBy-projection object-literal value) at the
// in-memory evaluator altitude (evalCollScalar), the shared surface the logic
// runner and the engine single-return path both bottom out in.

// evalLogicExpr parses, converts (logic/collection context), and evaluates an
// expression to a scalar -- the single-statement logic-body path for ANY
// converted node (a cond FunctionCallExpression, a projection chain, ...),
// unlike evalChain which asserts a *CollectionMethodExpression root.
func evalLogicExpr(t *testing.T, src string, args map[string]any) (any, error) {
	t.Helper()
	pexpr, err := parser.ParseExpression(src)
	if err != nil {
		t.Fatalf("parse %q: %v", src, err)
	}
	engineExpr, err := NewASTConverter(WithCollectionMethods()).ConvertExpression(pexpr)
	if err != nil {
		t.Fatalf("convert %q: %v", src, err)
	}
	return evalCollScalar(engineExpr, args, nil)
}

// --- Item 2: cond over a collection chain ---

func TestEvalCollCond_ChainPredicateAndBranches(t *testing.T) {
	args := sampleMembers()
	cases := []struct {
		name string
		src  string
		want any
	}{
		{
			// Predicate is a bare boolean chain; branches are literals.
			name: "bare_chain_predicate_true",
			src:  `cond(args.members.any(m => m.active), "has", "none")`,
			want: "has",
		},
		{
			// Predicate false -> else branch.
			name: "chain_predicate_false",
			src:  `cond(args.members.all(m => m.active), "all", "some")`,
			want: "some",
		},
		{
			// The chosen branch is ITSELF a collection chain (an aggregate).
			name: "branch_is_chain",
			src:  `cond(args.members.any(m => m.active), args.members.count(m => m.active), 0)`,
			want: 3,
		},
		{
			// A cond nested inside a select projection lambda: the predicate and
			// branches resolve against the per-element scope.
			name: "cond_nested_in_projection",
			src:  `args.members.select(m => cond(m.active, m.name, "inactive"))`,
			want: []any{"alice", "inactive", "carol", "dave"},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got, err := evalLogicExpr(t, tc.src, args)
			if err != nil {
				t.Fatalf("eval %q: %v", tc.src, err)
			}
			if !deepScalarEqual(got, tc.want) {
				t.Errorf("eval %q = %#v, want %#v", tc.src, got, tc.want)
			}
		})
	}
}

// TestCondComparisonOverChain_ParsesAndEvaluates is the FLIP of the wave-2 pin
// TestCondComparisonOverChain_ParseGap (#2542 item-2 headline). A cond whose
// predicate is a comparison over a collection-chain aggregate
// (`cond(x.count(...) > 0, ...)`) previously stopped at the `>` in the
// langparser cond-arg grammar (parseCondFunction -> parseExpressionArg) with a
// three-argument error. Wave 3 extends parseExpressionArg to consume ONE
// relational comparison, emitting the expression-led BinaryComparisonExpr the
// evaluator was already ready for (evaluateCondPredicate -> the condition
// evaluator / evalCollScalar). This pins the now-working parse AND end-to-end
// evaluation through the engine single-return evaluator.
func TestCondComparisonOverChain_ParsesAndEvaluates(t *testing.T) {
	src := `cond(args.members.count(m => m.active) > 0, "some", "none")`
	pexpr, err := parser.ParseExpression(src)
	if err != nil {
		t.Fatalf("parse %q: unexpected error now that the cond-arg grammar consumes a relational comparison: %v", src, err)
	}
	cond, ok := pexpr.(*parser.CondExpr)
	if !ok {
		t.Fatalf("parsed %q to %T, want *parser.CondExpr", src, pexpr)
	}
	if _, ok := cond.Condition.(*parser.BinaryComparisonExpr); !ok {
		t.Errorf("cond predicate parsed to %T, want *parser.BinaryComparisonExpr (expression-led comparison)", cond.Condition)
	}

	// End-to-end through the engine single-return evaluator: two active members
	// -> predicate true -> the "some" branch.
	got, evErr := evalLogicExpr(t, src, sampleMembers())
	if evErr != nil {
		t.Fatalf("eval %q: %v", src, evErr)
	}
	if got != "some" {
		t.Errorf("eval %q = %#v, want \"some\"", src, got)
	}

	// Predicate false -> the else branch.
	none, evErr := evalLogicExpr(t, `cond(args.members.count(m => m.active) > 5, "some", "none")`, sampleMembers())
	if evErr != nil {
		t.Fatalf("eval false-predicate cond: %v", evErr)
	}
	if none != "none" {
		t.Errorf("false-predicate cond = %#v, want \"none\"", none)
	}
}

// --- Item 3: arithmetic in a groupBy-projection object-literal value ---

func projectionScans() map[string]any {
	return map[string]any{
		"scans": []any{
			map[string]any{"worker": "w1", "good": true},
			map[string]any{"worker": "w1", "good": false},
			map[string]any{"worker": "w1", "good": true},
			map[string]any{"worker": "w2", "good": true},
		},
	}
}

func TestEvalCollProjection_ObjectLiteralValues(t *testing.T) {
	args := projectionScans()

	// Bare path projection: {worker: g.key} resolves the scope path, not a raw
	// "g.key" string.
	pathOut, err := evalLogicExpr(t, `args.scans.groupBy(s => s.worker).select(g => {worker: g.key})`, args)
	if err != nil {
		t.Fatalf("path projection: %v", err)
	}
	rows, ok := pathOut.([]any)
	if !ok || len(rows) != 2 {
		t.Fatalf("path projection = %#v, want 2 rows", pathOut)
	}
	if rows[0].(map[string]any)["worker"] != "w1" {
		t.Errorf("row0 worker = %#v, want w1", rows[0])
	}

	// Method-call projection value: {n: g.items.count()}.
	countOut, err := evalLogicExpr(t, `args.scans.groupBy(s => s.worker).select(g => {worker: g.key, n: g.items.count()})`, args)
	if err != nil {
		t.Fatalf("count projection: %v", err)
	}
	if n := countOut.([]any)[0].(map[string]any)["n"]; !deepScalarEqual(n, 3) {
		t.Errorf("w1 count = %#v, want 3", n)
	}

	// Arithmetic projection value: a per-group ratio. int/int is integer
	// division (#2316); the accuracy fraction needs a float operand.
	ratioOut, err := evalLogicExpr(t,
		`args.scans.groupBy(s => s.worker).select(g => {worker: g.key, accuracy: g.items.where(i => i.good).count() / (g.items.count() * 1.0)})`, args)
	if err != nil {
		t.Fatalf("ratio projection: %v", err)
	}
	w1 := ratioOut.([]any)[0].(map[string]any)
	if acc, ok := w1["accuracy"].(float64); !ok || acc < 0.66 || acc > 0.67 {
		t.Errorf("w1 accuracy = %#v, want ~0.6667 (2 good / 3)", w1["accuracy"])
	}
}

// Integer division truncation is the established EvaluateArithmetic (#2316)
// semantics: two integer group counts divide to an integer, so a naive
// good/total is 0 for a minority-good group. Pinned so the "use a float
// operand for a fractional ratio" contract is explicit.
func TestEvalCollProjection_IntegerDivisionTruncates(t *testing.T) {
	args := projectionScans()
	out, err := evalLogicExpr(t,
		`args.scans.groupBy(s => s.worker).select(g => {worker: g.key, acc: g.items.where(i => i.good).count() / g.items.count()})`, args)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	// w1: 2 good / 3 total -> integer 0.
	if acc := out.([]any)[0].(map[string]any)["acc"]; !deepScalarEqual(acc, 0) {
		t.Errorf("w1 integer acc = %#v, want 0 (integer division of 2/3)", acc)
	}
}

// Division by a zero group aggregate surfaces as a clean logic error naming the
// projection value, never a panic -- matching EvaluateArithmetic's div-by-zero
// contract (#2316/#2556); it is not silently coerced to nil.
func TestEvalCollProjection_DivisionByZeroErrors(t *testing.T) {
	args := map[string]any{"scans": []any{map[string]any{"worker": "w1", "good": true}}}
	_, err := evalLogicExpr(t,
		`args.scans.groupBy(s => s.worker).select(g => {worker: g.key, r: g.items.count() / g.items.where(i => i.bad).count()})`, args)
	if err == nil {
		t.Fatalf("division by a zero group aggregate must error")
	}
	if !strings.Contains(err.Error(), "division by zero") {
		t.Errorf("error = %q, want it to name division by zero", err.Error())
	}
}

// The projection object-literal + arithmetic surface is gated: the DEFAULT
// converter (specs / query filters) rejects the whole chain at load, so the
// arithmetic-in-projection never reaches SQL compilation (#2316 in-memory-only
// containment).
func TestProjectionArithmetic_DefaultConverterRejects(t *testing.T) {
	src := `data.groupBy(s => s.worker).select(g => {acc: g.a.count() / g.b.count()})`
	pexpr, err := parser.ParseExpression(src)
	if err != nil {
		t.Fatalf("parse %q: %v", src, err)
	}
	if _, err := NewASTConverter().ConvertExpression(pexpr); err == nil {
		t.Fatalf("default converter accepted %q; collection projections must stay in-memory only", src)
	}
}

// deepScalarEqual compares projection results structurally: []any and
// map[string]any recurse; numerics compare across int/int64/float64; other
// scalars compare by %v.
func deepScalarEqual(got, want any) bool {
	switch w := want.(type) {
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			return false
		}
		for i := range w {
			if !deepScalarEqual(g[i], w[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok || len(g) != len(w) {
			return false
		}
		for k := range w {
			if !deepScalarEqual(g[k], w[k]) {
				return false
			}
		}
		return true
	case int:
		return numericIs(got, int64(w))
	case int64:
		return numericIs(got, w)
	default:
		return got == want
	}
}

func numericIs(v any, n int64) bool {
	switch x := v.(type) {
	case int:
		return int64(x) == n
	case int64:
		return x == n
	case float64:
		return x == float64(n)
	default:
		return false
	}
}
