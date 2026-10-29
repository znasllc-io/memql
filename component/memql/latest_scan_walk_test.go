package memql

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
)

// latestScanWalkSQL renders the walk latestScanWalk builds for expr, or "" for
// a read it does not take. Building a bun query opens no connection.
func latestScanWalkSQL(t *testing.T, ctx context.Context, expr ExpressionNode, asOf *time.Time, sorter *compiledSort) string {
	t.Helper()
	e := &MemQLEngine{db: bun.NewDB(sql.OpenDB(pgdriver.NewConnector()), pgdialect.New())}
	walk := e.latestScanWalk(ctx, expr, asOf, sorter)
	if walk == nil {
		return ""
	}
	return walk.String()
}

// THE WALK IS TAKEN ONLY FOR A READ ITS TERM DECIDES, NEWEST FIRST
// (latest_scan_walk.go). A filter that can reject what the walk finds may need
// the whole history to fill a page, and a read in another order is sorted
// after it anyway; both stay on the lookup. The db-gated
// TestWorkRunReadsDoNotScanTheOwnersHistory measures the walk itself.
func TestLatestScanWalkTakesOnlyReadsItsTermDecides(t *testing.T) {
	const owner = "v1:identity:user:owner"
	run := irConceptEq("v1:work:run")
	ownerTerm := irPayloadCmp("ownerUserId", OpEq, owner)
	clusterOwner := &ComparisonExpression{Field: irActorField("isClusterOwner"), Operator: OpEq, Value: true}
	and := func(l, r ExpressionNode) ExpressionNode { return &LogicalExpression{Op: LogicalAnd, Left: l, Right: r} }
	or := func(l, r ExpressionNode) ExpressionNode { return &LogicalExpression{Op: LogicalOr, Left: l, Right: r} }
	// The owner tier's injected term, as the owner-scoped reads carry it.
	feed := and(and(run, ownerTerm), or(ownerTerm, clusterOwner))
	sorted := func(field string, direction SortDirection) *compiledSort {
		sorter, err := compileSortFields([]SortField{{Field: field, Direction: direction}})
		require.NoError(t, err)
		return sorter
	}
	writer, operator := nonClusterOwnerCtx(owner), clusterOwnerCtx(owner)

	for name, tc := range map[string]struct {
		ctx    context.Context
		expr   ExpressionNode
		sorter *compiledSort
		walks  bool
	}{
		"the owner's feed, for the owner":         {writer, feed, nil, true},
		"the owner's feed, for a cluster owner":   {operator, feed, nil, true},
		"sorted newest first by name":             {writer, feed, sorted("row.createdAt", SortDirectionDesc), true},
		"beside the staged-data gate":             {writer, and(feed, &ComparisonExpression{Field: FieldReference{Raw: "concept", Parts: []string{"concept"}}, Operator: OpNe, Value: "v1:staged:thing"}), nil, true},
		"oldest first":                            {writer, feed, sorted("row.createdAt", SortDirectionAsc), false},
		"by a payload field":                      {writer, feed, sorted("startedAt", SortDirectionDesc), false},
		"a status beside the owner":               {writer, and(feed, irPayloadCmp("status", OpEq, "running")), nil, false},
		"a finer lookup beside the owner":         {writer, and(feed, irPayloadCmp("goalId", OpEq, "v1:work:goal:g")), nil, false},
		"one run by id":                           {writer, and(feed, &ComparisonExpression{Field: FieldReference{Raw: "id", Parts: []string{"id"}}, Operator: OpEq, Value: "v1:work:run:r"}), nil, false},
		"two owners":                              {writer, and(run, irPayloadCmp("ownerUserId", OpIn, []any{owner, "v1:identity:user:other"})), nil, false},
		"under a negation":                        {writer, and(feed, irNot(irPayloadCmp("automationName", OpEq, "x"))), nil, false},
		"a cluster-owner floor the caller fails":  {writer, and(feed, clusterOwner), nil, false},
		"a cluster-owner floor the caller passes": {operator, and(feed, clusterOwner), nil, true},
		"a concept with no walk index":            {writer, and(irConceptEq("v1:work:step"), irPayloadCmp("ownerUserId", OpEq, owner)), nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			walk := latestScanWalkSQL(t, tc.ctx, tc.expr, nil, tc.sorter)
			if !tc.walks {
				require.Empty(t, walk, "this read must stay on the lookup")
				return
			}
			require.Contains(t, walk, `(w.payload->>'ownerUserId') = 'v1:identity:user:owner'`)
			require.Contains(t, walk, `latest_walk.found IS NULL`, "a version is kept only when no newer one exists")
			require.Contains(t, walk, `ORDER BY w."createdAt" DESC, w.id ASC`+"\nOFFSET 0", "the walk keeps the page order behind its fence")
		})
	}

	t.Run("asOf bounds the walk and the newer-version probe", func(t *testing.T) {
		asOf := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
		walk := latestScanWalkSQL(t, writer, feed, &asOf, nil)
		require.Contains(t, walk, `later."createdAt" <= '2026-03-01 00:00:00+00:00'`)
		require.Contains(t, walk, `w."createdAt" <= '2026-03-01 00:00:00+00:00'`)
	})
	t.Run("a cursor starts the walk where the index can begin", func(t *testing.T) {
		at := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
		walk := latestScanWalkSQL(t, contextWithKeyset(writer, keysetPosition{createdAt: at, id: "v1:work:run:r"}), feed, nil, nil)
		require.Contains(t, walk, `AND w."createdAt" <= '2026-03-01 00:00:00+00:00' AND (w."createdAt" < '2026-03-01 00:00:00+00:00' OR w.id > 'v1:work:run:r')`)
	})
}
