package memql

import (
	"strings"
	"testing"

	parser "github.com/znasllc-io/memql/component/language/parser"
)

// arithmetic_test.go covers the #2316 in-memory arithmetic surface end to
// end through the engine: lambda-body arithmetic via the collection
// evaluator, int vs float result typing, division/modulo by zero errors, and
// the load-time scope gate (admitted in logic/collection lambdas, rejected in
// specs / query filters).

// evalArith parses + converts (default converter, i.e. logic/lambda admitted
// is via WithCollectionMethods) and evaluates a bare arithmetic expression
// over the given args, exercising evalCollScalar directly.
func evalArith(t *testing.T, src string, args map[string]any) (any, error) {
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

func mustEvalArith(t *testing.T, src string, args map[string]any) any {
	t.Helper()
	v, err := evalArith(t, src, args)
	if err != nil {
		t.Fatalf("eval %q: %v", src, err)
	}
	return v
}

func TestArithmeticReduceSum(t *testing.T) {
	args := map[string]any{"nums": []any{int64(1), int64(2), int64(3), int64(4)}}
	got := evalChain(t, "args.nums.reduce(0, (acc, n) => acc + n)", args)
	if got != int64(10) {
		t.Fatalf("reduce sum = %v (%T), want int64(10)", got, got)
	}
}

func TestArithmeticSelectProduct(t *testing.T) {
	args := map[string]any{
		"members": []any{
			map[string]any{"a": int64(2), "b": int64(3)},
			map[string]any{"a": int64(4), "b": int64(5)},
		},
	}
	got := evalChain(t, "args.members.select(m => m.a * m.b)", args)
	out, ok := got.([]any)
	if !ok || len(out) != 2 {
		t.Fatalf("select = %#v, want 2-element slice", got)
	}
	if out[0] != int64(6) || out[1] != int64(20) {
		t.Fatalf("select products = %v, want [6 20]", out)
	}
}

func TestArithmeticIntVsFloat(t *testing.T) {
	// Integer operands -> int64 (integer division).
	if got := mustEvalArith(t, "7 / 2", nil); got != int64(3) {
		t.Fatalf("7 / 2 = %v (%T), want int64(3)", got, got)
	}
	// Any float operand -> float64.
	if got := mustEvalArith(t, "7.0 / 2", nil); got != float64(3.5) {
		t.Fatalf("7.0 / 2 = %v (%T), want float64(3.5)", got, got)
	}
	if got := mustEvalArith(t, "2 + 3 * 4", nil); got != int64(14) {
		t.Fatalf("2 + 3 * 4 = %v, want int64(14)", got)
	}
	if got := mustEvalArith(t, "10 % 3", nil); got != int64(1) {
		t.Fatalf("10 %% 3 = %v, want int64(1)", got)
	}
	if got := mustEvalArith(t, "-5 + 8", nil); got != int64(3) {
		t.Fatalf("-5 + 8 = %v, want int64(3)", got)
	}
}

func TestArithmeticDivByZeroErrors(t *testing.T) {
	if _, err := evalArith(t, "1 / 0", nil); err == nil || !strings.Contains(err.Error(), "division by zero") {
		t.Fatalf("1 / 0 err = %v, want division by zero", err)
	}
	if _, err := evalArith(t, "1 % 0", nil); err == nil || !strings.Contains(err.Error(), "modulo by zero") {
		t.Fatalf("1 %% 0 err = %v, want modulo by zero", err)
	}
	if _, err := evalArith(t, "1.0 % 2.0", nil); err == nil || !strings.Contains(err.Error(), "integer operands") {
		t.Fatalf("float %% err = %v, want integer-operands error", err)
	}
}

func TestArithmeticRejectedInSpecsAndQueryFilters(t *testing.T) {
	// The DEFAULT converter (what specs + query filters use) must reject an
	// ArithmeticExpr at load with the #2316 scope error.
	pexpr, err := parser.ParseExpression("qty * price")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	_, err = NewASTConverter().ConvertExpression(pexpr)
	if err == nil {
		t.Fatal("default converter accepted arithmetic; expected scope rejection")
	}
	if !strings.Contains(err.Error(), "in-memory") || !strings.Contains(err.Error(), "#2316") {
		t.Fatalf("scope error = %q, want the in-memory / #2316 message", err.Error())
	}
}

func TestArithmeticAdmittedInLogicConverter(t *testing.T) {
	pexpr, err := parser.ParseExpression("qty * price")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got, err := NewASTConverter(WithCollectionMethods()).ConvertExpression(pexpr)
	if err != nil {
		t.Fatalf("logic converter rejected arithmetic: %v", err)
	}
	if _, ok := got.(*ArithmeticExpression); !ok {
		t.Fatalf("converted to %T, want *ArithmeticExpression", got)
	}
}
