package memql

import (
	"fmt"
	"strings"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// logic_cond_branch_validator.go turns the identifier-led-comparison cond
// BRANCH value into a COMPILE-TIME rejection (memql#2655).
//
// A comparison is a first-class cond PREDICATE but not a cond BRANCH value
// (authoring-rules): the engine has no scalar plan-root branch for a Field-led
// comparison, and the multi-step string lowering serialized the branch and
// handed the chosen branch back as its own SOURCE TEXT -- so
// `cond(args.a == "x", args.b == "y", "n")` loaded green and silently returned
// the string `args.b=="y"`. The expression-led sibling (parenthesized,
// BinaryComparisonExpr) is deliberately NOT gated here: it already fails
// loudly at runtime, and this validator changes only the silent shape.
//
// Scope: Logic functions only, same altitude as
// validateLogicArithmeticOperands -- every step expression at load, so the
// lint/boot-parity pass surfaces it too.
func validateLogicCondBranchValues(funcDef *languageParser.FunctionDef) error {
	if funcDef == nil || funcDef.Type != languageParser.FunctionTypeLogic {
		return nil
	}
	auto, ok := funcDef.Body.(*languageParser.AutomationDef)
	if !ok || auto == nil {
		return nil
	}
	for _, step := range auto.Steps {
		for _, expr := range stepValueExpressions(step) {
			if bad := findIdentifierComparisonBranchValue(expr); bad != nil {
				field := strings.Join(bad.Field.Parts, ".")
				return fmt.Errorf(
					"logic %q: a comparison is a cond PREDICATE, not a cond BRANCH value: the identifier-led comparison `%s %s ...` in branch-value position would be returned as its own source text; move the comparison into a predicate (`cond(%s %s ..., true, false)`) or return literals from the branches (#2655)",
					funcDef.Name, field, bad.Operator, field, bad.Operator)
			}
		}
	}
	return nil
}

// findIdentifierComparisonBranchValue walks a logic value expression and
// returns the first Field-led ComparisonExpr sitting in a cond BRANCH value
// position (Then/Else), recursing the same containers the arithmetic-operand
// walk covers so a violation nested inside a coalesce arg, projection value,
// or nested cond branch is caught too. The cond PREDICATE position is
// deliberately exempt. Returns nil when the tree is clean.
func findIdentifierComparisonBranchValue(node languageParser.ExpressionNode) *languageParser.ComparisonExpr {
	switch n := node.(type) {
	case nil:
		return nil
	case *languageParser.CondExpr:
		for _, branch := range []languageParser.ExpressionNode{n.Then, n.Else} {
			if cmp, ok := branch.(*languageParser.ComparisonExpr); ok {
				return cmp
			}
			if cmp := findIdentifierComparisonBranchValue(branch); cmp != nil {
				return cmp
			}
		}
		// The predicate admits comparisons, but a NESTED cond inside it
		// still carries branch positions of its own.
		return findIdentifierComparisonBranchValue(n.Condition)
	case *languageParser.ArithmeticExpr:
		if cmp := findIdentifierComparisonBranchValue(n.Left); cmp != nil {
			return cmp
		}
		return findIdentifierComparisonBranchValue(n.Right)
	case *languageParser.BinaryComparisonExpr:
		if cmp := findIdentifierComparisonBranchValue(n.Left); cmp != nil {
			return cmp
		}
		return findIdentifierComparisonBranchValue(n.Right)
	case *languageParser.CoalesceExpr:
		return findIdentifierComparisonBranchValueIn(n.Args)
	case *languageParser.ConcatExpr:
		return findIdentifierComparisonBranchValueIn(n.Args)
	case *languageParser.AndExpr:
		return findIdentifierComparisonBranchValueIn(n.Args)
	case *languageParser.OrExpr:
		return findIdentifierComparisonBranchValueIn(n.Args)
	case *languageParser.NotExpr:
		return findIdentifierComparisonBranchValue(n.Target)
	case *languageParser.LogicalExpr:
		if cmp := findIdentifierComparisonBranchValue(n.Left); cmp != nil {
			return cmp
		}
		return findIdentifierComparisonBranchValue(n.Right)
	case *languageParser.MethodCallExpr:
		if cmp := findIdentifierComparisonBranchValue(n.Receiver); cmp != nil {
			return cmp
		}
		return findIdentifierComparisonBranchValueIn(n.Args)
	case *languageParser.LambdaExpr:
		return findIdentifierComparisonBranchValue(n.Body)
	case *languageParser.DotAccessExpr:
		return findIdentifierComparisonBranchValue(n.Object)
	}
	return nil
}

func findIdentifierComparisonBranchValueIn(nodes []languageParser.ExpressionNode) *languageParser.ComparisonExpr {
	for _, n := range nodes {
		if cmp := findIdentifierComparisonBranchValue(n); cmp != nil {
			return cmp
		}
	}
	return nil
}
