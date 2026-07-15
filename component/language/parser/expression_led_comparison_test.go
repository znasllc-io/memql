package parser

import (
	"testing"

	"github.com/znasllc-io/memql/component/language/ast"
)

// expression_led_comparison_test.go covers the #2542 item-5-residual comparison
// precedence layer: a comparison whose LEFT side is not a bare identifier --
// arithmetic (`r.count - 10 > 0`), a literal (`0 < r.count`), a parenthesised
// group, or a call result -- parses to an expression-led *BinaryComparisonExpr.
// The level sits between the logical (&&/||) levels and the additive level, so
// arithmetic binds tighter than a comparison. Identifier-led comparisons are
// still consumed one level down (in parseIdentifierExpression) and keep their
// Field-led *ComparisonExpr shape unchanged; back-compat regressions are pinned
// below.

func binCmp(t *testing.T, src string) *ast.BinaryComparisonExpr {
	t.Helper()
	expr, err := ParseExpression(src)
	if err != nil {
		t.Fatalf("ParseExpression(%q): %v", src, err)
	}
	c, ok := expr.(*ast.BinaryComparisonExpr)
	if !ok {
		t.Fatalf("ParseExpression(%q) = %T, want *ast.BinaryComparisonExpr", src, expr)
	}
	return c
}

// TestExpressionLedComparison_ArithmeticLHS pins that an arithmetic left side
// binds into the comparison as `(a - 10) > 0`, not the reverse.
func TestExpressionLedComparison_ArithmeticLHS(t *testing.T) {
	c := binCmp(t, "count - 10 > 0")
	if c.Operator != ast.OpGt {
		t.Fatalf("operator = %q, want >", c.Operator)
	}
	left, ok := c.Left.(*ast.ArithmeticExpr)
	if !ok {
		t.Fatalf("left = %T, want *ast.ArithmeticExpr", c.Left)
	}
	if left.Op != "-" {
		t.Fatalf("left op = %q, want -", left.Op)
	}
	if _, ok := c.Right.(*ast.LiteralExpr); !ok {
		t.Fatalf("right = %T, want *ast.LiteralExpr", c.Right)
	}
}

// TestExpressionLedComparison_ParenthesisedLHS pins the explicitly grouped
// arithmetic LHS `(count - 10) > 0`.
func TestExpressionLedComparison_ParenthesisedLHS(t *testing.T) {
	c := binCmp(t, "(count - 10) > 0")
	if c.Operator != ast.OpGt {
		t.Fatalf("operator = %q, want >", c.Operator)
	}
	if _, ok := c.Left.(*ast.ArithmeticExpr); !ok {
		t.Fatalf("left = %T, want *ast.ArithmeticExpr", c.Left)
	}
}

// TestExpressionLedComparison_LiteralLED pins the literal-led comparison
// `0 < count - 10` -- the whole comparison routes through the new level
// (literal LHS is never identifier-led), so the RHS arithmetic parses cleanly.
func TestExpressionLedComparison_LiteralLED(t *testing.T) {
	c := binCmp(t, "0 < count - 10")
	if c.Operator != ast.OpLt {
		t.Fatalf("operator = %q, want <", c.Operator)
	}
	if _, ok := c.Left.(*ast.LiteralExpr); !ok {
		t.Fatalf("left = %T, want *ast.LiteralExpr", c.Left)
	}
	right, ok := c.Right.(*ast.ArithmeticExpr)
	if !ok {
		t.Fatalf("right = %T, want *ast.ArithmeticExpr", c.Right)
	}
	if right.Op != "-" {
		t.Fatalf("right op = %q, want -", right.Op)
	}
}

// TestExpressionLedComparison_CallLED pins that a call-result LHS
// (`foo() > 5`) -- which parseIdentifierExpression returns before checking for
// a comparison operator -- now parses through the new level.
func TestExpressionLedComparison_CallLED(t *testing.T) {
	c := binCmp(t, "foo() > 5")
	if c.Operator != ast.OpGt {
		t.Fatalf("operator = %q, want >", c.Operator)
	}
	if _, ok := c.Left.(*ast.FunctionCallExpr); !ok {
		t.Fatalf("left = %T, want *ast.FunctionCallExpr", c.Left)
	}
}

// TestExpressionLedComparison_ArithmeticBindsTighter is the precedence
// regression table: arithmetic always binds tighter than a comparison, so the
// whole arithmetic subtree is the comparison's left operand. Each LHS ends in a
// non-identifier token (a literal / a grouped expr), which is the shape that
// routes the comparison operator to the new level -- see
// TestExpressionLedComparison_TrailingIdentifierQuirk for the one shape that
// does not.
func TestExpressionLedComparison_ArithmeticBindsTighter(t *testing.T) {
	cases := []struct {
		src    string
		op     ast.ComparisonOperator
		leftOp string // arithmetic op on the LHS
	}{
		{"a * 2 == b", ast.OpEq, "*"},
		{"a + 1 >= 0", ast.OpGe, "+"},
		{"total / 2 <= 100", ast.OpLe, "/"},
		{"a % 2 != 0", ast.OpNe, "%"},
	}
	for _, tc := range cases {
		t.Run(tc.src, func(t *testing.T) {
			c := binCmp(t, tc.src)
			if c.Operator != tc.op {
				t.Fatalf("operator = %q, want %q", c.Operator, tc.op)
			}
			left, ok := c.Left.(*ast.ArithmeticExpr)
			if !ok {
				t.Fatalf("left = %T, want *ast.ArithmeticExpr", c.Left)
			}
			if left.Op != tc.leftOp {
				t.Fatalf("left op = %q, want %q", left.Op, tc.leftOp)
			}
		})
	}
}

// TestExpressionLedComparison_TrailingIdentifierQuirk documents a PRE-EXISTING
// limitation this change does NOT alter: when an arithmetic operand is a bare
// identifier sitting DIRECTLY before a comparison operator (`a + b > 0`),
// parseIdentifierExpression consumes `b > 0` as its own identifier-led
// comparison first, so the tree is `a + (b > 0)` rather than `(a + b) > 0`.
// This is the same shape the pre-#2542 grammar produced (the identifier-led
// comparison predates the arithmetic layer); closing the item-5 gap did not
// touch it. The item-5 forms (`r.count - 10 > 0`, `0 < r.count`) end the
// arithmetic in a literal / group and are unaffected. Pinned so the quirk is
// not mistaken for a regression, and so a future full generalisation has an
// explicit before-state to flip.
func TestExpressionLedComparison_TrailingIdentifierQuirk(t *testing.T) {
	expr, err := ParseExpression("a + b > 0")
	if err != nil {
		t.Fatalf("ParseExpression: %v", err)
	}
	arith, ok := expr.(*ast.ArithmeticExpr)
	if !ok {
		t.Fatalf("`a + b > 0` = %T, want *ast.ArithmeticExpr (pre-existing identifier-led grab); if this now parses as a clean comparison the grammar was generalised -- update this pin", expr)
	}
	if _, ok := arith.Right.(*ast.ComparisonExpr); !ok {
		t.Fatalf("right operand = %T, want the identifier-led *ast.ComparisonExpr (`b > 0`)", arith.Right)
	}
}

// TestExpressionLedComparison_ComposesWithLogical pins that an expression-led
// comparison composes with && / || at the correct precedence: the comparison
// binds tighter than the connective, so `a - 1 > 0 && b < 5` is
// `(a - 1 > 0) && (b < 5)`.
func TestExpressionLedComparison_ComposesWithLogical(t *testing.T) {
	expr, err := ParseExpression("a - 1 > 0 && b < 5")
	if err != nil {
		t.Fatalf("ParseExpression: %v", err)
	}
	top, ok := expr.(*ast.LogicalExpr)
	if !ok {
		t.Fatalf("top = %T, want *ast.LogicalExpr", expr)
	}
	if top.Op != ast.LogicalAnd {
		t.Fatalf("top op = %q, want AND", top.Op)
	}
	// Left side is the expression-led comparison (a - 1 > 0).
	if _, ok := top.Left.(*ast.BinaryComparisonExpr); !ok {
		t.Fatalf("left = %T, want *ast.BinaryComparisonExpr", top.Left)
	}
	// Right side is an identifier-led comparison (b < 5) -> Field-led.
	if _, ok := top.Right.(*ast.ComparisonExpr); !ok {
		t.Fatalf("right = %T, want *ast.ComparisonExpr", top.Right)
	}
}

// TestExpressionLedComparison_LambdaBody pins the pin-test surface: a where()
// lambda whose body is an arithmetic-LHS comparison parses, and the lambda body
// is a *BinaryComparisonExpr.
func TestExpressionLedComparison_LambdaBody(t *testing.T) {
	expr, err := ParseExpression("rows.where(r => r.count - 10 > 0)")
	if err != nil {
		t.Fatalf("ParseExpression: %v", err)
	}
	call, ok := expr.(*ast.MethodCallExpr)
	if !ok {
		t.Fatalf("expr = %T, want *ast.MethodCallExpr", expr)
	}
	if len(call.Args) != 1 {
		t.Fatalf("where() args = %d, want 1", len(call.Args))
	}
	lam, ok := call.Args[0].(*ast.LambdaExpr)
	if !ok {
		t.Fatalf("arg = %T, want *ast.LambdaExpr", call.Args[0])
	}
	if _, ok := lam.Body.(*ast.BinaryComparisonExpr); !ok {
		t.Fatalf("lambda body = %T, want *ast.BinaryComparisonExpr", lam.Body)
	}
}

// --- Back-compat regressions: identifier-led shapes are unchanged ---

// TestIdentifierLedComparison_ShapeUnchanged pins that a bare identifier-led
// comparison still produces the Field-led *ComparisonExpr (never the new
// expression-led node), so existing packs parse byte-identically.
func TestIdentifierLedComparison_ShapeUnchanged(t *testing.T) {
	cases := []string{
		"count > 10",
		"payload.status == \"open\"",
		"m.role != \"admin\"",
		"score >= 5",
	}
	for _, src := range cases {
		t.Run(src, func(t *testing.T) {
			expr, err := ParseExpression(src)
			if err != nil {
				t.Fatalf("ParseExpression(%q): %v", src, err)
			}
			if _, ok := expr.(*ast.ComparisonExpr); !ok {
				t.Fatalf("%q = %T, want *ast.ComparisonExpr (identifier-led shape unchanged)", src, expr)
			}
		})
	}
}

// TestIdentifierLedNilComparison_StillDesugars pins that `x == nil` keeps its
// identifier-led OpMissing desugaring -- the new comparison level never sees
// it because parseIdentifierExpression consumes the operator first.
func TestIdentifierLedNilComparison_StillDesugars(t *testing.T) {
	expr, err := ParseExpression("payload.deletedAt == nil")
	if err != nil {
		t.Fatalf("ParseExpression: %v", err)
	}
	c, ok := expr.(*ast.ComparisonExpr)
	if !ok {
		t.Fatalf("expr = %T, want *ast.ComparisonExpr", expr)
	}
	if c.Operator != ast.OpMissing {
		t.Fatalf("operator = %q, want == nil (OpMissing)", c.Operator)
	}
}

// TestIdentifierLedMembership_Unchanged pins that keyword membership operators
// (`in` / `has` / `not in`) stay identifier-led and are untouched by the new
// symbol-only comparison level.
func TestIdentifierLedMembership_Unchanged(t *testing.T) {
	cases := []struct {
		src string
		op  ast.ComparisonOperator
	}{
		{"kind in [\"a\", \"b\"]", ast.OpIn},
		{"tags has \"urgent\"", ast.OpHas},
	}
	for _, tc := range cases {
		t.Run(tc.src, func(t *testing.T) {
			expr, err := ParseExpression(tc.src)
			if err != nil {
				t.Fatalf("ParseExpression(%q): %v", tc.src, err)
			}
			c, ok := expr.(*ast.ComparisonExpr)
			if !ok {
				t.Fatalf("%q = %T, want *ast.ComparisonExpr", tc.src, expr)
			}
			if c.Operator != tc.op {
				t.Fatalf("operator = %q, want %q", c.Operator, tc.op)
			}
		})
	}
}

// TestExpressionLedComparison_ReparsesSerializerForm guards that the fully
// parenthesised operator form the compiler serializer emits
// (`((count - 10) > 0)`) re-parses to the same expression-led comparison shape,
// so the serialize -> re-parse round trip is stable.
func TestExpressionLedComparison_ReparsesSerializerForm(t *testing.T) {
	c := binCmp(t, "((count - 10) > 0)")
	if c.Operator != ast.OpGt {
		t.Fatalf("operator = %q, want >", c.Operator)
	}
	if _, ok := c.Left.(*ast.ArithmeticExpr); !ok {
		t.Fatalf("left = %T, want *ast.ArithmeticExpr", c.Left)
	}
}
