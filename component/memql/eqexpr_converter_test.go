package memql

import (
	"strings"
	"testing"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// #2612: EqExpr/NotExpr are the shapes parseExpressionArg emits for ==/!=
// with a non-identifier-led left side (cond predicates, #2407/#2542). The
// AST converter rejected them ("unsupported parser expression type"), which
// is the fylo#44-era live-mount law: a coalesce() call -- and now its ??
// shorthand -- directly inside a nested cond predicate died at load while
// memqllint stayed green. They convert exactly like BinaryComparisonExpr:
// in-memory only (WithCollectionMethods), scope-erroring elsewhere.

func TestConvertEqExpr_InMemoryOnly(t *testing.T) {
	eq := &languageParser.EqExpr{
		Left:  &languageParser.CoalesceExpr{Args: []languageParser.ExpressionNode{&languageParser.ArgRefExpr{Path: "x"}}},
		Right: &languageParser.LiteralExpr{Value: "y"},
	}

	conv := NewASTConverter(WithCollectionMethods())
	node, err := conv.ConvertExpression(eq)
	if err != nil {
		t.Fatalf("EqExpr must convert under WithCollectionMethods: %v", err)
	}
	cmp, ok := node.(*BinaryComparisonExpression)
	if !ok {
		t.Fatalf("want *BinaryComparisonExpression, got %T", node)
	}
	if cmp.Operator != OpEq {
		t.Errorf("operator: want ==, got %v", cmp.Operator)
	}

	not := &languageParser.NotExpr{Target: eq}
	nNode, err := conv.ConvertExpression(not)
	if err != nil {
		t.Fatalf("NotExpr{EqExpr} must convert under WithCollectionMethods: %v", err)
	}
	nCmp, ok := nNode.(*BinaryComparisonExpression)
	if !ok {
		t.Fatalf("want *BinaryComparisonExpression for !=, got %T", nNode)
	}
	if nCmp.Operator != OpNe {
		t.Errorf("operator: want !=, got %v", nCmp.Operator)
	}

	// Scope parity with BinaryComparisonExpr: specs/query filters reject.
	strict := NewASTConverter()
	if _, err := strict.ConvertExpression(eq); err == nil || !strings.Contains(err.Error(), "logic / collection lambdas") {
		t.Errorf("EqExpr outside collection scope: want the #2542 scope error, got %v", err)
	}
}

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

// Review finding 5: both properties below survived mutation with the suite
// green -- silently converting !x to x (boolean inversion) and deleting the
// NotExpr scope gate were undetected. Pinned.
func TestConvertNotExpr_NarrownessAndScope(t *testing.T) {
	loose := NewASTConverter(WithCollectionMethods())

	// A NOT of anything but the != equality shape must REJECT, never
	// convert-through (convert-through inverts boolean semantics).
	bang := &languageParser.NotExpr{Target: &languageParser.ArgRefExpr{Path: "flag"}}
	if _, err := loose.ConvertExpression(bang); err == nil || !strings.Contains(err.Error(), "#2612") {
		t.Errorf("NotExpr{non-EqExpr} must reject with the #2612 narrowness error, got %v", err)
	}

	// The scope gate must fire for NotExpr in strict scope, same as EqExpr
	// and BinaryComparisonExpr -- a != expression-led comparison must not
	// leak toward SQL compilation.
	strict := NewASTConverter()
	ne := &languageParser.NotExpr{Target: &languageParser.EqExpr{
		Left:  &languageParser.CoalesceExpr{Args: []languageParser.ExpressionNode{&languageParser.ArgRefExpr{Path: "x"}}},
		Right: &languageParser.LiteralExpr{Value: "y"},
	}}
	if _, err := strict.ConvertExpression(ne); err == nil || !strings.Contains(err.Error(), "logic / collection lambdas") {
		t.Errorf("NotExpr in strict scope must get the #2542 scope error, got %v", err)
	}
}
