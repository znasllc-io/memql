package memql

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/ast"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/language/tiers"
	"github.com/znasllc-io/memql/component/memql/baseloader"
	"github.com/znasllc-io/memql/core/id"
)

// expr_lower_load.go -- where Lower runs (epic memql#5363, task memql#5366;
// D11: "Lower(ast) runs in MemQLEngine.Init for every P-position expression;
// a node that does not lower is a load refusal").
//
// # Two moments, because the registries fill in order
//
// Init loads queries BEFORE specs (functions can reference specs, and the
// spec loader is the later pass). So a v1 query filter is lowered twice:
//
//  1. at QUERY LOAD (tryParseNewFunctionSyntax), with the bound concept and
//     the declared arguments but no spec registry -- which is everything the
//     IR needs, since a predicate application lowers to a spec reference
//     whatever the spec's kind. The query is registered with that IR, and a
//     refusal here is an ordinary load failure the unified loader records.
//  2. in the INIT PASS (lowerAllPushdownPositions), after resolveSpecBindings
//     has resolved every binding and kind: the query's lambda is lowered
//     again with the registry, which is when `isAdmin(row)` against a context
//     spec can be refused. The registered IR is kept -- the second lowering is
//     a check, and resolveConstructReferences may already have qualified the
//     names in the first.
//
// A v1 spec or trait body is lowered once, in the Init pass: its binding (a
// concept, a shape, nothing for a trait) is only known once shapes are loaded
// and the binding is resolved.
//
// # What the Init pass adds beyond Lower
//
//   - every comparison of every lowered tree is DRY-COMPILED with a typed
//     placeholder per argument, through the SQL compiler the executor uses
//     (compileComparisonExpressionWithContext): a comparison the compiler
//     would refuse at the first call -- a concept the registry does not know,
//     an operator a column does not take -- refuses at load instead;
//   - a negation over a spec reference that reaches a traversal is refused (a
//     direct one Lower already refuses; one through a spec needs the
//     registry);
//   - every refine lambda is checked (validateRefine), static cost included.
//
// Every failure lands on the LoadReport as a skip, so strict boot refuses it --
// in this repository's tree and in a product bundle mounted at MEMQL_DSL_PATH
// alike, since both load through here.

// init wires the plan-constant evaluator (expr_plan_const.go) to EvalExpr, the
// in-process evaluator: a row-independent subtree of a pushdown predicate is
// evaluated once per call with the bindings argument expansion hands it.
// Wired from an init so it is set before any engine exists, and never written
// again outside tests -- argument expansion reads it without a lock.
func init() {
	planConstantEvaluator = func(ctx context.Context, n ast.ExpressionNode, bindings map[string]any) (any, error) {
		v, err := EvalExpr(ctx, n, MapScope(bindings), EvalOptions{CanonicalID: canonicalIDForPlanConstant})
		if err != nil {
			return nil, err
		}
		if IsAbsent(v) {
			// The expansion's absence rules speak nil; the sentinel is
			// EvalExpr's top-level spelling of the same value.
			return nil, nil
		}
		return v, nil
	}
}

// canonicalIDForPlanConstant backs canonicalId(v, "concept") inside a plan
// constant. Argument expansion is engine-free, so this is the registry-free
// half of canonicalizeIdValue: a bare slug is composed under the concept, a
// canonical id of that concept passes, and one of ANOTHER concept is refused
// rather than rewritten. That the concept exists is checked at load instead,
// by the Init pass's dry compile of the comparison the value lands in.
func canonicalIDForPlanConstant(_ context.Context, value any, concept string) (string, error) {
	text := strings.TrimSpace(fmt.Sprint(value))
	concept = strings.TrimSpace(concept)
	if text == "" {
		return "", nil
	}
	if !strings.ContainsRune(text, ':') {
		return id.BuildNodeId(concept, text), nil
	}
	if strings.HasPrefix(text, concept+":") {
		return text, nil
	}
	got, _, err := id.ParseNodeId(text)
	if err != nil {
		return "", fmt.Errorf("canonicalId: malformed id %q: %w", text, err)
	}
	if got != "" && got != concept {
		return "", fmt.Errorf("canonicalId: id %q is under concept %q, expected %q", text, got, concept)
	}
	return text, nil
}

// argTypesFromSchema reads a query's declared arguments as Lower's ArgTypes.
// A query with no args block has none, which is an empty, non-nil map: every
// `args.x` in its filter is then an undeclared argument.
func argTypesFromSchema(schema *ArgsSchemaConfig) map[string]ArgType {
	out := map[string]ArgType{}
	if schema == nil {
		return out
	}
	for _, f := range schema.Fields {
		if f == nil || strings.TrimSpace(f.Name) == "" {
			continue
		}
		out[f.Name] = ArgType(f.Type)
	}
	return out
}

// lowerQueryFilter lowers a query's v1 filter lambda. concept is the bound
// concept (nil when the query binds none, whose fields are then unchecked);
// predicate is nil at load and the spec lookup in the Init pass.
func lowerQueryFilter(lam *languageParser.LambdaExpr, concept *memoryNodes.Concept, args map[string]ArgType, predicate func(string) (*Spec, bool)) (ExpressionNode, error) {
	if lam == nil || len(lam.Params) != 1 {
		return nil, fmt.Errorf("a query filter is a lambda of one parameter, the row: filter row => <predicate>")
	}
	ir, err := Lower(lam.Body, LowerEnv{
		Position:  tiers.PositionQueryFilter,
		Param:     lam.Params[0],
		Concept:   concept,
		Args:      args,
		Predicate: predicate,
	})
	var lerr *LowerError
	if errors.As(err, &lerr) && lerr.Clause == "" {
		// The filter reaches the parser folded into the struct query's
		// `return` line; anchor the refusal to the lambda body there so an
		// authoring diagnostic can find the author's column again.
		lerr.Clause, lerr.Anchor = "filter", nodeSpan(lam.Body)
	}
	return ir, err
}

// LowerQueryFilter lowers an edition-2026 query-filter lambda over the
// concept named conceptID, with the given declared arguments, against this
// engine's spec registry. It is the entry a query position outside the
// function loader uses -- a tool's @handler(query=...) -- so that position
// lowers exactly as a query filter does. The result is the filter alone; the
// caller joins it onto its own `concept == <id>` binding.
func (e *MemQLEngine) LowerQueryFilter(conceptID string, lam *languageParser.LambdaExpr, args map[string]ArgType) (ExpressionNode, error) {
	var concept *memoryNodes.Concept
	if e != nil && e.concepts != nil && strings.TrimSpace(conceptID) != "" {
		c, err := e.concepts.Get(strings.TrimSpace(conceptID))
		if err != nil {
			return nil, fmt.Errorf("lower a filter over %s: %w", conceptID, err)
		}
		concept = c
	}
	if args == nil {
		args = map[string]ArgType{}
	}
	return lowerQueryFilter(lam, concept, args, e.predicateLookup())
}

// predicateLookup is LowerEnv.Predicate over this engine's spec registry.
func (e *MemQLEngine) predicateLookup() func(string) (*Spec, bool) {
	return func(name string) (*Spec, bool) {
		if e == nil || e.specs == nil {
			return nil, false
		}
		spec, err := e.specs.Get(name)
		if err != nil || spec == nil {
			return nil, false
		}
		return spec, true
	}
}

// specLowerEnv resolves a v1 spec's binding into the env Lower reads: nothing
// for a trait, the shape's key -> stored-path map for a shape, the concept for
// a concept. It mirrors resolveOneSpecBinding's resolution order (shape
// first, then concept), which has already run and set spec.Kind.
func specLowerEnv(spec *Spec, shapes *ShapeRegistry, concepts memoryNodes.Registry) (LowerEnv, error) {
	env := LowerEnv{Position: tiers.PositionSpecBody}
	if spec.Lambda == nil || len(spec.Lambda.Params) != 1 {
		return env, fmt.Errorf("a spec or trait body is a lambda of one parameter: = row => <predicate>")
	}
	env.Param = spec.Lambda.Params[0]
	switch {
	case spec.IsTrait:
	case spec.BoundName == "":
		return env, fmt.Errorf("non-trait spec has no signature binding -- declare `spec <boundName> %s = row => ...`", spec.Name)
	default:
		if shape, ok := shapeLookup(shapes, spec.BoundName); ok {
			keys := make(map[string]string, len(shape.Template))
			for key, raw := range shape.Template {
				if s, isString := raw.(string); isString {
					keys[key] = extractNodePath(s)
				}
			}
			env.ShapeKeys = keys
		} else if concept, err := resolveConceptByTrailingSegment(concepts, spec.BoundName); err == nil && concept != nil {
			env.Concept = concept
		} else {
			return env, fmt.Errorf("binding %q resolves to neither an imported shape nor a concept", spec.BoundName)
		}
	}
	// D1: over an @actor shape the parameter IS the envelope and is spelled
	// `actor`; over rows it is not, since `actor` is the reserved root a row
	// predicate may otherwise read.
	switch {
	case spec.Kind == SpecKindContext && env.Param != "actor":
		return env, &LowerError{Node: ast.FormatExpr(spec.Lambda), Position: tiers.PositionSpecBody,
			Reason: fmt.Sprintf("the parameter of a spec over an @actor shape is the actor envelope, spelled `actor`, not `%s`", env.Param),
			Fix:    "Write `= actor => actor.role == \"admin\"`"}
	case spec.Kind != SpecKindContext && env.Param == "actor":
		return env, &LowerError{Node: ast.FormatExpr(spec.Lambda), Position: tiers.PositionSpecBody,
			Reason: "`actor` names the actor envelope, and this predicate is over rows",
			Fix:    "Name the row: `= row => ...`"}
	}
	return env, nil
}

// lowerAllPushdownPositions is the Init pass: lowerPushdownSet
// (expr_lower_scope.go) over every registered spec, trait and query of the
// tree, against the engine's own registries. Failures land on the report
// (strict boot refuses them); a spec that fails to lower keeps a nil Expr,
// which every executor path refuses by name rather than guessing a body for.
func (e *MemQLEngine) lowerAllPushdownPositions(report *LoadReport, specs *SpecRegistry, shapes *ShapeRegistry, functions *FunctionRegistry) []error {
	var specList []*Spec
	if specs != nil {
		// The registry hands out clones, so each lowered spec is written back
		// below, under its QUALIFIED key as the binding resolver writes it.
		specList = specs.List()
		sort.Slice(specList, func(i, j int) bool { return specList[i].Name < specList[j].Name })
	}
	var queries []*Function
	if functions != nil {
		snapshot := functions.Snapshot()
		keys := make([]string, 0, len(snapshot))
		for k := range snapshot {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		seen := map[*Function]bool{}
		for _, key := range keys {
			if fn := snapshot[key]; fn != nil && !seen[fn] {
				seen[fn] = true
				queries = append(queries, fn)
			}
		}
	}

	// A body applying another spec reads only that spec's kind, which the
	// binding resolver has already written into the registry the scope's
	// predicate lookup reads.
	failures := lowerPushdownSet(specList, queries, e.engineScope(shapes))

	refused := map[*Spec]bool{}
	var problems []error
	for _, f := range failures {
		keyword, name, origin := "query", "", ""
		if f.spec != nil {
			keyword, name, origin = specKeyword(f.spec), f.spec.Name, f.spec.Origin
			if f.phase == "lower" {
				refused[f.spec] = true
			}
		} else if f.fn != nil {
			name, origin = f.fn.Name, f.fn.Origin
		}
		problems = append(problems, fmt.Errorf("%s %q: %w", keyword, name, f.err))
		if report != nil {
			report.AddSkip(baseloader.Skip{Component: "memql.lower", Keyword: keyword, Name: name, File: origin, Phase: f.phase, Err: f.err.Error()})
		}
	}
	for _, spec := range specList {
		if spec == nil || spec.Lambda == nil || spec.Expr == nil || refused[spec] {
			continue
		}
		if err := specs.Upsert(QualifyConstruct(ConstructNamespaceForOrigin(spec.Origin), spec.Name), spec); err != nil {
			problems = append(problems, fmt.Errorf("%s %q: %w", specKeyword(spec), spec.Name, err))
		}
	}
	return problems
}

// checkLoweredTree is the backstop over one lowered tree: every comparison
// dry-compiled with placeholders, every negation checked for a traversal
// behind a spec reference. conceptContext is the concept the tree is bound to
// ("" for a trait or a traversal's row), args the declared argument types the
// placeholders are typed by, and ps the scope the tree was lowered in -- its
// concepts are what a `concept == <id>` literal is checked against and its
// predicates what a spec reference is looked through.
func checkLoweredTree(expr ExpressionNode, conceptContext string, args map[string]ArgType, position tiers.Position, ps pushdownScope) error {
	var errs []error
	var walk func(n ExpressionNode, concept string, scope *sqlElementScope)
	walk = func(n ExpressionNode, concept string, scope *sqlElementScope) {
		switch x := n.(type) {
		case nil:
		case *LogicalExpression:
			walk(x.Left, concept, scope)
			walk(x.Right, concept, scope)
		case *NotExpression:
			if treeReachesRelationshipVia(x.Target, ps.predicate, map[string]struct{}{}) {
				errs = append(errs, &LowerError{Node: canonicalExpression(x), Position: position,
					Reason: "the negation reaches a relationship traversal through a spec, and the executor does not compute the complement of a row set",
					Fix:    "Move the negation inside the traversal's predicate, or select the complement with the query's own filter"})
				return
			}
			walk(x.Target, concept, scope)
		case *ArrayPredicateExpression:
			if x.Method == ArrayMethodCount {
				if _, err := sqlOperatorForComparison(x.CountOp); err != nil {
					errs = append(errs, err)
				}
				return
			}
			walk(x.Pred, concept, scope.child())
		case *RelationshipExpression:
			walk(x.Target, "", nil)
		case *SortExpression:
			walk(x.Target, concept, scope)
		case *PaginateExpression:
			walk(x.Target, concept, scope)
		case *SelectExpression:
			walk(x.Target, concept, scope)
		case *TimestampExpression:
			walk(x.Target, concept, scope)
		case *DepthExpression:
			walk(x.Target, concept, scope)
		case *CountExpression:
			walk(x.Target, concept, scope)
		case *ShapeExpression:
			walk(x.Target, concept, scope)
		case *RefineExpression:
			walk(x.Target, concept, scope)
		case *ComparisonExpression:
			if err := dryCompileComparison(x, concept, args, scope, ps.concepts); err != nil {
				errs = append(errs, &LowerError{Node: canonicalExpression(x), Position: position, Reason: err.Error(),
					Fix: "Rewrite the comparison with a value the column or field takes, as in `row.status == \"open\"`"})
			}
		}
	}
	walk(expr, conceptContext, nil)
	return errors.Join(errs...)
}

// dryCompileComparison compiles one lowered comparison with its runtime-only
// value -- an argument, an actor reference, a plan constant -- replaced by a
// placeholder of the type it will have, so what is checked is the comparison's
// SHAPE: the operator against the field, the value's kind against the column.
// concepts is the registry a `concept == <id>` literal is checked against.
func dryCompileComparison(cmp *ComparisonExpression, conceptContext string, args map[string]ArgType, scope *sqlElementScope, concepts memoryNodes.Registry) error {
	probe := *cmp
	if fo, ok := cmp.Value.(*FieldOperand); ok {
		_, err := compileFieldOperandComparison(&probe, fo, scope)
		return err
	}
	// An actor-field comparison is folded to a constant when the query runs
	// (resolveActorComparisonsToConstants); it never compiles as a column.
	if len(cmp.Field.Parts) > 0 && strings.EqualFold(strings.TrimSpace(cmp.Field.Parts[0]), "actor") {
		return nil
	}
	intrinsic := ""
	if len(cmp.Field.Parts) == 1 {
		if info, ok := resolveIntrinsicField(cmp.Field.Parts[0]); ok {
			canonical, _ := canonicalIntrinsicFieldName(cmp.Field.Parts[0])
			intrinsic = canonical
			if info.kind == intrinsicFieldConcept && !isLiteralComparisonValue(cmp.Value) {
				// Only a LITERAL concept can be checked against the
				// registry; a placeholder would be refused as unknown.
				return nil
			}
		}
	}
	switch v := cmp.Value.(type) {
	case *ArgReference:
		probe.Value = placeholderValue(cmp.Operator, intrinsic, conceptContext, argTypeWord(args[strings.SplitN(v.Path, ".", 2)[0]]))
	case *ActorReference:
		probe.Value = placeholderValue(cmp.Operator, intrinsic, conceptContext, "string")
	case *PlanConstExpression:
		probe.Value = placeholderValue(cmp.Operator, intrinsic, conceptContext, "")
	}
	if isArrayElementField(probe.Field) {
		if scope == nil {
			scope = (*sqlElementScope)(nil).child()
		}
		_, err := compileElementComparison(&probe, scope)
		return err
	}
	_, err := compileComparisonIn(concepts, &probe, conceptContext)
	return err
}

// isLiteralComparisonValue reports whether a comparison value is known at
// load: not an argument, an actor reference or a plan constant.
func isLiteralComparisonValue(v any) bool {
	switch v.(type) {
	case *ArgReference, *ActorReference, *PlanConstExpression:
		return false
	}
	return true
}

// placeholderValue is a stand-in for a value known only at the call, typed by
// what the comparison needs: a list for membership, an instant for createdAt,
// an id of the bound concept for id, and otherwise a value of the declared
// argument's type (a string when the type is not known).
func placeholderValue(op ComparisonOperator, intrinsic, conceptContext, typ string) any {
	scalar := func() any {
		switch {
		case intrinsic == "createdAt":
			return "2026-01-01T00:00:00Z"
		case intrinsic == "id":
			if conceptContext != "" {
				return conceptContext + ":placeholder"
			}
			return "v1:placeholder:concept:placeholder"
		case typ == "number":
			return float64(1)
		case typ == "bool":
			return true
		}
		return "placeholder"
	}
	switch op {
	case OpIn, OpOut:
		return []any{scalar()}
	case OpStartsWith, OpIncludes:
		return "placeholder"
	}
	return scalar()
}
