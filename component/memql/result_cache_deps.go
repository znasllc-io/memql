package memql

import (
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// dependencyConceptsForResult resolves the set of concept ids a cached
// query result depends on, so a graph write to any of them can evict
// the cached key (5.4 invalidation primitive). It draws from two
// sources and unions them:
//
//  1. The plan's filter expression -- every `concept == "v1:..."` /
//     `concept in [...]` predicate names a concept the query reads.
//     This covers the empty-result case: a query that currently
//     matches zero rows still depends on its concept, so a freshly
//     inserted row of that concept must evict the (empty) cached
//     result.
//  2. The returned bundle's node concepts -- captures concepts reached
//     via relationship traversal / depth expansion that the top-level
//     filter does not name.
//
// The result is deduped. An empty result means the engine could not
// name a dependency concept; the caller declines to cache rather than
// cache an un-invalidatable result.
func dependencyConceptsForResult(plan *QueryPlan, bundle *memqlv1.GraphBundle) []string {
	seen := make(map[string]struct{})
	var out []string
	add := func(concept string) {
		if concept == "" {
			return
		}
		if _, ok := seen[concept]; ok {
			return
		}
		seen[concept] = struct{}{}
		out = append(out, concept)
	}

	if plan != nil {
		collectConceptLiterals(plan.Root, add)
	}

	if bundle != nil {
		for _, n := range bundle.GetNodes() {
			if n == nil {
				continue
			}
			add(n.GetConcept())
		}
	}

	return out
}

// collectConceptLiterals walks an expression tree and reports every
// concept-intrinsic comparison's literal value(s) via add. It handles
// the logical (AND/OR), relationship/sort/select/count wrapper, and
// comparison node shapes; it deliberately ignores arg/actor-reference
// values (a non-literal concept can't be pinned to a single concept id
// at plan time, so it contributes no static dependency).
func collectConceptLiterals(expr ExpressionNode, add func(string)) {
	switch node := expr.(type) {
	case nil:
		return
	case *LogicalExpression:
		collectConceptLiterals(node.Left, add)
		collectConceptLiterals(node.Right, add)
	case *RelationshipExpression:
		collectConceptLiterals(node.Target, add)
	case *SortExpression:
		collectConceptLiterals(node.Target, add)
	case *SelectExpression:
		collectConceptLiterals(node.Target, add)
	case *CountExpression:
		collectConceptLiterals(node.Target, add)
	case *ComparisonExpression:
		collectConceptFromComparison(node, add)
	}
}

// collectConceptFromComparison reports the concept literal(s) named by
// a single comparison if (and only if) its field is the `concept`
// intrinsic and its value is a static string (or list of strings for
// `in`). Non-concept fields and non-literal values are skipped.
func collectConceptFromComparison(expr *ComparisonExpression, add func(string)) {
	if expr == nil || len(expr.Field.Parts) != 1 {
		return
	}
	info, ok := resolveIntrinsicField(expr.Field.Parts[0])
	if !ok || info.kind != intrinsicFieldConcept {
		return
	}
	switch v := expr.Value.(type) {
	case string:
		add(v)
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok {
				add(s)
			}
		}
	case []string:
		for _, s := range v {
			add(s)
		}
	}
}
