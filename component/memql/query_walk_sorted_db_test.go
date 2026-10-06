package memql

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestQueryWalkRefusesAFullPageWithoutCursorSupport(t *testing.T) {
	engine, db, _ := sharedReadMergeEngine(t)
	ctx := ContextWithFreshRead(clusterOwnerCtx("u-sorted-query-walk"))
	suffix := uniqueSuffix("sorted-query-walk")
	owner := "walk:" + suffix
	for n := range 3 {
		seedKeysetRow(t, ctx, db, fmt.Sprintf("%s:%s-%d", keysetConcept, suffix, n),
			time.Date(2026, 6, 22, 12, 0, n, 0, time.UTC), owner)
	}
	query := func(limit int) string {
		return fmt.Sprintf(`sort(paginate(concept==%s&&createdBy==%q, %d), "seq", "asc")`, keysetConcept, owner, limit)
	}
	result, err := engine.Execute(ctx, query(2))
	require.NoError(t, err)
	require.Len(t, MaterializeRows(result), 2)
	require.NotNil(t, result.GetMeta())
	require.True(t, result.GetMeta().HasMore, "a full payload-sorted page is not evidence of exhaustion")
	require.Empty(t, result.GetMeta().Cursor, "payload sorting does not support the createdAt/id keyset")
	visits := 0
	err = WalkQueryPages(ctx, engine.Execute, query(2), 10, func(*ExecuteResult) error { visits++; return nil })
	require.ErrorContains(t, err, "without a cursor")
	require.Zero(t, visits, "do not pass an untraversable truncated set to the consumer")
	err = WalkQueryPages(ctx, engine.Execute, query(4), 10, func(page *ExecuteResult) error {
		visits++
		require.Len(t, MaterializeRows(page), 3)
		return nil
	})
	require.NoError(t, err, "a genuinely short page still proves exhaustion")
	require.Equal(t, 1, visits)
}
