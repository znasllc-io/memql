package automations

import "strings"

// tryEvaluateLogicalLocally resolves a logic-body terminal return whose top
// node is a boolean connective (`&&` / `||`, with optional leading `!` groups)
// -- e.g. bootstrapCluster's
//
//	return existing.empty() && args.event.payload.node.type == "bff"
//
// EvaluateLocalExpr's other short-circuits each match a single AST node shape
// (literal, object literal, step-method chain, collection chain, arithmetic,
// comparison); none matches a top-level `&&` / `||`. Without this evaluator such
// a return falls through to engine.Execute, whose default (no-collection-
// methods) converter trips the ADR 2.2 gate on a collection-method operand
// ("convert logical left: collection methods ... are not allowed in specs or
// query filters") -- even though collection methods ARE allowed in a logic body
// (memql#2579). No DSL change is needed: the fix restores logic-body evaluation
// for the whole class of `<expr> && <expr>` / `<expr> || <expr>` terminal
// returns.
//
// It claims the expression only when a depth-0, outside-quotes `&&` / `||` is
// present, so it never hijacks a string literal containing `&&` / `||`, nor a
// single comparison / arithmetic atom (those are handled by the earlier
// short-circuits). Placement in EvaluateLocalExpr is AFTER the arithmetic and
// comparison attempts so a top-level `*ArithmeticExpr` / `*BinaryComparisonExpr`
// keeps its existing route; only a genuine boolean connective lands here.
func tryEvaluateLogicalLocally(expr string, evaluator *Evaluator) (any, bool, error) {
	norm := stripRedundantOuterParens(normalizeConditionOperators(strings.TrimSpace(expr)))
	// normalizeConditionOperators rewrites `&&` -> `;` and `||` -> `,` outside
	// quotes; if neither split finds a depth-0 separator there is no top-level
	// boolean connective and the expression is not ours.
	if len(splitConditionParts(norm, ',')) == 1 && len(splitConditionParts(norm, ';')) == 1 {
		return nil, false, nil
	}
	val, err := evalLogicalString(expr, evaluator)
	if err != nil {
		return nil, false, err
	}
	return val, true, nil
}

// evalLogicalString evaluates a boolean expression with the same OR / AND /
// paren / `!` precedence as Evaluator.EvaluateCondition, but resolves each ATOM
// through the logic-time local surface (EvaluateLocalExpr) first. That ordering
// is load-bearing: EvaluateCondition's atom path renders a bare collection- or
// step-method chain such as `existing.empty()` as a truthy literal string
// (constant-true), whereas EvaluateLocalExpr resolves it against the step
// result. Only when EvaluateLocalExpr declines (a field-led comparison /
// `exists(...)` atom like `args.event.payload.node.type == "bff"`) do we fall
// back to EvaluateCondition.
func evalLogicalString(s string, evaluator *Evaluator) (bool, error) {
	norm := stripRedundantOuterParens(normalizeConditionOperators(strings.TrimSpace(s)))

	// OR (lowest precedence): any part true.
	if parts := splitConditionParts(norm, ','); len(parts) > 1 {
		for _, p := range parts {
			v, err := evalLogicalString(p, evaluator)
			if err != nil {
				return false, err
			}
			if v {
				return true, nil
			}
		}
		return false, nil
	}

	// AND: all parts true.
	if parts := splitConditionParts(norm, ';'); len(parts) > 1 {
		for _, p := range parts {
			v, err := evalLogicalString(p, evaluator)
			if err != nil {
				return false, err
			}
			if !v {
				return false, nil
			}
		}
		return true, nil
	}

	// Leading NOT.
	if rest, ok := stripLeadingBang(norm); ok {
		v, err := evalLogicalString(rest, evaluator)
		if err != nil {
			return false, err
		}
		return !v, nil
	}

	// Atom: no top-level connective (so tryEvaluateLogicalLocally, reached via
	// EvaluateLocalExpr, declines immediately -- no infinite recursion).
	atom := strings.TrimSpace(norm)
	if val, handled, err := EvaluateLocalExpr(atom, evaluator); err != nil {
		return false, err
	} else if handled {
		return isTruthy(val), nil
	}
	return evaluator.EvaluateCondition(atom)
}
