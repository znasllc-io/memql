package memql

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/ast"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/metrics"
)

// refine_test.go -- the refine clause (memql#5366), DB-free: the directive is
// peeled like every other, every walker that knows a directive knows it, the
// bindings are captured at argument expansion, the page is filtered by
// EvalExpr, and the load-time checks refuse what the in-process evaluator
// could not run. refine_db_test.go runs it over a real page.

func refineLambda(t *testing.T, src string) *languageParser.LambdaExpr {
	t.Helper()
	lam, err := languageParser.ParseV1Lambda(src)
	require.NoError(t, err)
	return lam
}

func refineTree(t *testing.T, lambda string) *ShapeExpression {
	limit := 10
	return &ShapeExpression{
		TemplateName: "s",
		Target: &RefineExpression{
			Lambda: refineLambda(t, lambda),
			Target: &PaginateExpression{Limit: &limit, Target: irConceptEq("v1:x:ticket")},
		},
	}
}

func TestRefine_IsADirectiveEveryWalkerPeels(t *testing.T) {
	tree := refineTree(t, `row => row.title.includes("x")`)

	plan := &QueryPlan{Root: tree}
	root, err := applyDirectiveWrappers(plan)
	require.NoError(t, err)
	require.NotNil(t, plan.Refine, "applyDirectiveWrappers peels refine onto the plan")
	require.Equal(t, `row => row.title.includes("x")`, ast.FormatExpr(plan.Refine.Lambda))
	require.NotNil(t, plan.Limit)
	require.Equal(t, 10, *plan.Limit)
	require.Equal(t, `concept=="v1:x:ticket"`, canonicalExpression(root))

	require.Error(t, ensureNoDirectiveNodes(&LogicalExpression{Op: LogicalAnd, Left: irConceptEq("v1:x:a"), Right: &RefineExpression{Target: irConceptEq("v1:x:a")}}),
		"a refine below the top of the tree is refused like any other misplaced directive")

	clone, ok := cloneExpressionNode(tree).(*ShapeExpression)
	require.True(t, ok)
	cloned, ok := clone.Target.(*RefineExpression)
	require.True(t, ok, "clone keeps the refine node")
	require.Same(t, tree.Target.(*RefineExpression).Lambda, cloned.Lambda, "the parsed lambda is shared, never copied")
	require.NotSame(t, tree.Target.(*RefineExpression).Target, cloned.Target, "the target is cloned")

	require.Equal(t, "v1:x:ticket", extractConceptFromExpression(tree))
	require.Same(t, tree.Target, refineIn(tree))
	require.Equal(t, `concept=="v1:x:ticket"`, canonicalExpression(unwrapToFilter(tree)))
	require.Equal(t, `concept=="v1:x:ticket"`, canonicalExpression(peelDirectiveWrappers(tree)))
	require.False(t, treeHasPlanConstant(tree))

	withPlanConst := &RefineExpression{Lambda: refineLambda(t, `row => true`), Target: &ComparisonExpression{Field: irPayloadField("a"), Operator: OpEq, Value: &PlanConstExpression{Expr: refineLambda(t, `x => now`).Body}}}
	require.True(t, treeHasPlanConstant(withPlanConst), "a plan constant in the refine's TARGET is found through it")

	stamped := refineTree(t, `row => true`)
	require.True(t, stampConceptCacheHint(stamped, 30), "a cache hint reaches the concept binding inside the refine")
	bound := ensureBoundConceptFilter(refineTree(t, `row => true`), "v1:x:ticket")
	require.NotNil(t, refineIn(bound))
	bare := resolveBareConcept(refineTree(t, `row => true`), "v1:x:ticket")
	require.NotNil(t, refineIn(bare), "resolveBareConcept rebuilds the refine node rather than dropping it")
}

func TestRefine_ExpansionCapturesTheCallsBindings(t *testing.T) {
	v := planConstValidator(nil)
	expanded, err := v.expandExpressionWithArgs(refineTree(t, `row => row.title == args.q`), map[string]any{"q": "hello"})
	require.NoError(t, err)
	refine := refineIn(expanded)
	require.NotNil(t, refine)
	require.Equal(t, map[string]any{"q": "hello"}, refine.Bindings["args"])
	for _, root := range planConstantAmbientRoots {
		require.Contains(t, refine.Bindings, root, "the ambient %s is captured beside the arguments", root)
	}
}

func refineNode(t *testing.T, id, payload string) memorynodes.MemoryNode {
	t.Helper()
	require.True(t, json.Valid([]byte(payload)))
	return memorynodes.MemoryNode{ID: "v1:x:ticket:" + id, Concept: "v1:x:ticket", CreatedAt: time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC), Payload: json.RawMessage(payload)}
}

func TestRefine_FiltersThePageInProcessAndCountsTheDrop(t *testing.T) {
	e := &MemQLEngine{specs: newSpecRegistry()}
	page := []memorynodes.MemoryNode{
		refineNode(t, "a", `{"title":"Half Off Today","priority":3}`),
		refineNode(t, "b", `{"title":"nothing here","priority":1}`),
		refineNode(t, "c", `{"title":"OFFER","priority":5}`),
		refineNode(t, "d", `{"priority":9}`),
	}
	refine := &RefineExpression{
		Lambda:   refineLambda(t, `row => lower(row.title).includes(args.word) && row.priority >= args.floor`),
		Bindings: map[string]any{"args": map[string]any{"word": "off", "floor": int64(3)}},
	}
	query := "refineUnit" + uniqueSuffix("q")
	kept, err := e.applyRefine(context.Background(), refine, query, page)
	require.NoError(t, err)
	ids := make([]string, len(kept))
	for i, n := range kept {
		ids[i] = n.ID
	}
	require.Equal(t, []string{"v1:x:ticket:a", "v1:x:ticket:c"}, ids, "kept in page order")
	require.Equal(t, float64(2), metrics.QueryRefineRowsValue(query, "kept"))
	require.Equal(t, float64(2), metrics.QueryRefineRowsValue(query, "dropped"))

	// A stored value that is not a boolean is data, and data answers: it is
	// not true, so the row is dropped and its negation keeps it -- as the
	// pushdown reads the same field (expr_stored.go).
	storedNotBoolean := &RefineExpression{Lambda: refineLambda(t, `row => row.title`), Bindings: map[string]any{}}
	kept, err = e.applyRefine(context.Background(), storedNotBoolean, query, page[:1])
	require.NoError(t, err, "a stored non-boolean is not true; it does not fail the read")
	require.Empty(t, kept)
	negated := &RefineExpression{Lambda: refineLambda(t, `row => !row.title`), Bindings: map[string]any{}}
	kept, err = e.applyRefine(context.Background(), negated, query, page[:1])
	require.NoError(t, err)
	require.Len(t, kept, 1)

	// A value the predicate COMPUTED that is not a boolean is the author's
	// mistake: the row cannot be decided, and the read fails naming it.
	computed := &RefineExpression{Lambda: refineLambda(t, `row => lower(row.title)`), Bindings: map[string]any{}}
	_, err = e.applyRefine(context.Background(), computed, query, page[:1])
	require.ErrorContains(t, err, "condition_not_boolean", "a row the predicate cannot decide fails the read, naming the row")
	require.ErrorContains(t, err, "v1:x:ticket:a")
}

func TestRefine_ValidateRefusesWhatEvalExprCouldNotRun(t *testing.T) {
	e := &MemQLEngine{specs: newSpecRegistry()}
	// A spec with no edition-2026 body: EvalExpr evaluates a predicate from
	// its v1 lambda, and this one has only the IR a pre-2026 body converted
	// to. No DSL can register one since the flip, so it is placed here
	// directly -- the refusal is the in-process evaluator's to make, whatever
	// registered the spec.
	require.NoError(t, e.specs.add(&Spec{Name: "legacyOpen", Kind: SpecKindRow, Origin: "test", Expr: irConceptEq("v1:x:ticket")}))
	fn := &Function{Name: "q", ArgsSchema: &ArgsSchemaConfig{Fields: []*FunctionArgsField{{Name: "word", Type: "string"}}}}
	for _, tc := range []struct{ src, want string }{
		{`row => row.title.includes(args.word)`, ""},
		{`row => row.tags.where(t => t == args.word).count() > 0 && now > "2026"`, ""},
		{`row => row.title == args.missing`, "`args.missing` is not a declared argument"},
		{`row => event.x == 1`, "`event` is not defined here"},
		{`row => query other().count() > 0`, "is not admitted in a refine clause"},
		{`row => childOf(p => p.id == "x")`, "a traversal selects rows in SQL"},
		{`row => isNothing(row)`, "`isNothing` is not a spec, trait or catalog function known here"},
		{`row => legacyOpen(row)`, "`legacyOpen` has a pre-2026 body, which the in-process evaluator cannot evaluate"},
		{`row => row.tags.any(a => row.tags.any(b => row.tags.any(c => a == c)))`, "above tiers.MaxStaticCost"},
	} {
		err := e.validateRefine(fn, &RefineExpression{Lambda: refineLambda(t, tc.src)})
		if tc.want == "" {
			require.NoError(t, err, tc.src)
			continue
		}
		require.Error(t, err, tc.src)
		require.Contains(t, err.Error(), "does not lower in a refine clause", tc.src)
		require.Contains(t, err.Error(), tc.want, tc.src)
	}
}
