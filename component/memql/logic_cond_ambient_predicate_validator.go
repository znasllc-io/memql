package memql

import (
	"fmt"
	"strings"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// logic_cond_ambient_predicate_validator.go closes the half of memql#2962 that
// the arg-substitution fix structurally cannot reach.
//
// #2962 is about a cond PREDICATE that is a silent constant. The fix resolves
// an `args.`-rooted comparison during arg expansion, which is the only place it
// CAN be resolved -- Execute substitutes args and then evaluates the plan root
// with no args map at all. But expansion receives args and nothing else, so an
// ambient-namespace comparison (`actor.`, `config.`, `partition`, `now`) falls
// through untouched, resolves against a nil lambda scope, and takes the else
// branch for every input. Exactly the reported defect, in the namespace the
// report's own motivation is about:
//
//	cond(actor.role == "owner", "elevated", "plain")   ->  always "plain"
//
// That is a role gate that is open or closed by accident. `dsl/deployment/
// logic.memql`'s owner-only rollback gate escapes it only because the author
// hoisted `role := actor.role ?? ""` into a local first and compared the local;
// inlining it -- a change a reviewer would wave through as a simplification --
// silently disables the gate.
//
// #2962's definition of done anticipates this: a shape that cannot be supported
// must be a LOAD-TIME ERROR rather than a silent constant. Supporting it
// properly means plumbing the actor envelope and config through arg expansion,
// which is a wider change than this issue; filed separately. Until then this
// refuses the shape at load, so the failure is loud and immediate instead of a
// gate that quietly never fires.
//
// Scope is deliberately narrow. Only the PREDICATE position of `cond`, and only
// the reserved ambient roots -- `now`, `actor`, `partition`, `config`, `trace`
// are reserved top-level identifiers, so none of them can be a lambda local or
// a row field, and rejecting them here cannot collide with a legitimate lazy
// reference. A row/lambda-rooted comparison still falls through untouched, and
// an `args.`-rooted one is handled by the substitution this sits beside.
func validateLogicCondAmbientPredicate(funcDef *languageParser.FunctionDef) error {
	if funcDef == nil || funcDef.Type != languageParser.FunctionTypeLogic {
		return nil
	}
	auto, ok := funcDef.Body.(*languageParser.AutomationDef)
	if !ok || auto == nil {
		return nil
	}
	for _, step := range auto.Steps {
		var predicates []languageParser.ExpressionNode

		// An assignment step `w := cond(...)` is FLATTENED into a positional
		// FunctionStepConfig, arg "0" being the predicate -- the same shape
		// validateLogicCondBranchValues reads the branch positions "1"/"2"
		// from.
		if fc, isFn := step.Config.(*languageParser.FunctionStepConfig); isFn && fc != nil && strings.EqualFold(fc.Name, "cond") {
			if node, isExpr := fc.Args["0"].(languageParser.ExpressionNode); isExpr {
				predicates = append(predicates, node)
			}
		}
		// The terminal `return cond(...)` keeps its CondExpr node instead, so
		// the single-statement body -- the shape #2962 reports -- is only
		// reachable this way. Walk nested conds too: a predicate in an else
		// branch is the same defect one level down.
		if qc, isQ := step.Config.(*languageParser.QueryStepConfig); isQ && qc != nil && qc.Query != nil {
			predicates = append(predicates, condPredicatesIn(qc.Query)...)
		}

		for _, node := range predicates {
			root := ambientComparisonRoot(node)
			if root == "" {
				continue
			}
			return fmt.Errorf(
				"logic %q: cond predicate compares %s.* , which cannot be resolved. "+
					"Arg substitution runs before evaluation and receives only args, so an "+
					"ambient comparison here is not evaluated -- it silently takes the ELSE "+
					"branch for every input, which makes a gate written this way open or closed "+
					"by accident (memql#2962). Bind it to a local first and compare that: "+
					"`r := %s.<field> ?? \"\"` then `cond(r == ..., ...)`",
				funcDef.Name, root, root)
		}
	}
	return nil
}

// condPredicatesIn returns the predicate of every cond reachable from node,
// including conds nested in either branch.
func condPredicatesIn(node languageParser.ExpressionNode) []languageParser.ExpressionNode {
	switch n := node.(type) {
	case *languageParser.CondExpr:
		if n == nil {
			return nil
		}
		out := []languageParser.ExpressionNode{n.Condition}
		out = append(out, condPredicatesIn(n.Then)...)
		out = append(out, condPredicatesIn(n.Else)...)
		return out
	default:
		return nil
	}
}

// ambientComparisonRoot returns the reserved ambient namespace a Field-led
// comparison is rooted at, or "" when the node is not such a comparison.
func ambientComparisonRoot(node languageParser.ExpressionNode) string {
	cmp, ok := node.(*languageParser.ComparisonExpr)
	if !ok || cmp == nil {
		return ""
	}
	parts := cmp.Field.Parts
	if len(parts) == 0 {
		raw := strings.TrimSpace(cmp.Field.Raw)
		if raw == "" {
			return ""
		}
		parts = strings.Split(raw, ".")
	}
	switch parts[0] {
	case "actor", "config", "partition", "trace":
		return parts[0]
	default:
		return ""
	}
}
