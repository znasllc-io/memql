package memql

import (
	"context"
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
			if bad := unboundBareComparison(node, locals); bad != nil {
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
			if bad, reason := unresolvableAmbientComparison(node); bad != nil {
				field := bareFieldName(bad)
				return fmt.Errorf(
					"logic %q: cond predicate compares %q, %s -- so it resolves to nothing and the "+
						"comparison is a CONSTANT, silently taking the ELSE branch for every input, "+
						"exactly as an unbound bare identifier would (memql#3024, memql#2962). The "+
						"ambient envelope carries `actor.` / `config.` / `partition` / `now` and "+
						"nothing else; see buildAmbientEnvelope",
					funcDef.Name, field, reason)
			}
		}
	}
	return nil
}

// unresolvableAmbientComparison returns the comparison when node compares an
// ambient-looking path that CANNOT resolve on any evaluation path, plus the
// reason for the diagnostic. Returns nil for anything resolvable.
//
// Two ways a path is unresolvable, and both used to be refused by the
// validator this file replaced -- so leaving either un-refused is a regression
// that trades a loud boot error for a silent gate:
//
//   - the ROOT is reserved but supplied by nothing (`trace.`). No envelope has
//     ever carried it.
//   - the root IS carried but the LEAF is not a key the envelope has
//     (`actor.partitions`, which CLAUDE.md's argument-resolution table lists
//     but ActorEnvelopeMap does not emit; `config.someFlag`, which is not in
//     the policy_exposable allow-list; or a plain typo like `actor.rol`).
//
// The reference envelope is built with no context and no engine ON PURPOSE.
// ActorEnvelopeMap(nil) and BuildPolicyConfigCtx(nil) both emit their COMPLETE
// key set with denying/empty values, so the reference carries exactly the set
// of resolvable paths and never depends on who is calling. That is what makes
// this decidable at load time, where there is no caller.
func unresolvableAmbientComparison(node languageParser.ExpressionNode) (*languageParser.ComparisonExpr, string) {
	cmp, ok := node.(*languageParser.ComparisonExpr)
	if !ok || cmp == nil {
		return nil, ""
	}
	parts := comparisonFieldParts(cmp)
	if len(parts) == 0 {
		return nil, ""
	}
	root := strings.TrimSpace(parts[0])
	if isReservedUnsuppliedRoot(root) {
		return cmp, fmt.Sprintf("whose root %q is a reserved name that no evaluation envelope supplies", root)
	}
	if !isAmbientRoot(root) {
		return nil, ""
	}
	// A bare root with no leaf (`partition`, `now`) is the whole value and
	// always resolves; only a dotted path can miss.
	if len(parts) == 1 {
		return nil, ""
	}
	if _, found := getNestedValue(ambientReferenceEnvelope(), strings.Join(parts, ".")); found {
		return nil, ""
	}
	return cmp, fmt.Sprintf("which the %q envelope does not carry", root)
}

// condPredicateNodes returns the predicate expression of every cond in this
// step, including conds nested in either branch.
//
// The two shapes mirror how the loader lowers a body: an assignment step
// `w := cond(...)` is FLATTENED into a positional FunctionStepConfig with arg
// "0" as the predicate, while the terminal `return cond(...)` keeps its
// CondExpr node. The single-statement body memql#3024 reports is only
// reachable through the second.
//
// A *ForEachStepConfig body is deliberately NOT walked, and if you extend this
// to walk one you must ALSO change how `locals` is seeded. A loop's binding
// name lives in ForEachStepConfig.As / .Index, NOT in step.ID -- the parser
// gives such a step the synthetic id `forEach_<var>_<n>` -- so the caller's
// step.ID-based `locals` set does not contain the loop variable. Walking the
// body without fixing that turns every `cond(<loopvar> == ...)` inside a loop
// into a load rejection, which is a boot failure on every node. The two halves
// were measured together; keep them that way.
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
	parts := comparisonFieldParts(cmp)
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

// comparisonFieldParts returns the compared field's dotted segments, falling
// back to splitting the raw text when the parser did not populate Parts.
func comparisonFieldParts(cmp *languageParser.ComparisonExpr) []string {
	if len(cmp.Field.Parts) > 0 {
		return cmp.Field.Parts
	}
	raw := strings.TrimSpace(cmp.Field.Raw)
	if raw == "" {
		return nil
	}
	return strings.Split(raw, ".")
}

// ambientReferenceEnvelope is the ambient envelope built with no caller and no
// engine: every key present, with the denying / empty values
// ActorEnvelopeMap(nil) and BuildPolicyConfigCtx(nil) supply. Its KEYS are
// exactly the set of paths that can resolve at runtime, which is what lets the
// load-path validator decide resolvability without a caller.
//
// Values are never read from it -- only key presence -- so building it per
// call is deliberate: it must not be a package-level cache that could go stale
// against a changed allow-list.
func ambientReferenceEnvelope() map[string]any {
	return buildAmbientEnvelope(context.Background(), nil)
}

// bareFieldName renders the compared identifier for the diagnostic.
func bareFieldName(cmp *languageParser.ComparisonExpr) string {
	if len(cmp.Field.Parts) > 0 {
		return strings.Join(cmp.Field.Parts, ".")
	}
	return strings.TrimSpace(cmp.Field.Raw)
}
