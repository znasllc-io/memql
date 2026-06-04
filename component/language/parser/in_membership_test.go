package parser

import (
	"reflect"
	"testing"
)

func parseFilterExpr(t *testing.T, src string) ExpressionNode {
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

// TestInScalarInCollectionField locks in #976: `<scalar> in payload.<collection>`
// is the canonical membership form and desugars to the existing array-contains
// (`payload.<collection> has <scalar>`) codegen -- it must parse to exactly the
// AST the reversed `has` form produces.
func TestInScalarInCollectionField(t *testing.T) {
	got := parseFilterExpr(t, `args.groupId in payload.groupIds`)
	want := parseFilterExpr(t, `payload.groupIds has args.groupId`)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("`in` over a collection field should desugar to the `has` AST\n got:  %#v\n want: %#v", got, want)
	}
	cmp, ok := got.(*ComparisonExpr)
	if !ok || cmp.Operator != OpHas || cmp.Field.Raw != "payload.groupIds" {
		t.Fatalf("expected OpHas over payload.groupIds, got %#v", got)
	}
}

// TestInScalarInListLiteral keeps the list-literal membership form on OpIn.
func TestInScalarInListLiteral(t *testing.T) {
	expr := parseFilterExpr(t, `payload.kind in ["meeting", "reminder"]`)
	cmp, ok := expr.(*ComparisonExpr)
	if !ok {
		t.Fatalf("expected *ComparisonExpr, got %T", expr)
	}
	if cmp.Operator != OpIn {
		t.Fatalf("expected list-literal membership to stay OpIn, got %v", cmp.Operator)
	}
	if cmp.Field.Raw != "payload.kind" {
		t.Fatalf("expected payload.kind as Field, got %q", cmp.Field.Raw)
	}
	vals, ok := cmp.Value.([]any)
	if !ok || len(vals) != 2 {
		t.Fatalf("expected a 2-element list literal value, got %T %v", cmp.Value, cmp.Value)
	}
}

// TestNegatedInMembership covers `!(x in y)` -- negation wraps the desugared
// membership in a NotExpr.
func TestNegatedInMembership(t *testing.T) {
	expr := parseFilterExpr(t, `!(args.groupId in payload.groupIds)`)
	not, ok := expr.(*NotExpr)
	if !ok {
		t.Fatalf("expected *NotExpr, got %T", expr)
	}
	cmp, ok := not.Target.(*ComparisonExpr)
	if !ok {
		t.Fatalf("expected NotExpr.Target to be *ComparisonExpr, got %T", not.Target)
	}
	if cmp.Operator != OpHas || cmp.Field.Raw != "payload.groupIds" {
		t.Fatalf("expected negated membership over payload.groupIds (OpHas), got op=%v field=%q", cmp.Operator, cmp.Field.Raw)
	}
}

// TestHasStillParses guards the transitional state: `has` still parses (its
// tree-wide migration to `in` + rejection is the codemod #977).
func TestHasStillParses(t *testing.T) {
	expr := parseFilterExpr(t, `payload.groupIds has args.groupId`)
	cmp, ok := expr.(*ComparisonExpr)
	if !ok {
		t.Fatalf("expected *ComparisonExpr, got %T", expr)
	}
	if cmp.Operator != OpHas || cmp.Field.Raw != "payload.groupIds" {
		t.Fatalf("unexpected has parse: op=%v field=%q", cmp.Operator, cmp.Field.Raw)
	}
}
