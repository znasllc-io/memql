package memql

import (
	"fmt"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// logic_arithmetic_operand_validator.go turns the unparenthesized-comparison
// arithmetic trap (#2542) into a COMPILE-TIME rejection across the WHOLE logic
// body, not just the terminal return.
//
// `a - b > 0` parses as `a - (b > 0)`: a trailing bare-identifier operand folds
// the `> 0` into a Field-led ComparisonExpr one precedence level down (the
// pinned parser.TrailingIdentifierQuirk), so the subtraction's right operand is
// a boolean. A boolean is never a valid arithmetic operand, so the shape is
// always an author mistake -- but memqllint accepted it (parse + Init both pass;
// arithmetic converts, the boolean operand only surfaces at evalCollScalar) and
// the runtime then failed with an opaque `right operand of "-" is not numeric
// (bool)` / `expression *ast.ComparisonExpr is not supported as an arithmetic
// operand` error.
//
// The engine's ConvertExpression already rejects the trap in convertArithmeticExpr
// (comparisonOperandOf), which covers the logic TERMINAL RETURN at load time (the
// loader converts the `_return` expression). Multi-step logics stash their
// INTERMEDIATE `:=` steps as an un-converted AutomationDef -- those steps are
// re-parsed + evaluated only at RunLogic, so a trap in `flag := a - b > 0` would
// slip past Init. This validator walks every step's expression at load so the
// same clear rejection fires for the intermediate-step position too, and thus
// through the lint/boot-parity pass (LintUnifiedTree runs the real Init).
//
// Scope: Logic functions only (arithmetic is a logic/lambda-only surface).
func validateLogicArithmeticOperands(funcDef *languageParser.FunctionDef) error {
	if funcDef == nil || funcDef.Type != languageParser.FunctionTypeLogic {
		return nil
	}
	auto, ok := funcDef.Body.(*languageParser.AutomationDef)
	if !ok || auto == nil {
		return nil
	}
	for _, step := range auto.Steps {
		for _, expr := range stepValueExpressions(step) {
			if arith, side := findArithmeticOverComparison(expr); arith != nil {
				return fmt.Errorf(
					"logic %q: arithmetic operand is a comparison (%s of `%s`): `a %s b > 0` parses as `a %s (b > 0)` because the trailing identifier folds the comparison into the arithmetic; parenthesize the arithmetic sub-expression, e.g. `(a %s b) > 0` (#2542)",
					funcDef.Name, side, arith.Op, arith.Op, arith.Op, arith.Op)
			}
		}
	}
	return nil
}

// stepValueExpressions returns the value expressions carried by a logic step
// whose config is expression-bearing. A `:=` scalar step (arithmetic /
// comparison / cond / chain / query) and the synthetic `_return` step both wrap
// their expression in a QueryStepConfig.Query; a positional-builtin / function
// step carries its args. Anything else contributes no scalar value expression.
func stepValueExpressions(step languageParser.StepDef) []languageParser.ExpressionNode {
	switch cfg := step.Config.(type) {
	case *languageParser.QueryStepConfig:
		if cfg != nil && cfg.Query != nil {
			return []languageParser.ExpressionNode{cfg.Query}
		}
	}
	return nil
}

// findArithmeticOverComparison walks a logic value expression and returns the
// first ArithmeticExpr whose operand is a boolean-valued comparison node -- the
// #2542 unparenthesized-comparison trap -- with the offending side. It recurses
// the expression containers a logic value position admits (arithmetic,
// comparison, cond, coalesce/concat, method-call chains + lambda bodies, dot
// access, projection object/array literals, the string/date builtins), so a trap
// nested inside a projection value or a cond branch is caught too. Returns
// (nil, "") when the tree is clean.
func findArithmeticOverComparison(node languageParser.ExpressionNode) (*languageParser.ArithmeticExpr, string) {
	switch n := node.(type) {
	case nil:
		return nil, ""
	case *languageParser.ArithmeticExpr:
		if isComparisonNode(n.Left) {
			return n, "left"
		}
		if isComparisonNode(n.Right) {
			return n, "right"
		}
		if a, s := findArithmeticOverComparison(n.Left); a != nil {
			return a, s
		}
		return findArithmeticOverComparison(n.Right)
	case *languageParser.BinaryComparisonExpr:
		if a, s := findArithmeticOverComparison(n.Left); a != nil {
			return a, s
		}
		return findArithmeticOverComparison(n.Right)
	case *languageParser.CondExpr:
		for _, sub := range []languageParser.ExpressionNode{n.Condition, n.Then, n.Else} {
			if a, s := findArithmeticOverComparison(sub); a != nil {
				return a, s
			}
		}
	case *languageParser.CoalesceExpr:
		return findFirstArithOverComparison(n.Args)
	case *languageParser.ConcatExpr:
		return findFirstArithOverComparison(n.Args)
	case *languageParser.AndExpr:
		return findFirstArithOverComparison(n.Args)
	case *languageParser.OrExpr:
		return findFirstArithOverComparison(n.Args)
	case *languageParser.NotExpr:
		return findArithmeticOverComparison(n.Target)
	case *languageParser.LogicalExpr:
		if a, s := findArithmeticOverComparison(n.Left); a != nil {
			return a, s
		}
		return findArithmeticOverComparison(n.Right)
	case *languageParser.MethodCallExpr:
		if a, s := findArithmeticOverComparison(n.Receiver); a != nil {
			return a, s
		}
		return findFirstArithOverComparison(n.Args)
	case *languageParser.LambdaExpr:
		return findArithmeticOverComparison(n.Body)
	case *languageParser.DotAccessExpr:
		return findArithmeticOverComparison(n.Object)
	case *languageParser.LiteralExpr:
		return findArithOverComparisonInValue(n.Value)
	}
	return nil, ""
}

// findFirstArithOverComparison scans a slice of expression args.
func findFirstArithOverComparison(args []languageParser.ExpressionNode) (*languageParser.ArithmeticExpr, string) {
	for _, a := range args {
		if arith, s := findArithmeticOverComparison(a); arith != nil {
			return arith, s
		}
	}
	return nil, ""
}

// findArithOverComparisonInValue recurses a projection object/array literal
// value (`{acc: g.a.count() - g.b.count() > 0}`), whose entries may be
// ExpressionNodes carried on a LiteralExpr.
func findArithOverComparisonInValue(v any) (*languageParser.ArithmeticExpr, string) {
	switch val := v.(type) {
	case languageParser.ExpressionNode:
		return findArithmeticOverComparison(val)
	case map[string]any:
		for _, e := range val {
			if a, s := findArithOverComparisonInValue(e); a != nil {
				return a, s
			}
		}
	case []any:
		for _, e := range val {
			if a, s := findArithOverComparisonInValue(e); a != nil {
				return a, s
			}
		}
	}
	return nil, ""
}
