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
		// A `w := cond(...)` assignment step is FLATTENED into a positional
		// FunctionStepConfig -- arg "0" the predicate, args "1"/"2" the
		// branch values -- with no CondExpr node left for the generic walk
		// to anchor on. Check the branch positions directly.
		if fc, ok := step.Config.(*languageParser.FunctionStepConfig); ok && fc != nil && strings.EqualFold(fc.Name, "cond") {
			for _, key := range []string{"1", "2"} {
				if node, isExpr := fc.Args[key].(languageParser.ExpressionNode); isExpr {
					if cmp := findIdentifierComparisonBranchValue(node, true); cmp != nil {
						return condBranchValueError(funcDef.Name, cmp)
					}
				}
			}
		}
		for _, expr := range stepValueExpressions(step) {
			if bad := findIdentifierComparisonBranchValue(expr, false); bad != nil {
				return condBranchValueError(funcDef.Name, bad)
			}
		}
	}
	return nil
}

func condBranchValueError(logicName string, bad *languageParser.ComparisonExpr) error {
	field := strings.Join(bad.Field.Parts, ".")
	return fmt.Errorf(
		"logic %q: a comparison is a cond PREDICATE, not a cond BRANCH value: the identifier-led comparison `%s %s ...` in branch-value position would be returned as its own source text; move the comparison into a predicate (`cond(%s %s ..., true, false)`) or return literals from the branches (#2655)",
		logicName, field, bad.Operator, field, bad.Operator)
}

// findIdentifierComparisonBranchValue walks a logic value expression carrying
// BRANCH-CONTEXT mode: inBranch is true while inside a cond Then/Else
// subtree, where ANY Field-led ComparisonExpr -- direct or laundered through
// wrapper builtins / coalesce args / call args -- is the silent source-text
// shape and violates. A nested cond's PREDICATE resets the mode (comparisons
// are first-class there), and a lambda body STOPS the walk entirely (a
// different, correct evaluation context). Returns nil when the tree is clean.
func findIdentifierComparisonBranchValue(node languageParser.ExpressionNode, inBranch bool) *languageParser.ComparisonExpr {
	switch n := node.(type) {
	case nil:
		return nil
	case *languageParser.ComparisonExpr:
		if inBranch {
			return n
		}
		return nil
	case *languageParser.CondExpr:
		for _, branch := range []languageParser.ExpressionNode{n.Then, n.Else} {
			if cmp := findIdentifierComparisonBranchValue(branch, true); cmp != nil {
				return cmp
			}
		}
		// The predicate admits comparisons (mode resets), but a NESTED cond
		// inside it still carries branch positions of its own.
		return findIdentifierComparisonBranchValue(n.Condition, false)
	case *languageParser.ArithmeticExpr:
		if cmp := findIdentifierComparisonBranchValue(n.Left, inBranch); cmp != nil {
			return cmp
		}
		return findIdentifierComparisonBranchValue(n.Right, inBranch)
	case *languageParser.BinaryComparisonExpr:
		if cmp := findIdentifierComparisonBranchValue(n.Left, inBranch); cmp != nil {
			return cmp
		}
		return findIdentifierComparisonBranchValue(n.Right, inBranch)
	case *languageParser.CoalesceExpr:
		return findIdentifierComparisonBranchValueIn(n.Args, inBranch)
	case *languageParser.ConcatExpr:
		return findIdentifierComparisonBranchValueIn(n.Args, inBranch)
	case *languageParser.AndExpr:
		return findIdentifierComparisonBranchValueIn(n.Args, inBranch)
	case *languageParser.OrExpr:
		return findIdentifierComparisonBranchValueIn(n.Args, inBranch)
	case *languageParser.NotExpr:
		return findIdentifierComparisonBranchValue(n.Target, inBranch)
	case *languageParser.LogicalExpr:
		if cmp := findIdentifierComparisonBranchValue(n.Left, inBranch); cmp != nil {
			return cmp
		}
		return findIdentifierComparisonBranchValue(n.Right, inBranch)
	case *languageParser.MethodCallExpr:
		if cmp := findIdentifierComparisonBranchValue(n.Receiver, inBranch); cmp != nil {
			return cmp
		}
		return findIdentifierComparisonBranchValueIn(n.Args, inBranch)
	case *languageParser.LambdaExpr:
		// STOP: a lambda body is a different, correct evaluation context --
		// the in-memory collection evaluator handles a Field-led comparison
		// as a cond branch value fine (docs matrix: "yes (lambda body)"),
		// and gating it would brick loadable trees.
		return nil
	case *languageParser.DotAccessExpr:
		return findIdentifierComparisonBranchValue(n.Object, inBranch)
	// Single-target and fixed-arity wrapper builtins must not launder a
	// nested violation (`lower(cond(..., x == "y", ...))`).
	case *languageParser.ToStringExpr:
		return findIdentifierComparisonBranchValue(n.Target, inBranch)
	case *languageParser.FirstExpr:
		return findIdentifierComparisonBranchValue(n.Target, inBranch)
	case *languageParser.LastExpr:
		return findIdentifierComparisonBranchValue(n.Target, inBranch)
	case *languageParser.LowerExpr:
		return findIdentifierComparisonBranchValue(n.Target, inBranch)
	case *languageParser.UpperExpr:
		return findIdentifierComparisonBranchValue(n.Target, inBranch)
	case *languageParser.TrimExpr:
		return findIdentifierComparisonBranchValue(n.Target, inBranch)
	case *languageParser.HashExpr:
		return findIdentifierComparisonBranchValue(n.Target, inBranch)
	case *languageParser.ContainsExpr:
		if cmp := findIdentifierComparisonBranchValue(n.Target, inBranch); cmp != nil {
			return cmp
		}
		return findIdentifierComparisonBranchValue(n.Substring, inBranch)
	case *languageParser.CanonicalIdExpr:
		return findIdentifierComparisonBranchValue(n.Value, inBranch)
	case *languageParser.AddDurationExpr:
		if cmp := findIdentifierComparisonBranchValue(n.Timestamp, inBranch); cmp != nil {
			return cmp
		}
		return findIdentifierComparisonBranchValue(n.Duration, inBranch)
	case *languageParser.SubtractTimestampsExpr:
		if cmp := findIdentifierComparisonBranchValue(n.T1, inBranch); cmp != nil {
			return cmp
		}
		return findIdentifierComparisonBranchValue(n.T2, inBranch)
	case *languageParser.DaysBetweenExpr:
		if cmp := findIdentifierComparisonBranchValue(n.Date1, inBranch); cmp != nil {
			return cmp
		}
		return findIdentifierComparisonBranchValue(n.Date2, inBranch)
	case *languageParser.FunctionCallExpr:
		return findIdentifierComparisonBranchValueInValue(n.Args, inBranch)
	case *languageParser.LiteralExpr:
		// Projection object/array literals carry ExpressionNodes in Value.
		return findIdentifierComparisonBranchValueInValue(n.Value, inBranch)
	}
	return nil
}

// findIdentifierComparisonBranchValueInValue recurses projection literal /
// call-arg values (maps, slices, nested ExpressionNodes), mirroring the
// arithmetic validator's findArithOverComparisonInValue.
func findIdentifierComparisonBranchValueInValue(v any, inBranch bool) *languageParser.ComparisonExpr {
	switch val := v.(type) {
	case languageParser.ExpressionNode:
		return findIdentifierComparisonBranchValue(val, inBranch)
	case map[string]any:
		for _, e := range val {
			if cmp := findIdentifierComparisonBranchValueInValue(e, inBranch); cmp != nil {
				return cmp
			}
		}
	case []any:
		for _, e := range val {
			if cmp := findIdentifierComparisonBranchValueInValue(e, inBranch); cmp != nil {
				return cmp
			}
		}
	}
	return nil
}

func findIdentifierComparisonBranchValueIn(nodes []languageParser.ExpressionNode, inBranch bool) *languageParser.ComparisonExpr {
	for _, n := range nodes {
		if cmp := findIdentifierComparisonBranchValue(n, inBranch); cmp != nil {
			return cmp
		}
	}
	return nil
}
