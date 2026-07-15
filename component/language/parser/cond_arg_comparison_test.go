package parser

import (
	"testing"

	"github.com/znasllc-io/memql/component/language/ast"
)

// cond_arg_comparison_test.go covers the wave-3 #2542 item-2 grammar extension:
// parseExpressionArg (the restricted builtin-arg grammar shared by cond /
// coalesce / the date builtins) now consumes ONE relational comparison
// (< <= > >=) beyond a bare primary, so a cond predicate that is a comparison
// over a collection-chain aggregate (`cond(x.count(...) > 0, ...)`) parses to a
// CondExpr with an expression-led *BinaryComparisonExpr condition -- the shape
// the compiler serializer + the cond-predicate / single-return evaluators
// already handle. Before this the arg grammar stopped at the operator and
// reported "cond() requires three arguments". The change is additive: the four
// relational operators previously always errored here; == / != keep their
// established EqExpr shape; identifier-led comparisons are still folded into a
// Field-led ComparisonExpr one level down and are unaffected.

func condArg(t *testing.T, src string) *ast.CondExpr {
	t.Helper()
	expr, err := ParseExpression(src)
	if err != nil {
		t.Fatalf("ParseExpression(%q): %v", src, err)
	}
	c, ok := expr.(*ast.CondExpr)
	if !ok {
		t.Fatalf("ParseExpression(%q) = %T, want *ast.CondExpr", src, expr)
	}
	return c
}

// A comparison over a collection-chain aggregate as the cond predicate parses,
// with the predicate an expression-led *BinaryComparisonExpr, for every
// relational operator.
func TestCondArg_RelationalComparisonOverChain(t *testing.T) {
	for _, op := range []string{">", ">=", "<", "<="} {
		src := `cond(args.members.count(m => m.active) ` + op + ` 2, "some", "none")`
		cond := condArg(t, src)
		bc, ok := cond.Condition.(*ast.BinaryComparisonExpr)
		if !ok {
			t.Fatalf("op %q: cond predicate = %T, want *ast.BinaryComparisonExpr", op, cond.Condition)
		}
		if string(bc.Operator) != op {
			t.Errorf("op %q: predicate operator = %q, want %q", op, bc.Operator, op)
		}
		if _, ok := bc.Left.(*ast.MethodCallExpr); !ok {
			t.Errorf("op %q: predicate left = %T, want *ast.MethodCallExpr (the chain)", op, bc.Left)
		}
	}
}

// A comparison over a call/literal result (non-identifier LHS) also reaches the
// new relational branch and produces a *BinaryComparisonExpr.
func TestCondArg_RelationalOverCallResult(t *testing.T) {
	cond := condArg(t, `cond(first(rows).age >= 18, "adult", "minor")`)
	if _, ok := cond.Condition.(*ast.BinaryComparisonExpr); !ok {
		t.Fatalf("predicate = %T, want *ast.BinaryComparisonExpr", cond.Condition)
	}
}

// BACK-COMPAT: an identifier-led comparison is consumed one level down and keeps
// its Field-led *ComparisonExpr shape (the relational branch never fires -- the
// operator is already consumed before parseExpressionArg inspects it).
func TestCondArg_IdentifierLedComparisonUnchanged(t *testing.T) {
	cond := condArg(t, `cond(status > 3, "high", "low")`)
	if _, ok := cond.Condition.(*ast.ComparisonExpr); !ok {
		t.Fatalf("identifier-led predicate = %T, want *ast.ComparisonExpr (unchanged)", cond.Condition)
	}
}

// BACK-COMPAT: the == / != equality forms over a non-identifier LHS keep their
// established EqExpr / NotExpr(EqExpr) shape (#2407 / S9).
func TestCondArg_EqualityShapeUnchanged(t *testing.T) {
	eq := condArg(t, `cond(first(rows).status == "active", "a", "b")`)
	if _, ok := eq.Condition.(*ast.EqExpr); !ok {
		t.Fatalf("== predicate = %T, want *ast.EqExpr (unchanged)", eq.Condition)
	}
	ne := condArg(t, `cond(first(rows).status != "active", "a", "b")`)
	not, ok := ne.Condition.(*ast.NotExpr)
	if !ok {
		t.Fatalf("!= predicate = %T, want *ast.NotExpr wrapping EqExpr (unchanged)", ne.Condition)
	}
	if _, ok := not.Target.(*ast.EqExpr); !ok {
		t.Errorf("!= predicate target = %T, want *ast.EqExpr", not.Target)
	}
}

// A connective inside a cond arg is still rejected (nest cond / use an if step).
func TestCondArg_ConnectiveStillRejected(t *testing.T) {
	_, err := ParseExpression(`cond(a.count() > 0 && b.count() > 0, "x", "y")`)
	if err == nil {
		t.Fatalf("expected a parse error for a connective inside a cond predicate")
	}
}
