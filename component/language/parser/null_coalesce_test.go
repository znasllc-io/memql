package parser

import (
	"fmt"
	"testing"
)

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

	bare := parseArg(`args.kind??""`)
	if _, ok := bare.(*CoalesceExpr); !ok {
		t.Fatalf("bare arg fold: want CoalesceExpr, got %T", bare)
	}
}

// Review finding 2 on #2611: precedence must NOT depend on the operand's
// token type. parsePrimary folds identifier-led comparisons early, which
// made `stage ?? def == "active"` parse JS-loose (the comparison swallowed
// into the coalesce arm) while the literal-fallback form parsed Swift-tight.
// This is the reviewer's full matrix; every row must bind Swift-tight.
func TestNullCoalesce_SwiftTightForIdentifierOperands(t *testing.T) {
	type row struct {
		src        string
		wantTop    string // "cmp" or "coalesce" or "eq"
		coalesceOn string // "left" or "right" of the comparison
	}
	rows := []row{
		{`payload.stage??payload.def=="active"`, "cmp", "left"},
		// args-valued arms exercise the ArgRef early-return branch
		// (round-3 finding C); payload rows cannot reach it.
		{`args.stage??args.def=="active"`, "cmp", "left"},
		{`payload.a==payload.b??payload.c`, "cmp", "right"},
		{`payload.a=="b"??"c"`, "cmp", "right"},
		{`payload.n>payload.m??0`, "cmp", "right"},
	}
	for _, r := range rows {
		ast := parseFilterExpr(t, r.src)
		cmp, ok := ast.(*BinaryComparisonExpr)
		if !ok {
			// identifier-led folds may legitimately produce ComparisonExpr
			// (field-led) when the coalesce is on the VALUE side; accept
			// either comparison shape but never a top-level coalesce.
			if _, isCo := ast.(*CoalesceExpr); isCo {
				t.Errorf("%s: parsed JS-loose (top-level CoalesceExpr swallowed the comparison)", r.src)
				continue
			}
			fc, isField := ast.(*ComparisonExpr)
			if !isField {
				t.Errorf("%s: want a comparison at top, got %T", r.src, ast)
				continue
			}
			if r.coalesceOn == "right" {
				if _, ok := fc.Value.(*CoalesceExpr); !ok {
					t.Errorf("%s: comparison value: want CoalesceExpr, got %T", r.src, fc.Value)
				}
			}
			continue
		}
		side := cmp.Left
		if r.coalesceOn == "right" {
			side = cmp.Right
		}
		if _, ok := side.(*CoalesceExpr); !ok {
			t.Errorf("%s: %s of comparison: want CoalesceExpr, got %T", r.src, r.coalesceOn, side)
		}
	}

	// Double-sided: a ?? b == c ?? d must compare the two coalesces.
	ast := parseFilterExpr(t, `payload.a??payload.b==payload.c??payload.d`)
	cmp, ok := ast.(*BinaryComparisonExpr)
	if !ok {
		t.Fatalf("double-sided: want BinaryComparisonExpr, got %T", ast)
	}
	if _, ok := cmp.Left.(*CoalesceExpr); !ok {
		t.Errorf("double-sided left: want CoalesceExpr, got %T", cmp.Left)
	}
	if _, ok := cmp.Right.(*CoalesceExpr); !ok {
		t.Errorf("double-sided right: want CoalesceExpr, got %T", cmp.Right)
	}
}

// The DoD's `x == a ?? b` shape in the cond-args position (finding 3).
func TestNullCoalesce_ExpressionArgIdentifierRHS(t *testing.T) {
	tokens, err := NewLexer(`args.x==args.a??args.b`).Tokenize()
	if err != nil {
		t.Fatalf("tokenize: %v", err)
	}
	expr, err := NewParser(tokens).parseExpressionArg()
	if err != nil {
		t.Fatalf("parseExpressionArg: %v", err)
	}
	// Post-round-2: the identifier-led fold produces the BASELINE shape --
	// ComparisonExpr with the coalesce chain as its Value, exactly what
	// `args.x == coalesce(args.a, args.b)` produces in this position.
	cmp, ok := expr.(*ComparisonExpr)
	if !ok {
		t.Fatalf("want ComparisonExpr at top (baseline parity), got %T", expr)
	}
	if _, ok := cmp.Value.(*CoalesceExpr); !ok {
		t.Errorf("comparison value: want CoalesceExpr, got %T", cmp.Value)
	}

	tokens2, err := NewLexer(`args.x==coalesce(args.a, args.b)`).Tokenize()
	if err != nil {
		t.Fatalf("tokenize baseline: %v", err)
	}
	base, err := NewParser(tokens2).parseExpressionArg()
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	if fmt.Sprintf("%T", base) != fmt.Sprintf("%T", expr) {
		t.Errorf("shape parity: baseline %T vs ?? %T", base, expr)
	}
}

// Review finding 1 on #2611: the logic assignment RHS is a THIRD grammar
// position (the step parser's speculative arithmetic route). The story's
// own headline example must parse: `st := args.stage ?? ""`.
func TestNullCoalesce_LogicAssignmentRHS(t *testing.T) {
	src := `
func (Logic) probeAssign(_ any) {
  st := args.stage ?? ""
  return st
}
`
	file, err := ParseFile(src)
	if err != nil {
		t.Fatalf("the headline assignment `st := args.stage ?? \"\"` must parse: %v", err)
	}
	if file == nil || len(file.Definitions) != 1 {
		t.Fatalf("want one parsed definition")
	}
}

// Review round-2 finding A on #2611: the fold-suppression flag must not
// leak into bracketed subexpressions of a continuation operand. Prefixing
// `x ?? ` onto a working cond() must not change the predicate's node type
// (EqExpr under the leak, ComparisonExpr in the baseline -- the leaked
// shape dies in the AST converter at load).
func TestNullCoalesce_NoFoldSuppressionLeakIntoCalls(t *testing.T) {
	predType := func(src string) string {
		t.Helper()
		expr, err := ParseExpression(src)
		if err != nil {
			t.Fatalf("parse %q: %v", src, err)
		}
		co, ok := expr.(*CoalesceExpr)
		if !ok {
			t.Fatalf("%q: want CoalesceExpr at top, got %T", src, expr)
		}
		cond, ok := co.Args[1].(*CondExpr)
		if !ok {
			t.Fatalf("%q: want CondExpr arm, got %T", src, co.Args[1])
		}
		return typeName(cond.Condition)
	}

	baseline := func(src string) string {
		t.Helper()
		expr, err := ParseExpression(src)
		if err != nil {
			t.Fatalf("parse %q: %v", src, err)
		}
		co, ok := expr.(*CoalesceExpr)
		if !ok {
			t.Fatalf("%q: want CoalesceExpr at top, got %T", src, expr)
		}
		cond, ok := co.Args[1].(*CondExpr)
		if !ok {
			t.Fatalf("%q: want CondExpr arm, got %T", src, co.Args[1])
		}
		return typeName(cond.Condition)
	}

	short := predType(`args.a??cond(args.b=="active","yes","no")`)
	long := baseline(`coalesce(args.a, cond(args.b=="active","yes","no"))`)
	if short != long {
		t.Errorf("cond predicate node diverged under ??: short=%s baseline=%s (the flag leaked into the call args)", short, long)
	}

	// Parens inside a continuation operand: same rule.
	pShort, err := ParseExpression(`payload.a??(payload.b=="x")`)
	if err != nil {
		t.Fatalf("paren short: %v", err)
	}
	pLong, err := ParseExpression(`coalesce(payload.a, (payload.b=="x"))`)
	if err != nil {
		t.Fatalf("paren baseline: %v", err)
	}
	sArm := typeName(pShort.(*CoalesceExpr).Args[1])
	lArm := typeName(pLong.(*CoalesceExpr).Args[1])
	if sArm != lArm {
		t.Errorf("paren arm node diverged under ??: short=%s baseline=%s", sArm, lArm)
	}
}

func typeName(v any) string {
	if v == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%T", v)
}
