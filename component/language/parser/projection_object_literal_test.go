package parser

import (
	"testing"

	"github.com/znasllc-io/memql/component/language/ast"
)

// projection_object_literal_test.go pins the #2542 item-3 parse grammar: an
// object literal that is a `select(g => {...})` projection body parses its
// values as FULL expressions (arithmetic + scope paths become evaluable
// ExpressionNodes), while an object literal OUTSIDE a collection-method call
// keeps the JSON-value grammar (so arg-time / spec / shape object literals are
// unaffected and infix arithmetic stays out of those positions).

func TestProjectionObjectLiteral_ArithmeticValueParses(t *testing.T) {
	// The pre-#2542 JSON-value grammar stopped at the `/` with
	// `expected '}', got "/"`. It must now parse.
	expr := parseExpr(t, `rows.groupBy(g => g.w).select(g => {worker: g.key, acc: g.a.count() / g.b.count()})`)

	// Outer `.select(...)`.
	sel, ok := expr.(*ast.MethodCallExpr)
	if !ok || sel.Method != "select" {
		t.Fatalf("outer = %T (%v), want select MethodCallExpr", expr, expr)
	}
	lam, ok := sel.Args[0].(*ast.LambdaExpr)
	if !ok {
		t.Fatalf("select arg = %T, want *ast.LambdaExpr", sel.Args[0])
	}
	lit, ok := lam.Body.(*ast.LiteralExpr)
	if !ok {
		t.Fatalf("lambda body = %T, want *ast.LiteralExpr (object literal)", lam.Body)
	}
	obj, ok := lit.Value.(map[string]any)
	if !ok {
		t.Fatalf("literal value = %T, want map[string]any", lit.Value)
	}
	// The arithmetic value is an ArithmeticExpr (evaluable), not a raw string.
	if _, ok := obj["acc"].(*ast.ArithmeticExpr); !ok {
		t.Errorf("projection value acc = %T, want *ast.ArithmeticExpr", obj["acc"])
	}
	// The scope-path value is an ExpressionNode (evaluable), not the raw
	// string "g.key".
	if _, isStr := obj["worker"].(string); isStr {
		t.Errorf("projection value worker = raw string, want an evaluable ExpressionNode")
	}
	if _, ok := obj["worker"].(ast.ExpressionNode); !ok {
		t.Errorf("projection value worker = %T, want an ast.ExpressionNode", obj["worker"])
	}
}

// A plain object literal OUTSIDE any collection-method arg keeps the JSON-value
// grammar: arithmetic in a value is still a parse error (the containment that
// keeps infix arithmetic out of arg-time / spec / shape positions).
func TestObjectLiteral_ArithmeticRejectedOutsideCollectionContext(t *testing.T) {
	tokens, err := NewLexer(`{acc: a / b}`).Tokenize()
	if err != nil {
		t.Fatalf("tokenize: %v", err)
	}
	if _, err := NewParser(tokens).parseExpression(); err == nil {
		t.Fatalf("expected a parse error for arithmetic in a non-projection object literal value")
	}
}
