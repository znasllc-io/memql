package memql

import (
	"strings"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/tiers"
)

// expr_lower_scope.go -- THE lowering of edition-2026 pushdown constructs, as
// one function over a set of constructs and the scope they are lowered in
// (epic memql#5363, task memql#5366).
//
// Two kinds of caller make a query, spec or trait callable, and both run
// lowerPushdownSet:
//
//   - the Init pass (lowerAllPushdownPositions), over every construct of the
//     tree, in the engine's own registries;
//   - every authoring path (authoring_lower.go), over the constructs it is
//     about to make callable -- the Gate-1 sandbox behind define, validate and
//     stage; the promote and stage registrations; and the boot and cross-node
//     re-hydration of stored v1:authoring:construct rows -- in the same
//     registries with the authored layers on top.
//
// There is no second implementation, which is the point: a session-authored
// construct is lowered by exactly the code a tree construct is, checked by the
// same dry compile, and refused with the same three-part message.

// pushdownScope is what lowering a construct reads besides the construct
// itself: the concepts its fields are checked against, the shapes a spec may
// bind, and the predicates (specs and traits) a body may apply.
//
// The Init pass's scope is the engine's own registries. An authoring path's is
// the same registries with the authored layers on top -- the owner's staged
// and session specs, and the constructs being defined together -- so a query
// applying a spec declared beside it in one bundle is lowered against that
// spec, exactly as the tree's queries are lowered against the tree's specs.
//
// engineFree marks the one scope with no engine behind it: an engine-free
// validation (ValidateBundle, AuthorSessionBundle) holds the bundle's shapes
// and concepts but none of the engine's shapes or specs, and its predicate is
// nil. It lowers everything it can see and DEFERS what it cannot -- a spec
// bound to a shape the bundle does not declare, a predicate kind check -- to
// the engine-aware lowering every registration path runs, rather than refusing
// a construct for naming something that exists where it cannot look.
type pushdownScope struct {
	concepts   memoryNodes.Registry
	shapes     *ShapeRegistry
	predicate  func(name string) (*Spec, bool)
	engineFree bool
	// bindingsResolved marks a set whose spec bindings a pass has already
	// resolved -- the Init pass, where resolveSpecBindings runs over every
	// spec first and a spec it could not bind keeps an empty kind. Such a spec
	// is not bound again: specLowerEnv refuses it, in the v1 spelling, as the
	// Init pass always has.
	bindingsResolved bool
}

// pushdownFailure is one construct the lowering refused, and at which step.
// Exactly one of spec and fn is set.
type pushdownFailure struct {
	spec  *Spec
	fn    *Function
	phase string // "lower", "lower:dry-compile" or "lower:refine"
	err   error
}

// lowerPushdownSet lowers and checks one set of specs, traits and queries that
// become callable together, against one scope. The order is the Init order,
// and it is load-bearing:
//
//  1. every v1 spec's BINDING is resolved first (its kind: row or context),
//     because the next step checks each body's predicate applications against
//     the kinds of the specs they apply;
//  2. every v1 spec and trait BODY is lowered into its Expr;
//  3. every lowered spec is DRY-COMPILED, after all of them are lowered, so a
//     spec applying another is compiled against a scope holding both;
//  4. every v1 query filter is lowered AGAIN with the predicates in hand (the
//     kind check the lowering at compile could not make), its registered tree
//     dry-compiled, and its refine clause validated.
//
// Specs are mutated in place (Kind, Expr, ExprSource). A spec that fails to
// lower keeps a nil Expr, which every executor path refuses by name. Queries
// are only checked: their IR was built when they were compiled, and that first
// lowering is the one registered.
func lowerPushdownSet(specs []*Spec, queries []*Function, scope pushdownScope) []pushdownFailure {
	if scope.concepts == nil {
		// An engine without a concept registry (a bare test engine): bindings
		// and fields then resolve against nothing, rather than panicking on a
		// nil registry.
		scope.concepts = memoryNodes.NewRegistry(nil)
	}
	var failures []pushdownFailure
	fail := func(f pushdownFailure) { failures = append(failures, f) }
	lowered := map[*Spec]bool{}
	skip := map[*Spec]bool{} // a binding refused, or deferred by an engine-free scope

	for _, spec := range specs {
		if spec == nil || spec.Lambda == nil || spec.Kind != "" || scope.bindingsResolved {
			continue
		}
		if !spec.IsTrait && strings.TrimSpace(spec.BoundName) == "" {
			continue // no binding at all: specLowerEnv refuses it below
		}
		if scope.engineFree && !scope.canResolveBinding(spec) {
			skip[spec] = true // deferred: the binding may be a shape only the engine holds
			continue
		}
		if err := resolveOneSpecBinding(spec, scope.shapes, scope.concepts); err != nil {
			fail(pushdownFailure{spec: spec, phase: "lower", err: err})
			skip[spec] = true
		}
	}
	for _, spec := range specs {
		if spec == nil || spec.Lambda == nil || skip[spec] {
			continue
		}
		if err := lowerSpecBody(spec, scope); err != nil {
			fail(pushdownFailure{spec: spec, phase: "lower", err: err})
			continue
		}
		lowered[spec] = true
	}
	for _, spec := range specs {
		if !lowered[spec] {
			continue
		}
		if err := checkLoweredSpec(spec, scope); err != nil {
			fail(pushdownFailure{spec: spec, phase: "lower:dry-compile", err: err})
		}
	}
	for _, fn := range queries {
		if phase, err := checkQueryConstruct(fn, scope); err != nil {
			fail(pushdownFailure{fn: fn, phase: phase, err: err})
		}
	}
	return failures
}

// canResolveBinding reports whether an engine-free scope can see what a spec
// binds: nothing for a trait, a shape or concept it holds for a spec. A spec
// with no binding at all counts as resolvable -- its refusal does not depend on
// what the scope can see. It asks the question the binding resolver will
// (specBindingShape / specBindingConcept, the spec's own domain first), so a
// bound name two domains declare is seen exactly when the resolver can bind it.
func (s pushdownScope) canResolveBinding(spec *Spec) bool {
	if spec.IsTrait || strings.TrimSpace(spec.BoundName) == "" {
		return true
	}
	if _, ok := specBindingShape(s.shapes, spec); ok {
		return true
	}
	c, err := specBindingConcept(s.concepts, spec)
	return err == nil && c != nil
}

// lowerSpecBody lowers one v1 spec or trait body, whose binding is resolved,
// into its Expr.
func lowerSpecBody(spec *Spec, scope pushdownScope) error {
	env, err := specLowerEnv(spec, scope.shapes, scope.concepts)
	if err != nil {
		return err
	}
	env.Predicate = scope.predicate
	ir, err := Lower(spec.Lambda.Body, env)
	if err != nil {
		return err
	}
	spec.Expr = ir
	spec.ExprSource = canonicalExpression(ir)
	return nil
}

// checkLoweredSpec dry-compiles a lowered spec body. A context spec is
// evaluated in process against the actor, never compiled, so it is not.
func checkLoweredSpec(spec *Spec, scope pushdownScope) error {
	if spec == nil || spec.Expr == nil || spec.Kind == SpecKindContext {
		return nil
	}
	// The concept the spec is bound to, resolved as its binding was (own
	// domain first) -- not by a bare trailing-segment scan, which answers
	// nothing for a name two domains declare.
	conceptContext := ""
	if !spec.IsTrait {
		if c, err := specBindingConcept(scope.concepts, spec); err == nil && c != nil {
			conceptContext = c.Name
		}
	}
	return checkLoweredTree(spec.Expr, conceptContext, nil, tiers.PositionSpecBody, scope)
}

// checkQueryConstruct is the second look at a query: its v1 filter lowered
// again with the scope's predicates, its registered tree dry-compiled, and its
// refine clause validated. The returned phase names the step that refused.
func checkQueryConstruct(fn *Function, scope pushdownScope) (string, error) {
	if fn == nil {
		return "", nil
	}
	var concept *memoryNodes.Concept
	if scope.concepts != nil && fn.BoundConcept != "" {
		if c, err := scope.concepts.Get(fn.BoundConcept); err == nil {
			concept = c
		}
	}
	if fn.V1Filter != nil {
		args := argTypesFromSchema(fn.ArgsSchema)
		if _, err := lowerQueryFilter(fn.V1Filter, concept, args, scope.predicate); err != nil {
			return "lower", err
		}
		if err := checkLoweredTree(fn.Expr, fn.BoundConcept, args, tiers.PositionQueryFilter, scope); err != nil {
			return "lower:dry-compile", err
		}
	}
	if refine := refineIn(fn.Expr); refine != nil {
		if err := validateRefineIn(fn, refine, scope.predicate, concept); err != nil {
			return "lower:refine", err
		}
	}
	return "", nil
}

// engineScope is the Init pass's scope: this engine's registries.
func (e *MemQLEngine) engineScope(shapes *ShapeRegistry) pushdownScope {
	return pushdownScope{concepts: e.concepts, shapes: shapes, predicate: e.predicateLookup(), bindingsResolved: true}
}

// specKeyword is the declaration keyword a spec was written with.
func specKeyword(spec *Spec) string {
	if spec != nil && spec.IsTrait {
		return "trait"
	}
	return "spec"
}
