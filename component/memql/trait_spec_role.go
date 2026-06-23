package memql

import "fmt"

// trait_spec_role.go enforces the trait-vs-spec ROLE split introduced
// by the DSL grammar redesign (C4 / memql#2034). `trait` and `spec`
// share one runtime contract (an atomic boolean predicate stored in
// the SpecRegistry, discriminated by Spec.IsTrait) but carry distinct
// ROLES:
//
//   - trait <name> = a DATA / STATE predicate. Reads payload fields /
//     row intrinsics, so it classifies as SpecKindRow. Usable in a
//     query `filter` slot (it pushes down to SQL).
//
//   - spec <name>  = an AUTHORIZATION predicate. May read actor.*, so
//     it classifies as SpecKindContext. Usable in a `requires` slot /
//     via spec("name") (it evaluates in-process against the auth
//     envelope).
//
// The split is enforced at two altitudes:
//
//   1. Declaration (validateSpecRole, called from specDeclToSpec): a
//      `spec` whose body reads only row fields is really a data
//      predicate and is rejected -- it must be declared a `trait`.
//      Allowing it would let a row predicate leak into the `requires`
//      / authorization slot, where it is meaningless.
//
//   2. Filter usage (rejectAuthzSpecInFilter, called from
//      resolvePlanSpecs): a `spec` (IsTrait == false, an authorization
//      predicate) referenced by BARE NAME inside a query `filter` is
//      rejected -- authorization belongs in a `requires` slot, not in
//      the row-filter. (Bare references parse to a
//      SpecReferenceExpression; the sanctioned `spec("name")` builtin
//      invocation is a function call, not a bare reference, so the
//      per-row-authz admin-gate pattern is unaffected.)
//
// The reverse direction -- a `trait` (data predicate) used in the
// authorization / `requires` slot -- is already rejected by
// EvaluateSpec, which refuses any predicate that is not SpecKindContext
// (a reclassified trait is SpecKindRow). See spec_evaluator.go.
//
// This role check is distinct from the slot-KIND check owned by the
// C2/C3 validator rework: this gate keys on the trait-vs-spec ROLE
// (Spec.IsTrait), not on which slot kind a construct occupies.

// validateSpecRole enforces the declaration-time half of the role
// split: a `spec` must be an authorization predicate (it may not be a
// pure data/state predicate). A row-only `spec` body is the mislabeled
// case -- it should be a `trait`.
//
// The trait side (a `trait` reading actor.*) is intentionally NOT
// rejected here: the legacy per-row-authz pattern still composes a
// role check into a query `filter` as a trait, and that migration onto
// the dedicated `requires` slot lands with the slot itself. The authz
// invocation path (EvaluateSpec) already rejects a data (row) predicate
// used as a gate, so a reclassified data trait cannot be abused as
// authorization in the meantime.
func validateSpecRole(isTrait bool, kind SpecKind, name string) error {
	if !isTrait && kind == SpecKindRow {
		return fmt.Errorf(
			"spec %q reads only payload / row intrinsics, which makes it a data/state predicate -- declare it as a `trait` (usable in a query `filter`). A `spec` is an authorization predicate that reads actor.* and is usable in a `requires` slot",
			name,
		)
	}
	return nil
}

// rejectAuthzSpecInFilter walks a query filter expression and rejects
// any bare reference (SpecReferenceExpression) to a registered
// authorization `spec` (IsTrait == false). Traits (data predicates)
// pass; an authorization spec referenced by bare name in a filter is
// the anti-pattern the role split forbids.
//
// Only bare references are inspected. The `spec("name")` builtin
// invocation -- the sanctioned per-row-authz admin gate -- is a
// function call, not a SpecReferenceExpression, so it is unaffected.
func rejectAuthzSpecInFilter(expr ExpressionNode, specs *SpecRegistry) error {
	if expr == nil || specs == nil {
		return nil
	}
	switch node := expr.(type) {
	case *SpecReferenceExpression:
		if node == nil {
			return nil
		}
		spec, err := specs.Get(node.Name)
		if err != nil {
			// Unknown reference -- leave the error to the resolver, which
			// produces the canonical "unknown spec" diagnostic.
			return nil
		}
		if spec != nil && !spec.IsTrait {
			return fmt.Errorf(
				"authorization spec %q may not be referenced by bare name in a query `filter` -- it reads actor.* and belongs in a `requires` slot (or invoke it as an in-process gate via spec(%q)). Only a `trait` (a data/state predicate over payload / row fields) is usable in a filter",
				node.Name, node.Name,
			)
		}
		return nil
	case *LogicalExpression:
		if err := rejectAuthzSpecInFilter(node.Left, specs); err != nil {
			return err
		}
		return rejectAuthzSpecInFilter(node.Right, specs)
	case *RelationshipExpression:
		return rejectAuthzSpecInFilter(node.Target, specs)
	default:
		return nil
	}
}
