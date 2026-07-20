package memql

import (
	"strings"
	"testing"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// #2612 established that a non-identifier-led ==/!= in a cond predicate must
// load (the fylo#44-era live-mount law); #2654 normalized the arg grammar so
// those predicates arrive as BinaryComparisonExpr -- the one comparison shape
// -- and retired the EqExpr/NotExpr(EqExpr) duals. The loader-altitude test
// pins the behavior; the NotExpr tests pin that the converter never
// converts-through a bang negation (boolean inversion) and keeps the scope
// gate.

// The fylo#44 live-mount law, at the loader altitude: a nested cond whose
// predicate compares a coalesce (either spelling) to a literal must load.
func TestLogicNestedCondCoalescePredicate_Loads(t *testing.T) {
	for name, predicate := range map[string]string{
		"coalesce-spelling": `coalesce(args.b, "") == "y"`,
		"operator-spelling": `args.b ?? "" == "y"`,
	} {
		t.Run(name, func(t *testing.T) {
			src := strings.Join([]string{
				"@description(\"nested cond coalesce predicate probe\")",
				"logic logicNestedCondProbe {",
				"  args {",
				"    a string @required",
				"    b string",
				"  }",
				"  body {",
				"    return cond(args.a == \"x\", cond(" + predicate + ", \"1\", \"2\"), \"3\")",
				"  }",
				"}",
			}, "\n")

			fn, err := tryParseNewFunctionSyntax("logicNestedCondProbe", "logic", src, "common.logic.memql", dotAccessLoadRegistry())
			if err != nil {
				t.Fatalf("nested cond with a %s predicate must load: %v", name, err)
			}
			if fn == nil || fn.Expr == nil {
				t.Fatalf("expected fn.Expr to be set")
			}
		})
	}
}

// Review finding 5 (#2612) carried forward through #2654: a NOT of ANY target
// must REJECT, never convert-through (convert-through inverts boolean
// semantics) -- since the arg grammar no longer produces NotExpr for !=, the
// only producer left is the bang `!` operator, which never converted. The
// scope gate must still fire first in strict scope.
func TestConvertNotExpr_RejectsAndKeepsScopeGate(t *testing.T) {
	loose := NewASTConverter(WithCollectionMethods())

	bang := &languageParser.NotExpr{Target: &languageParser.ArgRefExpr{Path: "flag"}}
	if _, err := loose.ConvertExpression(bang); err == nil || !strings.Contains(err.Error(), "#2612") {
		t.Errorf("NotExpr must reject with the narrowness error, got %v", err)
	}

	// The scope gate fires for NotExpr in strict scope, same as
	// BinaryComparisonExpr -- an expression-led negation must not leak
	// toward SQL compilation.
	strict := NewASTConverter()
	ne := &languageParser.NotExpr{Target: &languageParser.ArgRefExpr{Path: "flag"}}
	if _, err := strict.ConvertExpression(ne); err == nil || !strings.Contains(err.Error(), "logic / collection lambdas") {
		t.Errorf("NotExpr in strict scope must get the #2542 scope error, got %v", err)
	}
}

// The normalized equality shape converts under WithCollectionMethods and
// scope-errors elsewhere -- parity previously pinned for the retired EqExpr
// shape, now pinned on the one shape both grammar positions emit.
func TestConvertBinaryComparisonEquality_InMemoryOnly(t *testing.T) {
	eq := &languageParser.BinaryComparisonExpr{
		Left:     &languageParser.CoalesceExpr{Args: []languageParser.ExpressionNode{&languageParser.ArgRefExpr{Path: "x"}}},
		Operator: languageParser.OpEq,
		Right:    &languageParser.LiteralExpr{Value: "y"},
	}

	conv := NewASTConverter(WithCollectionMethods())
	node, err := conv.ConvertExpression(eq)
	if err != nil {
		t.Fatalf("equality BinaryComparisonExpr must convert under WithCollectionMethods: %v", err)
	}
	cmp, ok := node.(*BinaryComparisonExpression)
	if !ok {
		t.Fatalf("want *BinaryComparisonExpression, got %T", node)
	}
	if cmp.Operator != OpEq {
		t.Errorf("operator: want ==, got %v", cmp.Operator)
	}

	ne := &languageParser.BinaryComparisonExpr{Left: eq.Left, Operator: languageParser.OpNe, Right: eq.Right}
	nNode, err := conv.ConvertExpression(ne)
	if err != nil {
		t.Fatalf("inequality BinaryComparisonExpr must convert under WithCollectionMethods: %v", err)
	}
	if nCmp, ok := nNode.(*BinaryComparisonExpression); !ok || nCmp.Operator != OpNe {
		t.Fatalf("want *BinaryComparisonExpression{!=}, got %T (%v)", nNode, err)
	}

	// Scope parity: specs/query filters reject the expression-led shape.
	strict := NewASTConverter()
	if _, err := strict.ConvertExpression(eq); err == nil || !strings.Contains(err.Error(), "logic / collection lambdas") {
		t.Errorf("equality outside collection scope: want the #2542 scope error, got %v", err)
	}
}
