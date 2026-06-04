package parser

import (
	"testing"
)

func parseWhenExpr(t *testing.T, src string) ExpressionNode {
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

// TestWhenGuardParses locks in #975: `when(args.x) { <expr> }` parses to a
// ConditionalFilterExpr carrying the guard arg path + the guarded expression,
// reusing the `?.` drop machinery.
func TestWhenGuardParses(t *testing.T) {
	expr := parseWhenExpr(t, `when(args.groupId) { args.groupId in payload.groupIds }`)
	cond, ok := expr.(*ConditionalFilterExpr)
	if !ok {
		t.Fatalf("expected *ConditionalFilterExpr, got %T", expr)
	}
	if cond.ArgPath != "groupId" {
		t.Fatalf("expected ArgPath \"groupId\" (args. prefix stripped), got %q", cond.ArgPath)
	}
	// The guarded block holds the in->has desugared membership.
	inner, ok := cond.Filter.(*ComparisonExpr)
	if !ok {
		t.Fatalf("expected the guarded block to be a *ComparisonExpr, got %T", cond.Filter)
	}
	if inner.Operator != OpHas || inner.Field.Raw != "payload.groupIds" {
		t.Fatalf("guarded membership: op=%v field=%q", inner.Operator, inner.Field.Raw)
	}
}

// TestWhenGuardInsideOr proves the guard composes inside `||` -- the parser
// builds a LogicalExpression(OR) over the guard and another term, which the
// engine's arg-expansion later collapses when the guard arg is absent.
func TestWhenGuardInsideOr(t *testing.T) {
	expr := parseWhenExpr(t, `when(args.tag) { payload.tag == args.tag } || payload.pinned == true`)
	logical, ok := expr.(*LogicalExpr)
	if !ok || logical.Op != LogicalOr {
		t.Fatalf("expected a top-level OR, got %T", expr)
	}
	if _, ok := logical.Left.(*ConditionalFilterExpr); !ok {
		t.Fatalf("expected the LHS to be the when-guard ConditionalFilterExpr, got %T", logical.Left)
	}
}

// TestWhenGuardRejectsNonArgGuard requires the guard to be an args.<field> ref.
func TestWhenGuardRejectsNonArgGuard(t *testing.T) {
	tokens, err := NewLexer(`when(payload.x) { payload.y == "z" }`).Tokenize()
	if err != nil {
		t.Fatalf("tokenize: %v", err)
	}
	if _, err := NewParser(tokens).parseExpression(); err == nil {
		t.Fatalf("expected when() with a non-args guard to be rejected")
	}
}
