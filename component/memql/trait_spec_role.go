package memql

// trait_spec_role.go — retired filter-usage guard.
//
// The trait-vs-spec ROLE split (#2034 / C4) keyed a query-`filter`
// restriction on Spec.IsTrait: an authorization `spec` (anything not a
// trait) referenced by bare name in a filter was rejected. The
// spec/shape binding redesign (epic #2281) supersedes that role model
// with a BINDING model:
//
//   - trait <name>        = the deliberately UNBOUND row predicate.
//   - spec <bound> <name> = signature-bound; a concept- / @row-shape
//     binding -> row-spec (SQL pushdown), an @actor-shape binding ->
//     context-spec (in-process; the engine inlines + substitutes the
//     auth envelope when such a predicate appears in a filter).
//
// Both kinds are now legitimately referenceable in a query `filter`
// (e.g. the forge validation/approval queues gate the whole read on an
// @actor-bound caller predicate, exactly as they did with the former
// actor-reading trait). So the bare-reference restriction is retired:
// rejectAuthzSpecInFilter is a no-op kept only so the single call site
// (resolvePlanSpecs) and its test stay valid until they are removed.

// rejectAuthzSpecInFilter is retired (epic #2281). It always returns
// nil; spec/trait references in a query filter are governed by the
// binding model + the spec resolver, not by a role-split guard.
func rejectAuthzSpecInFilter(expr ExpressionNode, specs *SpecRegistry) error {
	_ = expr
	_ = specs
	return nil
}
