package memql

import (
	"fmt"
	"strings"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// logic_cond_bare_identifier_validator.go closes the residual memql#3024
// records, and replaces logic_cond_ambient_predicate_validator.go.
//
// #2962 is about a cond PREDICATE that is a silent constant. Its fix resolves
// an `args.`-rooted comparison during arg expansion; memql#3024 threads the
// ambient envelope through the same path, so `actor.` / `config.` /
// `partition` / `now` now resolve there too and no longer need refusing.
//
// What neither resolves is a BARE identifier:
//
//	logic roleGate {
//	  args { role string @required }
//	  body { return cond(role == "owner", "yes", "no") }
//	}
//
// In a single-statement body there are no locals, so bare `role` resolves
// against a nil scope and the comparison is constant -- `"no"` for every input,
// loading green and linting green. Bare `role` for an arg is an authoring
// mistake (args are read as `args.role`), but the failure mode is a wrong
// answer with no diagnostic, which is exactly what #2962 exists to eliminate.
//
// # Why this cannot simply reject every bare identifier
//
// The legitimate shape is live in the tree, and it is the one every real cond
// role gate uses:
//
//	role    := actor.role ?? ""                    // dsl/deployment/logic.memql
//	allowed := cond(role == "owner", true, false)  // dsl/forge/logic.memql
//
// There, `role` IS in scope -- bound by a preceding step, and resolved by the
// LogicRunner when it walks the steps. Rejecting it would break boot on every
// node, since this validator sits on the load path for every binary. So the
// rule fires only when NO local of that name is bound by an earlier step,
// which is precisely the case where there is nothing for the identifier to
// resolve against.
//
// # Scope is deliberately narrow
//
// Only the PREDICATE position of a cond, and only a comparison that IS the
// predicate node (or a nested cond's predicate). The walk never descends into
// a lambda body, where a bare identifier is a lambda-scope reference that
// resolves correctly -- the same STOP the branch-value validator takes for the
// same reason. Reserved ambient roots are skipped outright: they are resolved
// by the envelope threading, never locals, and never payload fields.
func validateLogicCondBareIdentifierPredicate(funcDef *languageParser.FunctionDef) error {
	if funcDef == nil || funcDef.Type != languageParser.FunctionTypeLogic {
		return nil
	}
	auto, ok := funcDef.Body.(*languageParser.AutomationDef)
	if !ok || auto == nil {
		return nil
	}

	// Every local this body binds, collected UP FRONT rather than accumulated
	// as the walk proceeds.
	//
	// The binding name is the step's ID. StepDef.Name is empty for the
	// assignment lowering -- measured, not assumed, and the LogicRunner keys
	// its own bindings off step.ID too (SetStepResult).
	//
	// Source order is NOT binding order: the compiler topologically sorts
	// steps by dependency before the runner walks them ("the compiler already
	// topologically sorts them by dependency, so a forward pass guarantees a
	// step's references are bound before we hit it" -- logic_runner.go). A
	// rule counting only PRECEDING steps would reject a body that binds a
	// local textually later and resolves perfectly at runtime, so taking the
	// whole set is the CORRECT rule rather than merely the lenient one.
	locals := make(map[string]struct{}, len(auto.Steps))
	for _, step := range auto.Steps {
		if id := strings.TrimSpace(step.ID); id != "" {
			locals[id] = struct{}{}
		}
	}

	for _, step := range auto.Steps {
		for _, node := range condPredicateNodes(step) {
			bad := unboundBareComparison(node, locals)
			if bad == nil {
				continue
			}
			field := bareFieldName(bad)
			return fmt.Errorf(
				"logic %q: cond predicate compares bare identifier %q, which this body never binds "+
					"as a local -- so it resolves against an empty scope and the comparison is a "+
					"CONSTANT, silently taking the ELSE branch for every input. A gate written this "+
					"way is open or closed by accident rather than by its condition (memql#3024, "+
					"memql#2962). If %q is an argument, read it as `args.%s`; if it is meant to be a "+
					"local, bind it in a step first (`%s := ...`)",
				funcDef.Name, field, field, field, field)
		}
	}
	return nil
}

// condPredicateNodes returns the predicate expression of every cond in this
// step, including conds nested in either branch.
//
// The two shapes mirror how the loader lowers a body: an assignment step
// `w := cond(...)` is FLATTENED into a positional FunctionStepConfig with arg
// "0" as the predicate, while the terminal `return cond(...)` keeps its
// CondExpr node. The single-statement body memql#3024 reports is only
// reachable through the second.
func condPredicateNodes(step languageParser.StepDef) []languageParser.ExpressionNode {
	var out []languageParser.ExpressionNode
	if fc, isFn := step.Config.(*languageParser.FunctionStepConfig); isFn && fc != nil && strings.EqualFold(fc.Name, "cond") {
		if node, isExpr := fc.Args["0"].(languageParser.ExpressionNode); isExpr {
			out = append(out, node)
			// A cond branch may itself be a cond; its predicate is the same
			// defect one level down.
			for _, key := range []string{"1", "2"} {
				if branch, isExpr := fc.Args[key].(languageParser.ExpressionNode); isExpr {
					out = append(out, condPredicatesWithin(branch)...)
				}
			}
		}
	}
	if qc, isQ := step.Config.(*languageParser.QueryStepConfig); isQ && qc != nil && qc.Query != nil {
		out = append(out, condPredicatesWithin(qc.Query)...)
	}
	return out
}

// condPredicatesWithin returns the predicate of every cond reachable from
// node, including conds nested in either branch. It intentionally does not
// walk any other node type: a cond buried inside a lambda or a method chain
// evaluates in a scope this validator cannot see, and guessing there is how a
// load-path rule acquires false positives.
func condPredicatesWithin(node languageParser.ExpressionNode) []languageParser.ExpressionNode {
	cond, ok := node.(*languageParser.CondExpr)
	if !ok || cond == nil {
		return nil
	}
	out := []languageParser.ExpressionNode{cond.Condition}
	out = append(out, condPredicatesWithin(cond.Then)...)
	out = append(out, condPredicatesWithin(cond.Else)...)
	return out
}

// unboundBareComparison returns the comparison when node is a Field-led
// comparison over a SINGLE bare segment that names neither a bound local nor a
// reserved ambient root. Returns nil for anything else.
func unboundBareComparison(node languageParser.ExpressionNode, locals map[string]struct{}) *languageParser.ComparisonExpr {
	cmp, ok := node.(*languageParser.ComparisonExpr)
	if !ok || cmp == nil {
		return nil
	}
	parts := cmp.Field.Parts
	if len(parts) == 0 {
		raw := strings.TrimSpace(cmp.Field.Raw)
		if raw == "" {
			return nil
		}
		parts = strings.Split(raw, ".")
	}
	// A dotted path is `args.x` (resolved by expansion), an ambient root
	// (resolved by the envelope), or a row/lambda reference that must stay
	// lazy. None of them is this defect.
	if len(parts) != 1 {
		return nil
	}
	name := strings.TrimSpace(parts[0])
	if name == "" {
		return nil
	}
	// `partition` / `now` are single-segment ambient roots, resolved by the
	// envelope threading rather than by any local.
	if isAmbientRoot(name) {
		return nil
	}
	if _, bound := locals[name]; bound {
		return nil
	}
	return cmp
}

// bareFieldName renders the compared identifier for the diagnostic.
func bareFieldName(cmp *languageParser.ComparisonExpr) string {
	if len(cmp.Field.Parts) > 0 {
		return strings.Join(cmp.Field.Parts, ".")
	}
	return strings.TrimSpace(cmp.Field.Raw)
}
