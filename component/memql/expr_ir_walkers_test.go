package memql

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/ast"
)

// expr_ir_walkers_test.go -- THE CHECKLIST for an IR node (memql#5366).
//
// The executor's IR is walked by dozens of functions that each type-switch
// over node types, and a node a walker does not know falls into its default
// arm -- which is usually "leave it alone", and usually silently wrong: an
// actor reference left unresolved under a negation fails the read, a spec
// reference left unexpanded inside an element predicate fails the post-filter,
// a negated `concept == X` taken as the concept context composes ids against
// the wrong concept. None of those is a compile error.
//
// So every walker that learned NotExpression, ArrayPredicateExpression or
// PlanConstExpression has a row here, named `function (file)`, asserting what
// it does with them. ADDING AN IR NODE? Walk this table: each row is a walker
// your node must reach (or a reasoned refusal), and the grep that found them
// is `grep -n "case \*LogicalExpression" component/memql/*.go | grep -v _test`.
// literal_value_node_test.go's allExpressionNodeImplementers is the other half
// (cloneExpressionNode must know every node).

func irNot(target ExpressionNode) *NotExpression { return &NotExpression{Target: target} }

func irAnyTags(pred ExpressionNode) *ArrayPredicateExpression {
	return &ArrayPredicateExpression{Field: irPayloadField("tags"), Method: ArrayMethodAny, Param: "t", Pred: pred}
}

func irConceptEq(concept string) *ComparisonExpression {
	return &ComparisonExpression{Field: FieldReference{Raw: "concept", Parts: []string{"concept"}}, Operator: OpEq, Value: concept}
}

func irActorField(path string) FieldReference {
	return FieldReference{Raw: "actor." + path, Parts: []string{"actor", path}}
}

func irPlanConstReading(root string) *PlanConstExpression {
	return &PlanConstExpression{Expr: &ast.MemberExpr{Object: &ast.IdentExpr{Name: root}, Field: "x"}}
}

type irWalkerCase struct {
	walker string
	check  func(t *testing.T)
}

func irWalkerCases() []irWalkerCase {
	return []irWalkerCase{
		{"cloneExpressionNode (expression_helpers.go)", func(t *testing.T) {
			not := irNot(irPayloadCmp("a", OpEq, "x"))
			cloned := cloneExpressionNode(not).(*NotExpression)
			require.NotSame(t, not, cloned)
			require.NotSame(t, not.Target, cloned.Target, "the operand is deep-copied")

			arr := irAnyTags(irElementCmp("", OpEq, "x"))
			arr.CountValue = &PlanConstExpression{Expr: &ast.IdentExpr{Name: "now"}}
			carr := cloneExpressionNode(arr).(*ArrayPredicateExpression)
			carr.Field.Parts[1] = "mutated"
			require.Equal(t, "tags", arr.Field.Parts[1], "the field's parts are copied, not shared")
			require.NotSame(t, arr.Pred, carr.Pred)
			require.NotSame(t, arr.CountValue, carr.CountValue)

			pc := &PlanConstExpression{Expr: &ast.IdentExpr{Name: "now"}}
			cpc := cloneExpressionNode(pc).(*PlanConstExpression)
			require.NotSame(t, pc, cpc)
			require.Same(t, pc.Expr, cpc.Expr, "the parsed v1 AST is immutable and shared")

			c := cloneExpressionNode(&constantBoolExpression{value: true, planConstant: true}).(*constantBoolExpression)
			require.True(t, c.planConstant, "the plan-constant mark survives a clone")
		}},
		{"ensureBooleanExpression (expression_helpers.go)", func(t *testing.T) {
			require.NoError(t, ensureBooleanExpression(irNot(irPayloadCmp("a", OpEq, "x"))))
			require.Error(t, ensureBooleanExpression(irNot(&LiteralValueNode{Value: "x"})), "a negation's operand must be boolean")
			require.NoError(t, ensureBooleanExpression(irAnyTags(irElementCmp("", OpEq, "x"))))
			require.NoError(t, ensureBooleanExpression(&ArrayPredicateExpression{Field: irPayloadField("tags"), Method: ArrayMethodCount, CountOp: OpEq, CountValue: int64(1)}))
			require.NoError(t, ensureBooleanExpression(&PlanConstExpression{Expr: &ast.IdentExpr{Name: "now"}}))
		}},
		{"canonicalExpression / canonicalValue (canonical.go)", func(t *testing.T) {
			cmp := irPayloadCmp("a", OpEq, "x")
			require.NotEqual(t, canonicalExpression(cmp), canonicalExpression(irNot(cmp)))
			require.NotEmpty(t, canonicalExpression(irAnyTags(irElementCmp("", OpEq, "x"))))
			require.NotEmpty(t, canonicalExpression(&PlanConstExpression{Expr: &ast.IdentExpr{Name: "now"}}))
			require.Equal(t, "planConst(now)", canonicalValue(&PlanConstExpression{Expr: &ast.IdentExpr{Name: "now"}}))
		}},
		{"nodeMatchesExpression / nodeMatchesExpressionIn (executor_filter.go)", func(t *testing.T) {
			row := irRow(t, `{"a":"x","tags":["x"]}`)
			cache := map[string]map[string]any{}
			got, err := nodeMatchesExpression(row, irNot(irPayloadCmp("a", OpEq, "x")), cache)
			require.NoError(t, err)
			require.False(t, got)
			got, err = nodeMatchesExpression(row, irAnyTags(irElementCmp("", OpEq, "x")), cache)
			require.NoError(t, err)
			require.True(t, got)
			_, err = nodeMatchesExpression(row, &PlanConstExpression{Expr: &ast.IdentExpr{Name: "now"}}, cache)
			require.Error(t, err)
		}},
		{"tryCompileCombinedFilter / tryCompileCombinedFilterIn (executor_filter.go)", func(t *testing.T) {
			eng := &MemQLEngine{}
			_, ok := eng.tryCompileCombinedFilter(context.Background(), irNot(irPayloadCmp("a", OpEq, "x")), "")
			require.True(t, ok)
			_, ok = eng.tryCompileCombinedFilter(context.Background(), irAnyTags(irElementCmp("", OpEq, "x")), "")
			require.True(t, ok)
			_, ok = eng.tryCompileCombinedFilter(context.Background(), &PlanConstExpression{Expr: &ast.IdentExpr{Name: "now"}}, "")
			require.False(t, ok)
		}},
		{"resolveActorComparisonsToConstants (executor_filter.go)", func(t *testing.T) {
			ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "u-1", Role: auth.RoleAdmin})
			admin := &ComparisonExpression{Field: irActorField("role"), Operator: OpEq, Value: "admin"}
			out, err := resolveActorComparisonsToConstants(ctx, irNot(admin))
			require.NoError(t, err)
			require.Equal(t, "!(const(true))", canonicalExpression(out))
			out, err = resolveActorComparisonsToConstants(ctx, irAnyTags(&LogicalExpression{Op: LogicalAnd, Left: irElementCmp("", OpEq, "x"), Right: admin}))
			require.NoError(t, err)
			require.Equal(t, `payload.tags.any(AND($elem=="x",const(true)))`, canonicalExpression(out))
		}},
		{"canonicalizeRelationshipComparisons / canonicalizeElementComparisons (executor_filter.go)", func(t *testing.T) {
			eng := newTestEngineWithConcepts(t, map[string]*memoryNodes.Concept{
				"v1:identity:user": {Name: "v1:identity:user"},
				"v1:crm:team": {
					Name: "v1:crm:team",
					Relationships: []memoryNodes.RelationshipDefinition{
						{Type: "references", Field: "leadId", TargetConcept: "v1:identity:user", Direction: "outgoing"},
						{Type: "references", Field: "memberIds", TargetConcept: "v1:identity:user", Direction: "outgoing"},
					},
				},
			})
			ctx := context.Background()
			out := eng.canonicalizeRelationshipComparisons(ctx, irNot(irPayloadCmp("leadId", OpEq, "u-1")), "v1:crm:team")
			require.Equal(t, `!(payload.leadid=="v1:identity:user:u-1")`, canonicalExpression(out))

			members := &ArrayPredicateExpression{Field: irPayloadField("memberIds"), Method: ArrayMethodAny, Pred: irElementCmp("", OpEq, "u-1")}
			out = eng.canonicalizeRelationshipComparisons(ctx, members, "v1:crm:team")
			require.Equal(t, `payload.memberids.any($elem=="v1:identity:user:u-1")`, canonicalExpression(out),
				"an element of a relationship array compares canonical, as `v in row.memberIds` does")
			require.Equal(t, "u-1", members.Pred.(*ComparisonExpression).Value, "the input tree is never mutated")

			prefix := &ArrayPredicateExpression{Field: irPayloadField("memberIds"), Method: ArrayMethodAny, Pred: irElementCmp("", OpStartsWith, "u-")}
			out = eng.canonicalizeRelationshipComparisons(ctx, prefix, "v1:crm:team")
			require.Same(t, prefix, out, "a prefix is not an id and is not canonicalized")
		}},
		{"resolveCanonicalIdComparisons (executor_filter.go)", func(t *testing.T) {
			eng := newTestEngineWithConcepts(t, map[string]*memoryNodes.Concept{"v1:identity:user": {Name: "v1:identity:user"}})
			cid := &ast.CanonicalIdExpr{Value: &ast.LiteralExpr{Value: "u-1"}, Concept: "v1:identity:user"}
			out := eng.resolveCanonicalIdComparisons(context.Background(), irNot(irPayloadCmp("ownerId", OpEq, cid)))
			require.Equal(t, `!(payload.ownerid=="v1:identity:user:u-1")`, canonicalExpression(out))
			out = eng.resolveCanonicalIdComparisons(context.Background(), irAnyTags(irElementCmp("", OpEq, cid)))
			require.Equal(t, `payload.tags.any($elem=="v1:identity:user:u-1")`, canonicalExpression(out))
		}},
		{"extractConceptFromExpression (executor_filter.go)", func(t *testing.T) {
			require.Equal(t, "", extractConceptFromExpression(irNot(irConceptEq("v1:x:a"))),
				"a negated concept is the one the rows are NOT of")
			both := &LogicalExpression{Op: LogicalAnd, Left: irNot(irConceptEq("v1:x:a")), Right: irConceptEq("v1:x:b")}
			require.Equal(t, "v1:x:b", extractConceptFromExpression(both))
		}},
		{"evaluateExpressionSetWithContext (executor.go)", func(t *testing.T) {
			eng := &MemQLEngine{}
			_, err := eng.evaluateExpressionSetWithContext(context.Background(), irNot(&RelationshipExpression{Function: RelParentOf, Target: irPayloadCmp("a", OpEq, "x")}), nil, 0, nil, "")
			require.ErrorContains(t, err, "does not lower")
			_, err = eng.evaluateExpressionSetWithContext(context.Background(), &PlanConstExpression{Expr: &ast.IdentExpr{Name: "now"}}, nil, 0, nil, "")
			require.ErrorContains(t, err, "unevaluated")
			_, err = eng.evaluateExpressionSetWithContext(context.Background(), &ArrayPredicateExpression{Field: irActorField("groups"), Method: ArrayMethodAny, Pred: irElementCmp("", OpEq, "x")}, nil, 0, nil, "")
			require.ErrorContains(t, err, "payload array field")
		}},
		{"resolveActorReferences (executor.go)", func(t *testing.T) {
			ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "u-1", Role: auth.RoleWriter})
			out, err := resolveActorReferences(ctx, irNot(irPayloadCmp("owner", OpEq, &ActorReference{Path: "userId"})))
			require.NoError(t, err)
			require.Equal(t, `!(payload.owner=="u-1")`, canonicalExpression(out))
			out, err = resolveActorReferences(ctx, irAnyTags(irElementCmp("", OpEq, &ActorReference{Path: "userId"})))
			require.NoError(t, err)
			require.Equal(t, `payload.tags.any($elem=="u-1")`, canonicalExpression(out))
		}},
		{"resolveExpressionForExecution (executor.go)", func(t *testing.T) {
			id := &ComparisonExpression{Field: FieldReference{Raw: "id", Parts: []string{"id"}}, Operator: OpEq, Value: "abc"}
			out, err := resolveExpressionForExecution(irNot(id), "v1:x:y")
			require.NoError(t, err)
			require.Equal(t, `!(id=="v1:x:y:abc")`, canonicalExpression(out))
			out, err = resolveExpressionForExecution(irAnyTags(&LogicalExpression{Op: LogicalAnd, Left: irElementCmp("", OpEq, "t"), Right: id}), "v1:x:y")
			require.NoError(t, err)
			require.Contains(t, canonicalExpression(out), `id=="v1:x:y:abc"`)
		}},
		{"expandSpecReferences (executor.go)", func(t *testing.T) {
			eng := &MemQLEngine{specs: newSpecRegistry()}
			require.NoError(t, eng.specs.add(&Spec{Name: "isOpen", Origin: "walkers/specs.memql", Expr: irPayloadCmp("status", OpEq, "open")}))
			out, err := eng.expandSpecReferences(irNot(&SpecReferenceExpression{Name: "isOpen"}))
			require.NoError(t, err)
			require.Equal(t, `!(payload.status=="open")`, canonicalExpression(out))
			out, err = eng.expandSpecReferences(irAnyTags(&LogicalExpression{Op: LogicalAnd, Left: irElementCmp("", OpEq, "t"), Right: &SpecReferenceExpression{Name: "isOpen"}}))
			require.NoError(t, err)
			require.Contains(t, canonicalExpression(out), `payload.status=="open"`)
		}},
		{"expandExpressionWithArgs (function_validator.go)", func(t *testing.T) {
			v := newFunctionValidatorWithOrigin(nil, nil, auth.OriginInternal)
			out, err := v.expandExpressionWithArgs(irNot(irPayloadCmp("owner", OpEq, &ArgReference{Path: "who"})), map[string]any{"who": "u-2"})
			require.NoError(t, err)
			require.Equal(t, `!(payload.owner=="u-2")`, canonicalExpression(out))
			out, err = v.expandExpressionWithArgs(irAnyTags(irElementCmp("", OpEq, &ArgReference{Path: "tag"})), map[string]any{"tag": "t1"})
			require.NoError(t, err)
			require.Equal(t, `payload.tags.any($elem=="t1")`, canonicalExpression(out))
			// Plan constants: expr_plan_const_test.go carries the full rule set.
		}},
		{"hasFunctionCalls (function_validator.go)", func(t *testing.T) {
			require.True(t, hasFunctionCalls(irNot(&FunctionCallExpression{Name: "q"})))
			require.True(t, hasFunctionCalls(irAnyTags(&FunctionCallExpression{Name: "q"})))
			require.False(t, hasFunctionCalls(&PlanConstExpression{Expr: &ast.CallExpr{Name: "lower"}}))
		}},
		{"referenceRewriter.rewriteExpression (construct_reference_resolver.go)", func(t *testing.T) {
			key := QualifyConstruct("walkers", "isOpen")
			r := &referenceRewriter{scope: ConstructScope{Namespace: "walkers"}, spec: func(k string) bool { return k == key }}
			not := irNot(&SpecReferenceExpression{Name: "isOpen"})
			r.rewriteExpression(not)
			require.Equal(t, key, not.Target.(*SpecReferenceExpression).Name)
			arr := irAnyTags(&SpecReferenceExpression{Name: "isOpen"})
			r.rewriteExpression(arr)
			require.Equal(t, key, arr.Pred.(*SpecReferenceExpression).Name)
			r.rewriteExpression(&PlanConstExpression{Expr: &ast.IdentExpr{Name: "isOpen"}}) // not rewritten, and must not panic
		}},
		{"stampConceptCacheHint / collectCacheHints (function_loader.go / parser.go)", func(t *testing.T) {
			concept := irConceptEq("v1:x:a")
			require.False(t, stampConceptCacheHint(irNot(concept), 30), "a negated concept takes no TTL")
			require.Nil(t, concept.CacheHintSeconds)
			seconds := 30
			concept.CacheHintSeconds = &seconds
			hints := map[string]int64{}
			collectCacheHints(irNot(concept), hints)
			require.Empty(t, hints)
		}},
		{"containsBoundConceptEquality (function_loader.go)", func(t *testing.T) {
			require.False(t, containsBoundConceptEquality(irNot(irConceptEq("v1:x:a")), "v1:x:a"))
		}},
		{"resolveBareConcept (function_loader.go)", func(t *testing.T) {
			out := resolveBareConcept(irNot(&SpecReferenceExpression{Name: "concept"}), "v1:x:a")
			require.Equal(t, `!(concept=="v1:x:a")`, canonicalExpression(out))
		}},
		{"collectFunctionRefsRecursive (function_loader.go)", func(t *testing.T) {
			refs := collectFunctionReferences(&LogicalExpression{Op: LogicalAnd,
				Left:  irNot(&FunctionCallExpression{Name: "f"}),
				Right: &LogicalExpression{Op: LogicalOr, Left: irAnyTags(&FunctionCallExpression{Name: "g"}), Right: &PlanConstExpression{Expr: &ast.CallExpr{Name: "h"}}},
			})
			require.Equal(t, []string{"f", "g"}, refs, "a plan constant's calls are catalog functions, not constructs")
		}},
		{"walkForImpureLambda / findImpureCall (function_loader.go)", func(t *testing.T) {
			fns := map[string]*Function{"mutationDo": {Name: "mutationDo", FunctionKind: "mutation"}}
			require.Equal(t, "mutationDo", findImpureCall(irNot(&FunctionCallExpression{Name: "mutationDo"}), fns))
			require.Equal(t, "mutationDo", findImpureCall(irAnyTags(&FunctionCallExpression{Name: "mutationDo"}), fns))
			impure := &CollectionMethodExpression{Receiver: &ArgRefExpression{Path: "xs"}, Method: "where",
				Args: []ExpressionNode{&LambdaExpression{Params: []string{"x"}, Body: &FunctionCallExpression{Name: "mutationDo"}}}}
			require.Error(t, walkForImpureLambda(irNot(impure), fns))
			require.Error(t, walkForImpureLambda(irAnyTags(impure), fns))
		}},
		{"walkForInMemoryScans (collection_scan_lint.go)", func(t *testing.T) {
			fns := map[string]*Function{"allThings": {Name: "allThings", FunctionKind: "query", Expr: irConceptEq("v1:x:thing")}}
			scan := &CollectionMethodExpression{Receiver: &FunctionCallExpression{Name: "allThings"}, Method: "count"}
			var found []InMemoryScanFinding
			walkForInMemoryScans(irNot(scan), fns, func(f InMemoryScanFinding) { found = append(found, f) })
			walkForInMemoryScans(irAnyTags(scan), fns, func(f InMemoryScanFinding) { found = append(found, f) })
			require.Len(t, found, 2)
		}},
		{"expandSpecReferencesWithOverlay (authoring_session.go)", func(t *testing.T) {
			overlay := map[string]*Spec{"isOpen": {Name: "isOpen", Expr: irPayloadCmp("status", OpEq, "open")}}
			out, err := expandSpecReferencesWithOverlay(irNot(&SpecReferenceExpression{Name: "isOpen"}), overlay, map[string]struct{}{})
			require.NoError(t, err)
			require.Equal(t, `!(payload.status=="open")`, canonicalExpression(out))
			out, err = expandSpecReferencesWithOverlay(irAnyTags(&SpecReferenceExpression{Name: "isOpen"}), overlay, map[string]struct{}{})
			require.NoError(t, err)
			require.Equal(t, `payload.tags.any(payload.status=="open")`, canonicalExpression(out))
		}},
		{"validateAIContext (ai_validation.go)", func(t *testing.T) {
			require.Error(t, validateAIContext(irNot(&AIExpression{})))
			require.Error(t, validateAIContext(irAnyTags(&AIExpression{})))
		}},
		{"detectAIUsage (spec_helpers.go)", func(t *testing.T) {
			require.True(t, detectAIUsage(irNot(&AIExpression{})))
			require.True(t, detectAIUsage(irAnyTags(&AIExpression{})))
		}},
		{"evalCollScalar (collection_method.go)", func(t *testing.T) {
			got, err := evalCollScalar(irNot(&LiteralValueNode{Value: true}), nil, nil)
			require.NoError(t, err)
			require.Equal(t, false, got)
		}},
		{"evaluateSpecExpression / evaluateSpecValue (expression_evaluator.go)", func(t *testing.T) {
			withFakePlanConstantEvaluator(t)
			envelope := testPlanConstAmbient()
			role := &ComparisonExpression{Field: irActorField("role"), Operator: OpEq, Value: "writer"}
			got, err := evaluateSpecExpression(context.Background(), nil, irNot(role), envelope)
			require.NoError(t, err)
			require.Equal(t, false, got)
			got, err = evaluateSpecExpression(context.Background(), nil, &constantBoolExpression{value: true}, envelope)
			require.NoError(t, err)
			require.Equal(t, true, got)
			_, err = evaluateSpecExpression(context.Background(), nil, irAnyTags(irElementCmp("", OpEq, "x")), envelope)
			require.ErrorContains(t, err, "not evaluable in a context-spec body")
			got, err = evaluateSpecExpression(context.Background(), nil,
				&ComparisonExpression{Field: irActorField("role"), Operator: OpEq, Value: &PlanConstExpression{Expr: &ast.LiteralExpr{Value: "writer"}}}, envelope)
			require.NoError(t, err)
			require.Equal(t, true, got)
		}},
		{"collectConceptLiterals (result_cache_deps.go)", func(t *testing.T) {
			var got []string
			collectConceptLiterals(irNot(irConceptEq("v1:x:a")), func(c string) { got = append(got, c) })
			collectConceptLiterals(irAnyTags(irConceptEq("v1:x:b")), func(c string) { got = append(got, c) })
			require.Equal(t, []string{"v1:x:a", "v1:x:b"}, got, "over-approximating a dependency costs an eviction; missing one serves stale rows")
		}},
		{"planReferencesActor (result_cache_policy.go)", func(t *testing.T) {
			eng := &MemQLEngine{}
			require.True(t, eng.planReferencesActor(irNot(irPayloadCmp("owner", OpEq, &ActorReference{Path: "userId"}))))
			require.True(t, eng.planReferencesActor(irAnyTags(irElementCmp("", OpEq, &ActorReference{Path: "userId"}))))
			require.True(t, eng.planReferencesActor(irPlanConstReading("actor")))
			require.False(t, eng.planReferencesActor(irPlanConstReading("args")))
			require.True(t, eng.planReferencesActor(irPayloadCmp("owner", OpEq, irPlanConstReading("actor"))))
			require.False(t, eng.planReferencesActor(irPayloadCmp("owner", OpEq, irPlanConstReading("args"))))
			require.True(t, eng.planReferencesActor(&ArrayPredicateExpression{Field: irPayloadField("tags"), Method: ArrayMethodCount, CountOp: OpGt, CountValue: irPlanConstReading("actor")}))
		}},
		{"cloneRowAuthzPredicate / stampRowAuthzConcept (rowauthz_enforce.go)", func(t *testing.T) {
			not := irNot(irPayloadCmp("owner", OpEq, "u"))
			cloned := cloneRowAuthzPredicate(not).(*NotExpression)
			require.NotSame(t, not.Target, cloned.Target)
			stampRowAuthzConcept(not, "v1:x:a")
			require.Equal(t, "v1:x:a", not.Target.(*ComparisonExpression).RowAuthzConcept)
		}},
		{"flattenRuntimeFunctionArgs (parser_langpath.go)", func(t *testing.T) {
			call := &FunctionCallExpression{Name: "q", Args: map[string]any{"0": map[string]any{"a": 1}}}
			flattenRuntimeFunctionArgs(irNot(call))
			require.Equal(t, map[string]any{"a": 1}, call.Args)
		}},
		{"lowerRankScope / treeHasRankScope (rowauthz_rank.go)", func(t *testing.T) {
			rank := &RankScopeExpression{OwnerField: "ownerUserId"}
			require.True(t, treeHasRankScope(irNot(rank)), "the predicate must answer for every node the lowering handles")
			memo := &rankScopeMemo{scope: &rankScope{readOwners: map[string]struct{}{"u-1": {}}}}
			memo.once.Do(func() {})
			ctx := context.WithValue(context.Background(), rankScopeMemoKey{}, memo)
			out := (&MemQLEngine{}).lowerRankScope(ctx, irNot(rank))
			require.Equal(t, `!(payload.owneruseridin["u-1"])`, canonicalExpression(out))
		}},
		{"lowerAccountScope / treeHasAccountScope (rowauthz_account.go)", func(t *testing.T) {
			account := &AccountScopeExpression{Field: ""}
			require.True(t, treeHasAccountScope(irNot(account)))
			out := (&MemQLEngine{}).lowerAccountScope(context.Background(), irNot(account))
			require.Equal(t, "!(const(false))", canonicalExpression(out))
		}},
		{"specValidator.expandExpression / specUsage.collect (spec_validator.go)", func(t *testing.T) {
			v := newSpecValidator(nil, map[string]*Spec{"isOpen": {Name: "isOpen", Expr: irPayloadCmp("status", OpEq, "open")}}, nil)
			out, err := v.expandExpression(irNot(&SpecReferenceExpression{Name: "isOpen"}), false)
			require.NoError(t, err)
			require.Equal(t, `!(payload.status=="open")`, canonicalExpression(out))
			usage := newSpecUsage()
			usage.collect(irNot(irConceptEq("v1:x:a")))
			usage.collect(irAnyTags(irElementCmp("", OpEq, "x")))
			require.Contains(t, usage.concepts, "v1:x:a", "a negated concept still constrains the concept")
			require.Contains(t, usage.payloadPaths, "tags")
			require.NotContains(t, usage.payloadPaths, arrayElementRoot)
		}},
		{"rewriteFilterFieldRefs / filterFieldRef / ensureNoDirectiveNodes / collectConceptFields (parser.go)", func(t *testing.T) {
			bare := &ComparisonExpression{Field: FieldReference{Raw: "status", Parts: []string{"status"}}, Operator: OpEq, Value: "x"}
			require.NoError(t, rewriteFilterFieldRefs(irNot(bare)))
			require.Equal(t, []string{"payload", "status"}, bare.Field.Parts)

			active := &ComparisonExpression{Field: FieldReference{Raw: "active", Parts: []string{"active"}}, Operator: OpEq, Value: true}
			element := irElementCmp("", OpEq, "a")
			arr := &ArrayPredicateExpression{Field: FieldReference{Raw: "tags", Parts: []string{"tags"}}, Method: ArrayMethodAny,
				Pred: &LogicalExpression{Op: LogicalAnd, Left: element, Right: active}}
			require.NoError(t, rewriteFilterFieldRefs(arr))
			require.Equal(t, []string{"payload", "tags"}, arr.Field.Parts)
			require.Equal(t, []string{arrayElementRoot}, element.Field.Parts, "the element root is never payload-prefixed")
			require.Equal(t, []string{"payload", "active"}, active.Field.Parts)

			require.Error(t, ensureNoDirectiveNodes(irNot(&SortExpression{})))
			require.Error(t, ensureNoDirectiveNodes(irAnyTags(&PaginateExpression{})))

			concept := irConceptEq("v1:x:a")
			concept.FieldSelections = []FieldReference{{Raw: "payload.a", Parts: []string{"payload", "a"}}}
			fields := map[string][]FieldReference{}
			collectConceptFields(irNot(concept), fields)
			require.Empty(t, fields, "field selections ride a concept the query reads, not one it excludes")
		}},
		{"rewriteSpecFields (spec_binding_resolver.go)", func(t *testing.T) {
			mapper := conceptFieldMapper(nil)
			bare := &ComparisonExpression{Field: FieldReference{Raw: "status", Parts: []string{"status"}}, Operator: OpEq, Value: "x"}
			require.NoError(t, rewriteSpecFields(irNot(bare), mapper))
			require.Equal(t, []string{"payload", "status"}, bare.Field.Parts)
			element := irElementCmp("qty", OpGt, int64(0))
			arr := &ArrayPredicateExpression{Field: FieldReference{Raw: "items", Parts: []string{"items"}}, Method: ArrayMethodAll, Pred: element}
			require.NoError(t, rewriteSpecFields(arr, mapper))
			require.Equal(t, []string{"payload", "items"}, arr.Field.Parts)
			require.Equal(t, []string{arrayElementRoot, "qty"}, element.Field.Parts, "an element field is not a field of the binding")
		}},
		{"firstOpaqueConjunct (rowauthz_shadow.go)", func(t *testing.T) {
			require.Equal(t, "a negation", firstOpaqueConjunct([]ExpressionNode{irNot(irPayloadCmp("a", OpEq, "x"))}))
			require.True(t, strings.HasPrefix(firstOpaqueConjunct([]ExpressionNode{irAnyTags(irElementCmp("", OpEq, "x"))}), "the collection predicate "))
			require.Equal(t, "an unevaluated plan constant", firstOpaqueConjunct([]ExpressionNode{&PlanConstExpression{Expr: &ast.IdentExpr{Name: "now"}}}))
		}},
		{"evaluateExprAgainstPayload (shape_template.go)", func(t *testing.T) {
			payload := map[string]any{"status": "open", "tags": []any{"x"}}
			got, err := evaluateExprAgainstPayload(irNot(irPayloadCmp("status", OpEq, "open")), payload)
			require.NoError(t, err)
			require.False(t, got)
			got, err = evaluateExprAgainstPayload(irAnyTags(irElementCmp("", OpEq, "x")), payload)
			require.NoError(t, err)
			require.True(t, got)
			_, err = evaluateExprAgainstPayload(&PlanConstExpression{Expr: &ast.IdentExpr{Name: "now"}}, payload)
			require.Error(t, err)
		}},
		{"stagedDataExcludesConcept / StagedDataTraverses (staged_shadow.go)", func(t *testing.T) {
			require.True(t, stagedDataExcludesConcept(irNot(irConceptEq("v1:x:staged")), "v1:x:staged"))
			in := &ComparisonExpression{Field: FieldReference{Raw: "row.concept", Parts: []string{"row", "concept"}}, Operator: OpIn, Value: []any{"v1:x:staged"}}
			require.True(t, stagedDataExcludesConcept(irNot(in), "v1:x:staged"))
			ne := &ComparisonExpression{Field: FieldReference{Raw: "concept", Parts: []string{"concept"}}, Operator: OpNe, Value: "v1:x:staged"}
			require.False(t, stagedDataExcludesConcept(irNot(ne), "v1:x:staged"), "a negated exclusion INCLUDES the concept")
			require.True(t, StagedDataTraverses(irNot(&RelationshipExpression{Function: RelChildOf})))
		}},
		{"cloneStagedPredicate (staged_enforce.go)", func(t *testing.T) {
			not := irNot(irConceptEq("v1:x:staged"))
			cloned := cloneStagedPredicate(not).(*NotExpression)
			require.NotSame(t, not.Target, cloned.Target)
		}},
		{"findTemporalAccess (temporal_access.go)", func(t *testing.T) {
			ts := &TimestampExpression{UseLatest: true}
			require.Same(t, ts, findTemporalAccess(irNot(ts)))
			require.Same(t, ts, findTemporalAccess(irAnyTags(ts)))
		}},
		{"walkSpecRefs / normalizeSpecCallsToReferences (ast_converter.go)", func(t *testing.T) {
			var seen []string
			walkSpecRefs(&LogicalExpression{Op: LogicalAnd, Left: irNot(irPayloadCmp("a", OpEq, "x")), Right: irAnyTags(irElementCmp("", OpEq, "x"))},
				func(ref FieldReference) { seen = append(seen, strings.Join(ref.Parts, ".")) })
			require.Equal(t, []string{"payload.a", "payload.tags", arrayElementRoot}, seen)
			out, err := normalizeSpecCallsToReferences(irNot(&FunctionCallExpression{Name: "isOpen"}))
			require.NoError(t, err)
			require.IsType(t, &SpecReferenceExpression{}, out.(*NotExpression).Target)
		}},
		{"treeReachesRelationship (expr_not.go)", func(t *testing.T) {
			eng := &MemQLEngine{}
			require.True(t, eng.treeReachesRelationship(irNot(&LogicalExpression{Op: LogicalOr, Left: irPayloadCmp("a", OpEq, "x"), Right: &RelationshipExpression{Function: RelOwns}}), map[string]struct{}{}))
		}},
		{"treeHasPlanConstant / specBodyNeedsExpansion (expr_plan_const.go)", func(t *testing.T) {
			pc := &PlanConstExpression{Expr: &ast.IdentExpr{Name: "now"}}
			require.True(t, treeHasPlanConstant(irNot(pc)))
			require.True(t, treeHasPlanConstant(irAnyTags(irElementCmp("qty", OpGt, pc))))
			require.True(t, treeHasPlanConstant(&ArrayPredicateExpression{Field: irPayloadField("tags"), Method: ArrayMethodCount, CountOp: OpGt, CountValue: pc}))
			require.False(t, treeHasPlanConstant(irNot(irPayloadCmp("a", OpEq, "x"))))
			specs := newSpecRegistry()
			require.NoError(t, specs.add(&Spec{Name: "isFresh", Origin: "walkers/specs.memql", Expr: irNot(irPayloadCmp("expiresAt", OpLt, pc))}))
			v := newFunctionValidatorWithOrigin(nil, specs, auth.OriginInternal)
			require.True(t, v.specBodyNeedsExpansion("isFresh", map[string]struct{}{}))
		}},
	}
}

func TestIRWalkersLearnTheEditionTwentySixNodes(t *testing.T) {
	seen := map[string]bool{}
	for _, tc := range irWalkerCases() {
		require.False(t, seen[tc.walker], "walker %q is listed twice", tc.walker)
		seen[tc.walker] = true
		t.Run(tc.walker, tc.check)
	}
}
