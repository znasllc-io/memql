package memql

import (
	"testing"
)

// findArgRefValue walks the expression tree and returns the Value
// of the first ComparisonExpression whose Field matches `field`.
func findArgRefValue(e ExpressionNode, field string) any {
	switch n := e.(type) {
	case *LogicalExpression:
		if v := findArgRefValue(n.Left, field); v != nil {
			return v
		}
		return findArgRefValue(n.Right, field)
	case *ComparisonExpression:
		if len(n.Field.Parts) >= 2 && n.Field.Parts[0] == "payload" && n.Field.Parts[1] == field {
			return n.Value
		}
	}
	return nil
}

func parseFilterToExpr(t *testing.T, query string) ExpressionNode {
	t.Helper()
	tokens, err := tokenize(query)
	if err != nil {
		t.Fatalf("tokenize: %v", err)
	}
	p := newParser(tokens, nil)
	expr, err := p.parse()
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return expr
}

// TestArgsReferenceProducesArgReference pins the author-facing
// `args.X` form: the parser emits the same *ArgReference AST node
// that the legacy `ctx.X` form produces. This is the foundation for
// F.3 of the ctx-envelope purge -- it lets the rewriter stop
// translating `args.X` -> `ctx.X` at source-text time.
func TestArgsReferenceProducesArgReference(t *testing.T) {
	expr := parseFilterToExpr(t, `concept==v1:cognition:space; payload.id==args.spaceId`)
	val := findArgRefValue(expr, "id")
	ref, ok := val.(*ArgReference)
	if !ok {
		t.Fatalf("payload.id value is %T, want *ArgReference", val)
	}
	if ref.Path != "spaceId" {
		t.Errorf("ArgReference.Path = %q, want %q", ref.Path, "spaceId")
	}
}

// TestArgsReferenceDottedPath pins multi-segment `args.X.Y` paths.
func TestArgsReferenceDottedPath(t *testing.T) {
	expr := parseFilterToExpr(t, `concept==v1:identity:user; payload.email==args.event.payload.email`)
	val := findArgRefValue(expr, "email")
	ref, ok := val.(*ArgReference)
	if !ok {
		t.Fatalf("payload.email value is %T, want *ArgReference", val)
	}
	if ref.Path != "event.payload.email" {
		t.Errorf("ArgReference.Path = %q, want %q", ref.Path, "event.payload.email")
	}
}

// TestArgsReferenceParityWithCtx pins that `args.X` and `ctx.X`
// produce identical AST nodes (same struct + Path field). They are
// the same runtime concept under two surface names during the
// transition.
func TestArgsReferenceParityWithCtx(t *testing.T) {
	argsExpr := parseFilterToExpr(t, `concept==v1:cognition:space; payload.id==args.spaceId`)
	ctxExpr := parseFilterToExpr(t, `concept==v1:cognition:space; payload.id==ctx.spaceId`)

	argsRef, ok := findArgRefValue(argsExpr, "id").(*ArgReference)
	if !ok {
		t.Fatalf("args.X did not produce *ArgReference")
	}
	ctxRef, ok := findArgRefValue(ctxExpr, "id").(*ArgReference)
	if !ok {
		t.Fatalf("ctx.X did not produce *ArgReference")
	}
	if argsRef.Path != ctxRef.Path {
		t.Errorf("paths diverge: args.X -> %q, ctx.X -> %q", argsRef.Path, ctxRef.Path)
	}
}

// TestBareArgsErrors pins that bare `args` (no field path) is
// rejected with a helpful message, matching the bare-ctx behaviour.
func TestBareArgsErrors(t *testing.T) {
	tokens, err := tokenize(`concept==v1:cognition:space; payload.id==args`)
	if err != nil {
		t.Fatalf("tokenize: %v", err)
	}
	p := newParser(tokens, nil)
	if _, err := p.parse(); err == nil {
		t.Fatalf("expected bare `args` to error, got nil")
	}
}
