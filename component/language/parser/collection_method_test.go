package parser

import (
	"testing"

	"github.com/znasllc-io/memql/component/language/ast"
)

func parseExpr(t *testing.T, src string) ExpressionNode {
	t.Helper()
	tokens, err := NewLexer(src).Tokenize()
	if err != nil {
		t.Fatalf("tokenize %q: %v", src, err)
	}
	expr, err := NewParser(tokens).parseExpression()
	if err != nil {
		t.Fatalf("parse %q: %v", src, err)
	}
	return expr
}

// TestCollectionMethodChainParses locks the worked example from the ADR:
// the full chain parses into nested MethodCallExpr with a lambda arg.
func TestCollectionMethodChainParses(t *testing.T) {
	expr := parseExpr(t, `args.members.where(m => m.role == "admin" && m.active).count()`)

	// Outer node is `.count()` -> MethodCallExpr{Method: count}.
	outer, ok := expr.(*ast.MethodCallExpr)
	if !ok {
		t.Fatalf("outer expr = %T, want *ast.MethodCallExpr", expr)
	}
	if outer.Method != "count" {
		t.Fatalf("outer method = %q, want count", outer.Method)
	}
	if len(outer.Args) != 0 {
		t.Fatalf("count() args = %d, want 0", len(outer.Args))
	}

	// Receiver is `.where(...)`.
	where, ok := outer.Receiver.(*ast.MethodCallExpr)
	if !ok {
		t.Fatalf("count receiver = %T, want *ast.MethodCallExpr", outer.Receiver)
	}
	if where.Method != "where" {
		t.Fatalf("inner method = %q, want where", where.Method)
	}
	if len(where.Args) != 1 {
		t.Fatalf("where() args = %d, want 1", len(where.Args))
	}
	lam, ok := where.Args[0].(*ast.LambdaExpr)
	if !ok {
		t.Fatalf("where arg = %T, want *ast.LambdaExpr", where.Args[0])
	}
	if len(lam.Params) != 1 || lam.Params[0] != "m" {
		t.Fatalf("lambda params = %v, want [m]", lam.Params)
	}

	// where's receiver is the arg reference `args.members`.
	if _, ok := where.Receiver.(*ast.ArgRefExpr); !ok {
		t.Fatalf("where receiver = %T, want *ast.ArgRefExpr", where.Receiver)
	}
}

// TestCollectionMethodSimpleArgs covers non-lambda args (take/skip/contains)
// and a no-receiver-field method.
func TestCollectionMethodSimpleArgs(t *testing.T) {
	for _, src := range []string{
		`args.items.take(5)`,
		`args.items.skip(2)`,
		`args.items.contains("x")`,
		`args.items.first()`,
		`args.items.empty()`,
		`args.items.orderBy(x => x.age).first()`,
		`args.nums.reduce(0, (acc, n) => acc)`,
		`args.items.where(m => m.active).select(m => m.email)`,
	} {
		expr := parseExpr(t, src)
		if _, ok := expr.(*ast.MethodCallExpr); !ok {
			t.Errorf("%q parsed to %T, want *ast.MethodCallExpr", src, expr)
		}
	}
}

// TestParamListLambdaParses confirms `(acc, x) => ...` param-list lambdas.
func TestParamListLambdaParses(t *testing.T) {
	expr := parseExpr(t, `args.nums.reduce(0, (acc, n) => acc)`)
	outer := expr.(*ast.MethodCallExpr)
	if len(outer.Args) != 2 {
		t.Fatalf("reduce args = %d, want 2", len(outer.Args))
	}
	lam, ok := outer.Args[1].(*ast.LambdaExpr)
	if !ok {
		t.Fatalf("reduce arg1 = %T, want lambda", outer.Args[1])
	}
	if len(lam.Params) != 2 || lam.Params[0] != "acc" || lam.Params[1] != "n" {
		t.Fatalf("reduce lambda params = %v, want [acc n]", lam.Params)
	}
}
