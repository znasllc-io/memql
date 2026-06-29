package parser

import (
	"testing"

	"github.com/znasllc-io/memql/component/language/ast"
)

// arithmetic_test.go covers the #2316 arithmetic precedence layer in the
// expression grammar: `+ - * / %` with Go precedence (`* / %` tighter than
// `+ -`), left-associativity, parenthesised grouping, unary minus, and use as
// a lambda body. Arithmetic is parsed everywhere; scope (in-memory only) is
// enforced downstream by the engine AST converter.

func arith(t *testing.T, src string) *ast.ArithmeticExpr {
	t.Helper()
	expr, err := ParseExpression(src)
	if err != nil {
		t.Fatalf("ParseExpression(%q): %v", src, err)
	}
	a, ok := expr.(*ast.ArithmeticExpr)
	if !ok {
		t.Fatalf("ParseExpression(%q) = %T, want *ast.ArithmeticExpr", src, expr)
	}
	return a
}

func litI(t *testing.T, n ast.ExpressionNode, want int64) {
	t.Helper()
	le, ok := n.(*ast.LiteralExpr)
	if !ok {
		t.Fatalf("node = %T, want *ast.LiteralExpr", n)
	}
	got, ok := le.Value.(int64)
	if !ok || got != want {
		t.Fatalf("literal = %v (%T), want int64(%d)", le.Value, le.Value, want)
	}
}

func TestArithmeticPrecedence_MulBindsTighter(t *testing.T) {
	// `2 + 3 * 4` must parse as `2 + (3 * 4)`.
	top := arith(t, "2 + 3 * 4")
	if top.Op != "+" {
		t.Fatalf("top op = %q, want +", top.Op)
	}
	litI(t, top.Left, 2)
	rhs, ok := top.Right.(*ast.ArithmeticExpr)
	if !ok {
		t.Fatalf("rhs = %T, want *ast.ArithmeticExpr", top.Right)
	}
	if rhs.Op != "*" {
		t.Fatalf("rhs op = %q, want *", rhs.Op)
	}
	litI(t, rhs.Left, 3)
	litI(t, rhs.Right, 4)
}

func TestArithmeticPrecedence_SubtractionLeftAssociative(t *testing.T) {
	// `a - b - c` must parse as `(a - b) - c`.
	top := arith(t, "a - b - c")
	if top.Op != "-" {
		t.Fatalf("top op = %q, want -", top.Op)
	}
	left, ok := top.Left.(*ast.ArithmeticExpr)
	if !ok {
		t.Fatalf("left = %T, want nested *ast.ArithmeticExpr (left-assoc)", top.Left)
	}
	if left.Op != "-" {
		t.Fatalf("left op = %q, want -", left.Op)
	}
	if _, ok := top.Right.(*ast.SpecReferenceExpr); !ok {
		t.Fatalf("right = %T, want *ast.SpecReferenceExpr (c)", top.Right)
	}
}

func TestArithmeticPrecedence_ParenGroupsOverrideMul(t *testing.T) {
	// `(a + b) * c` -- the grouping forces addition under multiplication.
	top := arith(t, "(a + b) * c")
	if top.Op != "*" {
		t.Fatalf("top op = %q, want *", top.Op)
	}
	left, ok := top.Left.(*ast.ArithmeticExpr)
	if !ok {
		t.Fatalf("left = %T, want grouped *ast.ArithmeticExpr", top.Left)
	}
	if left.Op != "+" {
		t.Fatalf("grouped op = %q, want +", left.Op)
	}
}

func TestArithmeticUnaryMinus(t *testing.T) {
	// `-x` folds to `0 - x`.
	top := arith(t, "-x")
	if top.Op != "-" {
		t.Fatalf("top op = %q, want -", top.Op)
	}
	litI(t, top.Left, 0)
	if _, ok := top.Right.(*ast.SpecReferenceExpr); !ok {
		t.Fatalf("right = %T, want *ast.SpecReferenceExpr (x)", top.Right)
	}
}

func TestArithmeticNegativeLiteralStillScans(t *testing.T) {
	// `-5` must remain a single numeric literal, NOT a unary-minus tree.
	expr, err := ParseExpression("-5")
	if err != nil {
		t.Fatalf("ParseExpression(-5): %v", err)
	}
	le, ok := expr.(*ast.LiteralExpr)
	if !ok {
		t.Fatalf("-5 = %T, want *ast.LiteralExpr (negative literal preserved)", expr)
	}
	if le.Value.(int64) != -5 {
		t.Fatalf("-5 value = %v, want -5", le.Value)
	}
}

func TestArithmeticArgRefOperand(t *testing.T) {
	// `args.qty * args.price` -- both operands resolve to ArgRefExpr.
	top := arith(t, "args.qty * args.price")
	if top.Op != "*" {
		t.Fatalf("top op = %q, want *", top.Op)
	}
	l, ok := top.Left.(*ast.ArgRefExpr)
	if !ok || l.Path != "qty" {
		t.Fatalf("left = %#v, want ArgRefExpr{qty}", top.Left)
	}
	r, ok := top.Right.(*ast.ArgRefExpr)
	if !ok || r.Path != "price" {
		t.Fatalf("right = %#v, want ArgRefExpr{price}", top.Right)
	}
}

func TestArithmeticAsLambdaBody(t *testing.T) {
	// `(acc, x) => acc + x` -- the lambda body is an ArithmeticExpr.
	expr, err := ParseExpression("args.nums.reduce(0, (acc, x) => acc + x)")
	if err != nil {
		t.Fatalf("parse reduce: %v", err)
	}
	mc, ok := expr.(*ast.MethodCallExpr)
	if !ok {
		t.Fatalf("expr = %T, want *ast.MethodCallExpr", expr)
	}
	if len(mc.Args) != 2 {
		t.Fatalf("reduce args = %d, want 2", len(mc.Args))
	}
	lam, ok := mc.Args[1].(*ast.LambdaExpr)
	if !ok {
		t.Fatalf("arg1 = %T, want *ast.LambdaExpr", mc.Args[1])
	}
	if _, ok := lam.Body.(*ast.ArithmeticExpr); !ok {
		t.Fatalf("lambda body = %T, want *ast.ArithmeticExpr", lam.Body)
	}
}
