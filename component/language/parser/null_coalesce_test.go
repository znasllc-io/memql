package parser

import "testing"

// #2611: the `??` null-coalescing operator, resurrected. It was retired in
// struct-form Phase 4 with a parse error, but the lexer kept the token and
// the object-literal value position kept the semantics. The resurrection
// moves the precedence deliberately: Swift-tight (tighter than comparison,
// looser than additive), because the dominant corpus idiom is
// fallback-then-compare -- `args.stage ?? "" == "active"` must mean
// `(args.stage ?? "") == "active"`. Under the historical/JS binding a
// non-nil LHS silently short-circuits the comparison: the exact
// silent-constant-gate bug class wave-3 (#2542) eliminated.
//
// `a ?? b ?? c` folds n-ary into ONE CoalesceExpr (the memql#1614 final-
// arg-fallback semantics, matching the object-literal fold), so every
// downstream evaluator sees exactly what the coalesce(a, b, c) spelling
// produces.

func TestNullCoalesce_BinaryAndChain(t *testing.T) {
	ast := parseFilterExpr(t, `payload.status??payload.fallback`)
	c, ok := ast.(*CoalesceExpr)
	if !ok {
		t.Fatalf("want CoalesceExpr, got %T", ast)
	}
	if len(c.Args) != 2 {
		t.Fatalf("want 2 args, got %d", len(c.Args))
	}

	chain := parseFilterExpr(t, `payload.a??payload.b??payload.c`)
	cc, ok := chain.(*CoalesceExpr)
	if !ok {
		t.Fatalf("chain: want CoalesceExpr, got %T", chain)
	}
	if len(cc.Args) != 3 {
		t.Fatalf("chain must fold n-ary into ONE CoalesceExpr (final-arg-fallback semantics); got %d args", len(cc.Args))
	}
	if _, nested := cc.Args[0].(*CoalesceExpr); nested {
		t.Fatal("chain folded left-nested instead of flat")
	}
}

// The headline: fallback-then-compare binds the coalesce FIRST.
func TestNullCoalesce_SwiftTightPrecedence(t *testing.T) {
	ast := parseFilterExpr(t, `coalesce(payload.stage, "x")=="active"`)
	want, ok := ast.(*BinaryComparisonExpr)
	if !ok {
		t.Fatalf("baseline coalesce() spelling: want BinaryComparisonExpr, got %T", ast)
	}
	if _, ok := want.Left.(*CoalesceExpr); !ok {
		t.Fatalf("baseline LHS: want CoalesceExpr, got %T", want.Left)
	}

	got := parseFilterExpr(t, `payload.stage??"x"=="active"`)
	cmp, ok := got.(*BinaryComparisonExpr)
	if !ok {
		t.Fatalf("?? spelling: want BinaryComparisonExpr (Swift-tight: (a ?? b) == c), got %T", got)
	}
	lhs, ok := cmp.Left.(*CoalesceExpr)
	if !ok {
		t.Fatalf("?? spelling LHS: want CoalesceExpr, got %T -- the operator bound looser than the comparison", cmp.Left)
	}
	if len(lhs.Args) != 2 {
		t.Fatalf("LHS coalesce: want 2 args, got %d", len(lhs.Args))
	}
	if cmp.Operator != ComparisonOperator("==") {
		t.Errorf("operator: want ==, got %q", cmp.Operator)
	}
}

// Looser than additive: `a + 1 ?? 0` coalesces the SUM.
func TestNullCoalesce_LooserThanAdditive(t *testing.T) {
	ast := parseFilterExpr(t, `payload.n+1??0`)
	c, ok := ast.(*CoalesceExpr)
	if !ok {
		t.Fatalf("want CoalesceExpr at top, got %T", ast)
	}
	if len(c.Args) != 2 {
		t.Fatalf("want 2 args, got %d", len(c.Args))
	}
	if _, ok := c.Args[0].(*ArithmeticExpr); !ok {
		t.Errorf("first arg: want the folded sum (*ArithmeticExpr), got %T", c.Args[0])
	}
}

// Tighter than && : `a ?? b && c` is `(a ?? b) && c`.
func TestNullCoalesce_TighterThanLogical(t *testing.T) {
	ast := parseFilterExpr(t, `payload.a??payload.b&&payload.c==true`)
	l, ok := ast.(*LogicalExpr)
	if !ok {
		t.Fatalf("want LogicalExpr at top, got %T", ast)
	}
	if l.Op != LogicalAnd {
		t.Fatalf("want &&, got %v", l.Op)
	}
	if _, ok := l.Left.(*CoalesceExpr); !ok {
		t.Errorf("left of &&: want CoalesceExpr, got %T", l.Left)
	}
}

// The retirement error is gone; the retired message must not resurface.
func TestNullCoalesce_NoRetirementError(t *testing.T) {
	tokens, err := NewLexer(`payload.a??payload.b`).Tokenize()
	if err != nil {
		t.Fatalf("lexer: %v", err)
	}
	if _, err := NewParser(tokens).parseExpression(); err != nil {
		t.Fatalf("`??` must parse post-#2611, got: %v", err)
	}
}

// The cond-args path bypasses the main cascade, so the fold is mirrored
// there in lockstep: `cond(args.stage ?? "" == "active", ...)` must produce
// the identical AST shape to the coalesce() spelling.
func TestNullCoalesce_ExpressionArgLockstep(t *testing.T) {
	parseArg := func(src string) ExpressionNode {
		t.Helper()
		tokens, err := NewLexer(src).Tokenize()
		if err != nil {
			t.Fatalf("tokenize %q: %v", src, err)
		}
		p := NewParser(tokens)
		expr, err := p.parseExpressionArg()
		if err != nil {
			t.Fatalf("parseExpressionArg %q: %v", src, err)
		}
		return expr
	}

	baseline := parseArg(`coalesce(args.stage, "")=="active"`)
	be, ok := baseline.(*EqExpr)
	if !ok {
		t.Fatalf("baseline: want EqExpr, got %T", baseline)
	}
	if _, ok := be.Left.(*CoalesceExpr); !ok {
		t.Fatalf("baseline LHS: want CoalesceExpr, got %T", be.Left)
	}

	short := parseArg(`args.stage??""=="active"`)
	se, ok := short.(*EqExpr)
	if !ok {
		t.Fatalf("?? spelling: want EqExpr (lockstep with the baseline), got %T", short)
	}
	lhs, ok := se.Left.(*CoalesceExpr)
	if !ok {
		t.Fatalf("?? spelling LHS: want CoalesceExpr, got %T", se.Left)
	}
	if len(lhs.Args) != 2 {
		t.Fatalf("LHS coalesce: want 2 args, got %d", len(lhs.Args))
	}

	if bare := parseArg(`args.kind??""`); func() bool { _, ok := bare.(*CoalesceExpr); return !ok }() {
		t.Fatalf("bare arg fold: want CoalesceExpr, got %T", bare)
	}
}
