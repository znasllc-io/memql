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
// memql#2693 extends the gate to the two value positions ADJACENT to the cond
// branch that carried the identical silent-wrong class: a bare/terminal
// identifier-led comparison `return args.b == "y"` (a logic value step is a
// named-call query, so a top-level comparison here mis-routes as a store query
// -- the inline `query { ... }` filter form does not parse in a logic body),
// and the same comparison laundered through a coalesce/concat arg. The gate
// stays OFF for the legal boolean-returning shape (`x.empty() && args.b=="y"`),
// where a comparison is a first-class `&&`/`||`/`!` operand.
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
		// P2 (memql#2693): a QueryStepConfig is the terminal `return <expr>`
		// and every `x := <expr>` value step -- a logic query step is a named
		// call (FunctionCallExpr), so a bare top-level identifier-led
		// comparison here is the "mis-routed as a store query" shape, not a
		// real filter (the inline `query { ... }` filter form does not parse
		// in a logic body). Walk the value in branch context; real named-call
		// query steps carry no top-level comparison, and their value-builtin
		// wrappers are covered by the coalesce/concat arms above.
		if qc, ok := step.Config.(*languageParser.QueryStepConfig); ok && qc != nil && qc.Query != nil {
			if bad := findIdentifierComparisonBranchValue(qc.Query, true); bad != nil {
				return condBranchValueError(funcDef.Name, bad)
			}
			continue
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
		"logic %q: an identifier-led comparison `%s %s ...` in a VALUE position (a cond BRANCH value, a coalesce/concat arg, or a bare/terminal return) silently returns its own source text or mis-routes as a store query -- neither evaluates the comparison; move it into a predicate (`cond(%s %s ..., true, false)`), combine it into a boolean expression (`... && %s %s ...`), or return literals (#2655, #2693)",
		logicName, field, bad.Operator, field, bad.Operator, field, bad.Operator)
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
	// Boolean combinators are a PREDICATE context: a comparison that is a
	// direct `&&`/`||`/`!` operand is a legal boolean-returning logic
	// (`existing.empty() && args.x == "bff"`, dsl/cluster/logic.memql), not a
	// mis-lowering value -- so reset the mode to false for the operands. The
	// walk CONTINUES (it does not stop), so a genuine cond-branch violation
	// nested under a boolean operand is still caught (memql#2693). `&&`/`||`
	// parse to LogicalExpr and `!` to NotExpr; the parser no longer builds
	// AndExpr/OrExpr (the removed `and()`/`or()` prefix builtins), so those
	// vestigial node types have no arm.
	case *languageParser.NotExpr:
		return findIdentifierComparisonBranchValue(n.Target, false)
	case *languageParser.LogicalExpr:
		if cmp := findIdentifierComparisonBranchValue(n.Left, false); cmp != nil {
			return cmp
		}
		return findIdentifierComparisonBranchValue(n.Right, false)
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
