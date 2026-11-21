package calls

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
)

func historyPageFixture(n int) map[string]any {
	started := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC).Add(-time.Duration(n) * time.Minute)
	rowID := fmt.Sprintf("history-%03d", n)
	return map[string]any{"id": rowID, "siteId": "site", "ownerUserId": "owner", "keyId": "key", "orderKey": historyOrder(started, rowID), "role": "host", "status": "ended", "startedAt": started, "endedAt": started.Add(time.Second), "configuredInsights": true, "configuredResources": false, "agentHosted": false}
}

func TestHistoryPaginationRejectsIncompleteOrUnscopedPages(t *testing.T) {
	for _, scenario := range []string{"nil", "missing_meta", "missing_payload", "scalar", "bad_row", "no_cursor", "empty_more", "owner", "site", "key", "archive", "empty_order", "non_advancing", "duplicate_id", "wrong_order", "oversized"} {
		t.Run(scenario, func(t *testing.T) {
			rows := []map[string]any{historyPageFixture(1)}
			var result any = inventoryResult(rows, "", false)
			switch scenario {
			case "nil":
				result = (*memql.ExecuteResult)(nil)
			case "missing_meta":
				result.(*memql.ExecuteResult).Meta = nil
			case "missing_payload":
				result = &memql.ExecuteResult{Meta: &memql.ResultMeta{}}
			case "scalar":
				result = memql.NewResultWithOutput("private malformed response")
			case "bad_row":
				result = memql.NewResultWithOutput([]any{rows[0], false})
			case "no_cursor":
				result = inventoryResult(rows, "", true)
			case "empty_more":
				result = inventoryResult([]map[string]any{}, "next", true)
			case "owner":
				rows[0]["ownerUserId"] = "other-owner"
			case "site":
				rows[0]["siteId"] = "other-site"
			case "key":
				rows[0]["keyId"] = "other-key"
			case "archive":
				rows[0]["archivedAt"] = time.Now()
			case "empty_order":
				rows[0]["orderKey"] = ""
			case "non_advancing":
				rows[0]["orderKey"] = historyPageFixture(0)["orderKey"]
			case "duplicate_id":
				second := historyPageFixture(2)
				second["id"] = rows[0]["id"]
				result = inventoryResult(append(rows, second), "", false)
			case "wrong_order":
				result = inventoryResult(append(rows, historyPageFixture(0)), "", false)
			case "oversized":
				for i := 2; i <= 52; i++ {
					rows = append(rows, historyPageFixture(i))
				}
				result = inventoryResult(rows, "", false)
			}
			store := &Store{engine: inventoryEngineFunc(func(context.Context, string) (any, error) { return result, nil })}
			rowsOut, more, err := store.historyForKey(context.Background(), "owner", "site", "key", rowString(historyPageFixture(0), "orderKey"), false)
			require.Error(t, err)
			require.Nil(t, rowsOut)
			require.False(t, more)
		})
	}
}

func TestHistoryPaginationKeepsNativeContinuationAndCancellation(t *testing.T) {
	ctx := memql.ContextWithCursor(context.Background(), "unrelated incoming cursor")
	store := &Store{engine: inventoryEngineFunc(func(ctx context.Context, _ string) (any, error) {
		require.Empty(t, memql.CursorFromContext(ctx), "history uses its authenticated orderKey, never an incoming native cursor")
		return inventoryResult([]map[string]any{historyPageFixture(1)}, "native-continuation", true), nil
	})}
	rows, more, err := store.historyForKey(ctx, "owner", "site", "key", "", false)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.True(t, more)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store.engine = inventoryEngineFunc(func(context.Context, string) (any, error) {
		cancel()
		return inventoryResult([]map[string]any{}, "", false), nil
	})
	rows, _, err = store.historyForKey(ctx, "owner", "site", "key", "", false)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, rows)
	store.engine = inventoryEngineFunc(func(context.Context, string) (any, error) { return nil, errors.New("private fixture content") })
	_, _, err = store.historyForKey(context.Background(), "owner", "site", "key", "", false)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "private fixture content")
}

func TestHistoryPaginationOverviewIsLimitedAtReadBudget(t *testing.T) {
	reads := 0
	store := &Store{engine: inventoryEngineFunc(func(context.Context, string) (any, error) {
		reads++
		return inventoryResult([]map[string]any{historyPageFixture(reads)}, fmt.Sprint(reads), true), nil
	})}
	svc := &Service{store: store, now: func() time.Time { return time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC) }}
	h := keyHolder{site: callrules.SiteRef{SiteID: "site", OwnerUserID: "owner"}, row: &sessionRow{KeyID: "key"}}
	out, err := svc.historyOverview(context.Background(), h, 7)
	require.NoError(t, err)
	require.Equal(t, 20, reads)
	require.Equal(t, "limited", out.Usage.Status)
	require.Nil(t, out.HostedCalls)
	require.Nil(t, out.Usage.InsightsCalls)
	require.Empty(t, out.Series)
}
