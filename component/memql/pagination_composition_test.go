package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
)

func composedPageFunction(name, owner string, cap int) *Function {
	root := ExpressionNode(irConceptEq(keysetConcept))
	if owner != "" {
		root = &LogicalExpression{Op: LogicalAnd, Left: root, Right: &ComparisonExpression{
			Field: FieldReference{Raw: "createdBy", Parts: []string{"createdBy"}}, Operator: OpEq, Value: owner,
		}}
	}
	return &Function{Name: name, FunctionKind: "query", Enabled: true, RequiresRank: "writer", Expr: &SortExpression{
		Fields: []SortField{{Field: "createdAt", Direction: SortDirectionDesc}},
		Target: &PaginateExpression{Target: root, Limit: &cap},
	}}
}

func TestNamedQueryPaginationNarrowsWithoutMutatingDeclaredCap(t *testing.T) {
	registry := newFunctionRegistry()
	fn := composedPageFunction("composedPage", "", 50)
	require.NoError(t, registry.add(fn))
	e := &MemQLEngine{initialized: true, functions: registry}
	original := canonicalExpression(fn.Expr)
	for _, tc := range []struct {
		call string
		want int
	}{
		{`paginate(query composedPage(), 10)`, 10},
		{`paginate(query composedPage(), 100)`, 50},
		{`query composedPage()`, 50},
		{`paginate(query composedPage(), 1)`, 1},
		{`paginate(query composedPage(), 50)`, 50},
	} {
		plan, err := e.Parse(tc.call)
		require.NoError(t, err, tc.call)
		require.NotNil(t, plan.Limit)
		require.Equal(t, tc.want, *plan.Limit, tc.call)
		require.Equal(t, []SortField{{Field: "createdAt", Direction: SortDirectionDesc}}, plan.Sort)
		require.Equal(t, map[string]string{"composedPage": "writer"}, plan.RequiredRanks)
		require.Equal(t, original, canonicalExpression(fn.Expr), "another caller still gets the declared cap")
	}
}

func TestNamedQueryPaginationKeepsDuplicateAndInvalidCapRejections(t *testing.T) {
	registry := newFunctionRegistry()
	require.NoError(t, registry.add(composedPageFunction("composedPage", "", 50)))
	bad := composedPageFunction("badPage", "", 50)
	limit := 30
	bad.Expr = &PaginateExpression{Target: bad.Expr, Limit: &limit}
	require.NoError(t, registry.add(bad))
	e := &MemQLEngine{initialized: true, functions: registry}
	for _, query := range []string{
		`paginate(paginate(concept==v1:test:item, 50), 10)`,
		`paginate(paginate(query composedPage(), 30), 10)`,
		`query badPage()`, `paginate(query badPage(), 10)`,
	} {
		_, err := e.Parse(query)
		require.ErrorContains(t, err, "multiple paginate()", query)
	}
	for _, cap := range []string{"0", "-1", "1.5", `"10"`, "true"} {
		_, err := e.Parse("paginate(query composedPage(), " + cap + ")")
		require.Error(t, err, "invalid outer cap %s must not be hidden by the inner cap", cap)
	}
}

func TestNamedQueryPaginationComposesNestedDeclarationsAndKeepsBothRanks(t *testing.T) {
	registry := newFunctionRegistry()
	inner := composedPageFunction("innerPage", "", 50)
	require.NoError(t, registry.add(inner))
	cap := 20
	outer := &Function{Name: "outerPage", FunctionKind: "query", Enabled: true, RequiresRank: "developer",
		Expr: &PaginateExpression{Target: &FunctionCallExpression{Name: "innerPage"}, Limit: &cap}}
	require.NoError(t, registry.add(outer))
	innerOriginal, outerOriginal := canonicalExpression(inner.Expr), canonicalExpression(outer.Expr)
	e := &MemQLEngine{initialized: true, functions: registry}
	for _, tc := range []struct {
		query string
		cap   int
	}{
		{`query outerPage()`, 20},
		{`paginate(query outerPage(), 10)`, 10},
		{`paginate(query outerPage(), 100)`, 20},
	} {
		plan, err := e.Parse(tc.query)
		require.NoError(t, err, tc.query)
		require.Equal(t, tc.cap, *plan.Limit)
		require.Equal(t, map[string]string{"innerPage": "writer", "outerPage": "developer"}, plan.RequiredRanks)
		require.Equal(t, innerOriginal, canonicalExpression(inner.Expr))
		require.Equal(t, outerOriginal, canonicalExpression(outer.Expr))
	}
}

func TestNamedQueryPaginationBoundsSQLCursorCacheAndRankDB(t *testing.T) {
	// This asserts an actual cache hit, so it owns its cache. The package's
	// shared engine may hold hot entries from earlier tests; Ristretto may
	// legitimately reject a new entry under that admission pressure even when
	// SetWithTTL queued it. Wait below drains the asynchronous admission queue
	// before asserting reuse, without adding a timing-dependent sleep.
	t.Setenv(envCacheSize, "1024")
	engine, db, _ := readMergeTestEngine(t)
	require.NotNil(t, engine.cache)
	initialAdds := engine.cache.Stats().KeysAdded
	owner := uniqueSuffix("composed-page")
	ctx := clusterOwnerCtx(owner)
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	var want []string
	for n := 0; n < 25; n++ {
		id := fmt.Sprintf("%s:%s-%02d", keysetConcept, owner, n)
		seedKeysetRow(t, ctx, db, id, base.Add(time.Duration(n)*time.Second), owner)
		want = append([]string{id}, want...)
	}
	registry := newFunctionRegistry()
	fn := composedPageFunction("composedPage", owner, 500)
	fn.BoundConcept = keysetConcept
	require.NoError(t, registry.add(fn))
	hook := &osListEvidenceCapture{}
	db.AddQueryHook(hook)
	ctx = context.WithValue(ctx, osListEvidenceContextKey{}, true)
	call := `paginate(query composedPage(), 10)`
	var got []string
	cursor := ""
	for _, size := range []int{10, 10, 5} {
		page, err := engine.executeWith(ContextWithCursor(ctx, cursor), call, registry, nil, false)
		require.NoError(t, err)
		engine.cache.cache.Wait()
		ids := pageIDs(t, page)
		require.Len(t, ids, size)
		got = append(got, ids...)
		cursor = page.GetMeta().Cursor
		if size == 10 {
			require.NotEmpty(t, cursor)
		}
	}
	require.Equal(t, want, got)
	require.Equal(t, initialAdds+3, engine.cache.Stats().KeysAdded, "positive control: all three distinct pages were admitted to this private cache")
	hook.mu.Lock()
	queries := append([]string(nil), hook.queries...)
	hook.mu.Unlock()
	primary := 0
	for _, query := range queries {
		// The separate latest-version recheck reads only this page's IDs;
		// the authored createdBy predicate identifies the primary list scan.
		if !strings.HasPrefix(query, "SELECT") || !strings.Contains(query, `("createdBy" = '`) {
			continue
		}
		primary++
		require.True(t, strings.HasSuffix(query, "LIMIT 10"), "SQL must page before materialization: %s", query)
		var raw []byte
		require.NoError(t, db.QueryRowContext(context.Background(), "EXPLAIN (ANALYZE, FORMAT JSON) "+query).Scan(&raw))
		var plans []map[string]any
		require.NoError(t, json.Unmarshal(raw, &plans))
		require.LessOrEqual(t, plans[0]["Plan"].(map[string]any)["Actual Rows"].(float64), float64(10))
	}
	require.Equal(t, 3, primary, "each continuation must have its own SQL page")

	// A cached first page stays isolated from the continuation and other caps.
	hook.mu.Lock()
	hook.queries = nil
	hook.mu.Unlock()
	repeated, err := engine.executeWith(ctx, call, registry, nil, false)
	require.NoError(t, err)
	require.Equal(t, want[:10], pageIDs(t, repeated))
	hook.mu.Lock()
	repeatedQueries := append([]string(nil), hook.queries...)
	hook.mu.Unlock()
	for _, query := range repeatedQueries {
		require.NotContains(t, query, `("createdBy" = '`, "the repeated first page should come from its cache entry")
	}
	for _, tc := range []struct {
		call string
		size int
	}{
		{`paginate(query composedPage(), 5)`, 5},
		{`paginate(query composedPage(), 20)`, 20},
		{`query composedPage()`, 25},
	} {
		page, err := engine.executeWith(ctx, tc.call, registry, nil, false)
		require.NoError(t, err)
		require.Equal(t, want[:tc.size], pageIDs(t, page), tc.call)
	}
	_, err = engine.executeWith(shareDirActor(context.Background(), owner, auth.RoleReader), call, registry, nil, false)
	require.Error(t, err, "a cached privileged page must not bypass the named query's writer floor")
}
