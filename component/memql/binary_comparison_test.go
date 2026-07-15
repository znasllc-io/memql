package memql

import (
	"strings"
	"testing"

	parser "github.com/znasllc-io/memql/component/language/parser"
)

// binary_comparison_test.go pins #2542 item-5 residual: an EXPRESSION-LED
// comparison (`r.count - 10 > 0`, `0 < r.count`) must convert under the
// logic/collection scope and evaluate in-memory, and must be REJECTED by the
// default converter (specs / query filters) so it never reaches SQL.

// convertBinaryComparison parses src and converts it with the logic-scope
// converter, returning the engine expression.
func convertBinaryComparison(t *testing.T, src string) ExpressionNode {
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

// TestConvertBinaryComparisonExpr pins the converter shape: the parser's
// BinaryComparisonExpr becomes an engine BinaryComparisonExpression whose
// operands are the converted sub-expressions (an ArithmeticExpression LHS and a
// literal RHS for `count - 10 > 0`).
func TestConvertBinaryComparisonExpr(t *testing.T) {
	expr := convertBinaryComparison(t, `count - 10 > 0`)
	cmp, ok := expr.(*BinaryComparisonExpression)
	if !ok {
		t.Fatalf("converted to %T, want *BinaryComparisonExpression", expr)
	}
	if cmp.Operator != OpGt {
		t.Errorf("operator = %q, want >", cmp.Operator)
	}
	if _, ok := cmp.Left.(*ArithmeticExpression); !ok {
		t.Errorf("left = %T, want *ArithmeticExpression", cmp.Left)
	}
	if _, ok := cmp.Right.(*LiteralValueNode); !ok {
		t.Errorf("right = %T, want *LiteralValueNode", cmp.Right)
	}
}

// TestBinaryComparisonRejectedInQueryFilterScope pins the in-memory-only gate:
// the default converter (specs / query filters) rejects the expression-led
// comparison at load, exactly like arithmetic and collection methods -- it
// never reaches SQL compilation.
func TestBinaryComparisonRejectedInQueryFilterScope(t *testing.T) {
	for _, src := range []string{`count - 10 > 0`, `0 < count`} {
		pexpr, err := parser.ParseExpression(src)
		if err != nil {
			t.Fatalf("parse %q: %v", src, err)
		}
		_, err = NewASTConverter().ConvertExpression(pexpr)
		if err == nil {
			t.Fatalf("default converter must reject an expression-led comparison %q", src)
		}
		if !strings.Contains(err.Error(), "only available in logic") {
			t.Errorf("%q: unexpected error %q; want the scope rejection", src, err.Error())
		}
	}
}

// TestBinaryComparisonEval pins the in-memory evaluation over a bound lambda
// scope: both operands evaluate through the collection subset and the operator
// applies via the shared runtimeCompareValues. Numeric and edge (nil operand)
// cases are covered.
func TestBinaryComparisonEval(t *testing.T) {
	// The lambda param `r` binds to this element.
	locals := map[string]any{
		"r": map[string]any{"count": int64(30), "name": "bob"},
	}
	cases := []struct {
		name string
		src  string
		want any
	}{
		{"arith lhs gt true", `r.count - 10 > 0`, true},
		{"arith lhs gt false", `r.count - 40 > 0`, false},
		{"literal led lt true", `0 < r.count - 10`, true},
		{"paren arith lhs ge true", `(r.count - 30) >= 0`, true},
		{"eq via arithmetic", `r.count / 3 == 10`, true},
		{"ne true", `r.count * 2 != 0`, true},
		{"le boundary", `r.count - 30 <= 0`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expr := convertBinaryComparison(t, tc.src)
			got, err := evalCollScalar(expr, nil, locals)
			if err != nil {
				t.Fatalf("evaluate %q: %v", tc.src, err)
			}
			if got != tc.want {
				t.Errorf("evaluate %q = %#v, want %#v", tc.src, got, tc.want)
			}
		})
	}
}

// TestBinaryComparisonEval_OuterArgOperand pins that an outer `args.X` operand
// inside an expression-led comparison resolves against the caller args (not the
// lambda element). The arithmetic is parenthesised so the `args.threshold`
// operand is followed by `)` (an ArgRef), not directly by `>` (which the
// identifier-led path would otherwise grab -- see the parser TrailingIdentifier
// quirk pin).
func TestBinaryComparisonEval_OuterArgOperand(t *testing.T) {
	args := map[string]any{"threshold": int64(20)}
	locals := map[string]any{"r": map[string]any{"count": int64(30)}}

	expr := convertBinaryComparison(t, `(r.count - args.threshold) > 0`)
	got, err := evalCollScalar(expr, args, locals)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if got != true {
		t.Errorf("(r.count(30) - args.threshold(20)) > 0 = %#v, want true", got)
	}
}

// TestBinaryComparisonInWhereLambda pins the pin-test surface at the engine
// altitude: a where() chain with an expression-led comparison lambda keeps
// exactly the elements that satisfy it.
func TestBinaryComparisonInWhereLambda(t *testing.T) {
	args := map[string]any{
		"rows": []any{
			map[string]any{"count": int64(3)},
			map[string]any{"count": int64(30)},
			map[string]any{"count": int64(11)},
		},
	}
	expr := convertBinaryComparison(t, `args.rows.where(r => r.count - 10 > 0)`)
	got, err := evalCollScalar(expr, args, nil)
	if err != nil {
		t.Fatalf("evaluate where chain: %v", err)
	}
	rows, ok := got.([]any)
	if !ok {
		t.Fatalf("result = %T, want []any", got)
	}
	if len(rows) != 2 {
		t.Fatalf("kept %d rows, want 2 (count 30 and 11)", len(rows))
	}
}

// TestBinaryComparisonCloneRoundTrips pins that cloneExpressionNode deep-copies
// the node (exhaustive coverage also guards this, but this asserts the operands
// survive the copy).
func TestBinaryComparisonCloneRoundTrips(t *testing.T) {
	orig := &BinaryComparisonExpression{
		Left:     &ArithmeticExpression{Op: "-", Left: &SpecReferenceExpression{Name: "r.count"}, Right: &LiteralValueNode{Value: int64(10)}},
		Operator: OpGt,
		Right:    &LiteralValueNode{Value: int64(0)},
	}
	cloned, ok := cloneExpressionNode(orig).(*BinaryComparisonExpression)
	if !ok {
		t.Fatalf("clone = %T, want *BinaryComparisonExpression", cloneExpressionNode(orig))
	}
	if cloned == orig {
		t.Fatal("clone returned the same pointer, want a copy")
	}
	if cloned.Operator != OpGt {
		t.Errorf("clone operator = %q, want >", cloned.Operator)
	}
	if _, ok := cloned.Left.(*ArithmeticExpression); !ok {
		t.Errorf("clone left = %T, want *ArithmeticExpression", cloned.Left)
	}
}
